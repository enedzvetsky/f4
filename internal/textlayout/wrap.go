package textlayout

import (
	"math"
	"sort"
	"time"

	"github.com/unxed/f4/internal/piecetable"
	"github.com/unxed/vtui"
)

// LineFragment описывает один визуальный кусок логической строки после свертки.
type LineFragment struct {
	LogicalLineIdx  int // Номер оригинальной строки (до \n)
	ByteOffsetStart int // Смещение начала фрагмента (от начала всего файла/буфера)
	ByteOffsetEnd   int // Смещение конца фрагмента
	VisualWidth     int // Ширина фрагмента в колонках терминала (учитывая CJK)
}

// WrapEngine отвечает за вычисление визуальной разметки текста.
type WrapEngine struct {
	Pt            *piecetable.PieceTable
	Li            *piecetable.LineIndex
	wrapWidth     int
	wordWrap      bool
	fragmentCache [][]LineFragment
	tabSize       int

	// rowOffsets[i] хранит общее количество визуальных строк во всех
	// логических строках ПЕРЕД строкой i.
	rowOffsets []int
	totalRows  int
	validUntil int // Index of the last logical line with a valid calculated row offset

	tmpBuf []byte // Reusable buffer for avoiding allocations

	// noWrapCache is deliberately a map rather than fragmentCache: edits in a
	// huge file must not walk or allocate a slice for every logical line.
	noWrapCache map[int]noWrapLayout
	// noWrapCached counts the cluster entries held across noWrapCache so the
	// cache cannot grow with the number of lines the user has scrolled past.
	noWrapCached int
}

func NewWrapEngine(Pt *piecetable.PieceTable, Li *piecetable.LineIndex) *WrapEngine {
	return &WrapEngine{
		Pt:            Pt,
		Li:            Li,
		wrapWidth:     80,
		wordWrap:      true,
		fragmentCache: nil,
		tabSize:       8,
	}
}

type visualCluster struct {
	text       string
	width      int
	byteStart  int
	byteEnd    int
	logicalPos int
	logicalEnd int
}

// noWrapLayout caches what a cursor-column lookup needs for one unwrapped
// logical line, which otherwise rebuilds the complete long line on every key
// press. Only the cluster ends and their prefix widths are kept: retaining the
// line text and the clusters themselves cost roughly seventy times the size of
// the text, so scrolling through a file was enough to exhaust memory.
type noWrapLayout struct {
	fragments    []LineFragment
	clusterEnds  []int
	prefixWidths []int
	hasRTL       bool
}

// noWrapCacheBudget caps the cluster entries kept across all cached lines. At
// eight bytes per entry this holds the cache to about 8 MB; going over drops it
// wholesale, which costs one rescan of the lines still on screen.
const noWrapCacheBudget = 1 << 20

// maxLaidOutLineBytes is how much of one logical line is laid out. It matches
// the editor's own long-line limit -- the highlighter cuts a line at 64 KB and
// word wrap refuses a line longer than that -- so a line past it is already
// one the editor only partly describes. An offset beyond the cap snaps to the
// end of the laid-out part, as LogicalToVisual already does for capped lines.
const maxLaidOutLineBytes = 64 * 1024

const (
	// rowCountSliceBudget is the longest one GetTotalVisualRows call may
	// spend laying lines out. It is the stall the user can feel, so it is
	// kept well inside one frame; how many lines fit into it is whatever the
	// document's lines cost.
	rowCountSliceBudget = 4 * time.Millisecond
	// rowCountClockStride is how many lines pass between two clock readings
	// inside a slice.
	rowCountClockStride = 64
)

// logicalTextClusters keeps zoin-bot's grapheme boundaries in document order.
func logicalTextClusters(text string) []visualCluster {
	base := VisualClusters(text)
	logical := make([]visualCluster, 0, len(base))
	for _, cluster := range base {
		logical = append(logical, visualCluster{
			text:       cluster.Text,
			width:      cluster.Width,
			byteStart:  cluster.Start,
			byteEnd:    cluster.End,
			logicalPos: cluster.RuneStart,
			logicalEnd: cluster.RuneEnd,
		})
	}
	return logical
}

// layoutLine runs the bidi algorithm (UAX #9) over the editor's own cluster
// boundaries: the virama joined clusters of VisualClusters, not the UAX #29
// ones vtui's string helpers would segment on their own, so that wrapping,
// painting, hit testing and caret movement all agree on one set of units.
func layoutLine(text string, logical []visualCluster) vtui.BidiLayout {
	spans := make([]vtui.ClusterSpan, len(logical))
	for i, cluster := range logical {
		spans[i] = vtui.ClusterSpan{Start: cluster.byteStart, End: cluster.byteEnd}
	}
	return vtui.LayoutBidi(text, spans, vtui.DefaultBidiParagraph)
}

// visualClusters returns the clusters of text in the order they are drawn,
// with mirrored glyphs substituted where a cluster reads right to left.
func visualClusters(text string) []visualCluster {
	logical := logicalTextClusters(text)
	if vtui.DefaultBidiMode != vtui.BidiFull || !vtui.HasRTL(text) {
		return logical
	}
	layout := layoutLine(text, logical)
	visual := make([]visualCluster, 0, len(logical))
	for _, index := range layout.VisualToLogical {
		cluster := logical[index]
		cluster.text = layout.Text(index, cluster.text)
		visual = append(visual, cluster)
	}
	return visual
}

// VisualClustersInVisualOrder returns grapheme clusters in terminal order.
// zoin-bot uses this shared mapping for editor painting and hit testing.
func VisualClustersInVisualOrder(text string) []VisualCluster {
	clusters := visualClusters(text)
	result := make([]VisualCluster, 0, len(clusters))
	for _, cluster := range clusters {
		result = append(result, VisualCluster{
			Text:      cluster.text,
			Width:     cluster.width,
			Start:     cluster.byteStart,
			End:       cluster.byteEnd,
			RuneStart: cluster.logicalPos,
			RuneEnd:   cluster.logicalEnd,
		})
	}
	return result
}

// visualCaretMap is the caret equivalent of visualClusters, over the same
// cluster boundaries: LogicalToVisual[b] is the visual boundary the caret is
// drawn at when it stands at logical boundary b (between clusters b-1 and
// b), VisualToLogical[v] the logical boundary a click at visual boundary v
// selects. The placement rule is vtui.BidiLayout.CaretVisual: the caret
// stands at the trailing edge of the cluster it follows, so inside a right to
// left word it walks leftwards while the logical position advances, as in
// Notepad and the Windows edit controls.
type visualCaretMap struct {
	VisualToLogical []int
	LogicalToVisual []int
}

func buildVisualCaretMap(text string) visualCaretMap {
	logical := logicalTextClusters(text)
	n := len(logical)
	visualToLogical := make([]int, n+1)
	logicalToVisual := make([]int, n+1)
	if vtui.DefaultBidiMode != vtui.BidiFull || !vtui.HasRTL(text) || n == 0 {
		for i := 0; i <= n; i++ {
			visualToLogical[i] = i
			logicalToVisual[i] = i
		}
		return visualCaretMap{VisualToLogical: visualToLogical, LogicalToVisual: logicalToVisual}
	}
	layout := layoutLine(text, logical)
	for i := 0; i <= n; i++ {
		logicalToVisual[i] = layout.CaretVisual(i)
		visualToLogical[i] = layout.CaretLogical(i)
	}
	return visualCaretMap{VisualToLogical: visualToLogical, LogicalToVisual: logicalToVisual}
}

func logicalClusterIndexAtByte(clusters []visualCluster, byteOffset int) int {
	for index, cluster := range clusters {
		if byteOffset <= cluster.byteStart || byteOffset < cluster.byteEnd {
			return index
		}
	}
	return len(clusters)
}

func visualClusterWidths(clusters []visualCluster, tabSize int) []int {
	if tabSize <= 0 {
		tabSize = 8
	}
	widths := make([]int, len(clusters))
	column := 0
	for i, cluster := range clusters {
		width := cluster.width
		if cluster.text == "\t" {
			width = tabSize - (column % tabSize)
		}
		if width <= 0 {
			width = 1
		}
		widths[i] = width
		column += width
	}
	return widths
}

func fragmentLogicalToVisual(text string, byteOffset, tabSize int) int {
	if byteOffset < 0 {
		byteOffset = 0
	}
	if byteOffset > len(text) {
		byteOffset = len(text)
	}
	clusters := visualClusters(text)
	visualPos := 0
	if vtui.DefaultBidiMode == vtui.BidiFull && vtui.HasRTL(text) {
		logicalIndex := logicalClusterIndexAtByte(logicalTextClusters(text), byteOffset)
		caret := buildVisualCaretMap(text)
		if logicalIndex < len(caret.LogicalToVisual) {
			visualPos = caret.LogicalToVisual[logicalIndex]
		}
	} else {
		for _, cluster := range clusters {
			if byteOffset < cluster.byteEnd {
				break
			}
			visualPos++
		}
	}
	if visualPos > len(clusters) {
		visualPos = len(clusters)
	}
	widths := visualClusterWidths(clusters, tabSize)
	width := 0
	for _, clusterWidth := range widths[:visualPos] {
		width += clusterWidth
	}
	return width
}

func fragmentVisualToLogical(text string, visualCol, tabSize int) int {
	clusters := visualClusters(text)
	widths := visualClusterWidths(clusters, tabSize)
	visualPos := 0
	if visualCol > 0 {
		width := 0
		for visualPos < len(clusters) && width+widths[visualPos] <= visualCol {
			width += widths[visualPos]
			visualPos++
		}
	}
	if vtui.DefaultBidiMode == vtui.BidiFull && vtui.HasRTL(text) {
		logical := logicalTextClusters(text)
		caret := buildVisualCaretMap(text)
		logicalIndex := len(logical)
		if visualPos < len(caret.VisualToLogical) {
			logicalIndex = caret.VisualToLogical[visualPos]
		}
		if logicalIndex < len(logical) {
			return logical[logicalIndex].byteStart
		}
		return len(text)
	} else if visualPos < len(clusters) {
		return clusters[visualPos].byteStart
	} else {
		return len(text)
	}
}

func fragmentVisualMove(text string, byteOffset, direction int) (int, bool) {
	clusters := visualClusters(text)
	if len(clusters) == 0 {
		return byteOffset, false
	}
	visualPos := 0
	if vtui.DefaultBidiMode == vtui.BidiFull && vtui.HasRTL(text) {
		logicalIndex := logicalClusterIndexAtByte(logicalTextClusters(text), byteOffset)
		caret := buildVisualCaretMap(text)
		if logicalIndex < len(caret.LogicalToVisual) {
			visualPos = caret.LogicalToVisual[logicalIndex]
		}
	} else {
		for _, cluster := range clusters {
			if byteOffset < cluster.byteEnd {
				break
			}
			visualPos++
		}
	}
	target := visualPos + direction
	if target < 0 || target > len(clusters) {
		return byteOffset, false
	}
	if vtui.DefaultBidiMode == vtui.BidiFull && vtui.HasRTL(text) {
		logical := logicalTextClusters(text)
		caret := buildVisualCaretMap(text)
		logicalIndex := len(logical)
		if target < len(caret.VisualToLogical) {
			logicalIndex = caret.VisualToLogical[target]
		}
		if logicalIndex < len(logical) {
			return logical[logicalIndex].byteStart, true
		}
		return len(text), true
	} else if target < len(clusters) {
		return clusters[target].byteStart, true
	} else {
		return len(text), true
	}
}

// MoveVisual moves one grapheme cluster in the direction shown on screen.
// It also crosses wrapped rows, which lets the editor use one navigation rule
// for LTR, RTL, combining, and wide text.
func (we *WrapEngine) MoveVisual(byteOffset, direction int) int {
	if direction == 0 {
		return byteOffset
	}
	visualRow, _ := we.LogicalToVisual(byteOffset)
	logLineIdx, fragIdx := we.GetLogLineAtVisualRow(visualRow)
	fragments := we.GetFragments(logLineIdx)
	if fragIdx < 0 || fragIdx >= len(fragments) {
		return byteOffset
	}
	frag := fragments[fragIdx]
	we.tmpBuf = we.tmpBuf[:0]
	we.tmpBuf, _ = we.Pt.AppendRange(we.tmpBuf, frag.ByteOffsetStart, frag.ByteOffsetEnd-frag.ByteOffsetStart)
	rel := byteOffset - frag.ByteOffsetStart
	if rel < 0 {
		rel = 0
	}
	if rel > len(we.tmpBuf) {
		rel = len(we.tmpBuf)
	}
	if moved, ok := fragmentVisualMove(string(we.tmpBuf), rel, direction); ok && moved != rel {
		return frag.ByteOffsetStart + moved
	}
	if direction < 0 && visualRow > 0 {
		return we.VisualToLogical(visualRow-1, int(^uint(0)>>1))
	}
	if direction > 0 && visualRow+1 < we.GetTotalVisualRows() {
		return we.VisualToLogical(visualRow+1, 0)
	}
	return byteOffset
}

func (we *WrapEngine) SetTabSize(size int) {
	if size <= 0 {
		size = 8
	}
	if we.tabSize != size {
		we.tabSize = size
		we.InvalidateCache()
	}
}

func (we *WrapEngine) SetPointers(Pt *piecetable.PieceTable, Li *piecetable.LineIndex) {
	we.Pt = Pt
	we.Li = Li
	we.InvalidateCache()
}

// SetWidth устанавливает ширину для свертки. При изменении сбрасывает кэш.
func (we *WrapEngine) SetWidth(width int) {
	if width < 1 {
		width = 1
	} // Ширина не может быть меньше 1
	if width != we.wrapWidth {
		we.wrapWidth = width
		we.InvalidateCache()
	}
}

// ToggleWrap включает/выключает перенос по словам.
func (we *WrapEngine) ToggleWrap(wrap bool) {
	if wrap != we.wordWrap {
		we.wordWrap = wrap
		we.InvalidateCache()
	}
}

// InvalidateCache сбрасывает кэш фрагментов.
func (we *WrapEngine) InvalidateCache() {
	we.fragmentCache = nil
	we.validUntil = -1
	we.rowOffsets = nil
	we.totalRows = 0
	we.noWrapCache = nil
	we.noWrapCached = 0
}

func (we *WrapEngine) InvalidateFrom(logLineIdx int) {
	if logLineIdx < 0 {
		logLineIdx = 0
	}
	if we.fragmentCache != nil && logLineIdx < len(we.fragmentCache) {
		for i := logLineIdx; i < len(we.fragmentCache); i++ {
			we.fragmentCache[i] = nil
		}
	}
	if logLineIdx <= we.validUntil {
		we.validUntil = logLineIdx - 1
	}
	for idx, cached := range we.noWrapCache {
		if idx >= logLineIdx {
			we.noWrapCached -= len(cached.clusterEnds)
			delete(we.noWrapCache, idx)
		}
	}
}

// GetFragments возвращает визуальные фрагменты для одной логической строки.
func (we *WrapEngine) GetFragments(logLineIdx int) []LineFragment {
	lineCount := we.Li.LineCount()
	if logLineIdx < 0 || logLineIdx >= lineCount {
		return nil
	}

	if we.wordWrap {
		if we.fragmentCache != nil && logLineIdx < len(we.fragmentCache) && we.fragmentCache[logLineIdx] != nil {
			return we.fragmentCache[logLineIdx]
		}
	} else if cached, ok := we.noWrapCache[logLineIdx]; ok {
		return cached.fragments
	}

	startOffset := we.Li.GetLineOffset(logLineIdx)
	endOffset := we.Pt.Size()
	if logLineIdx+1 < we.Li.LineCount() {
		endOffset = we.Li.GetLineOffset(logLineIdx + 1)
	}
	// The cap covers every line, not only the unindexed tail it was first
	// written for. The layout below builds a cluster for every grapheme of
	// the line, and a log with a dumped payload on one line makes that
	// several million of them -- more than a second inside the frame that
	// first scrolled the line into view, on a file that is otherwise smooth.
	// Only a screenful of columns is ever drawn, and the rest of the editor
	// has stopped at this same length all along: the highlighter cuts a line
	// here, and word wrap turns itself off past it.
	if endOffset-startOffset > maxLaidOutLineBytes {
		endOffset = startOffset + maxLaidOutLineBytes
	}

	we.tmpBuf = we.tmpBuf[:0]
	var err error
	we.tmpBuf, err = we.Pt.AppendRange(we.tmpBuf, startOffset, endOffset-startOffset)

	// If data is not ready, return a dummy visual fragment
	if err == piecetable.ErrLoading {
		frag := LineFragment{
			LogicalLineIdx:  logLineIdx,
			ByteOffsetStart: startOffset,
			ByteOffsetEnd:   endOffset,
			VisualWidth:     16, // Width of "[ Loading... ]"
		}
		return []LineFragment{frag} // DO NOT CACHE LOADING STUBS
	}

	lineData := we.tmpBuf
	truncated := false

	// Убираем \n или \r\n с конца
	if len(lineData) > 0 && lineData[len(lineData)-1] == '\n' {
		lineData = lineData[:len(lineData)-1]
		if len(lineData) > 0 && lineData[len(lineData)-1] == '\r' {
			lineData = lineData[:len(lineData)-1]
		}
	}

	// PREVENT LONG-LINE FLASH:
	// If lineData STILL contains '\n' in the middle (because LineIndex hasn't indexed it yet),
	// truncate it to the first '\n' to prevent rendering multiple lines as one continuous paragraph.
	for i, b := range lineData {
		if b == '\n' {
			lineData = lineData[:i]
			if i > 0 && lineData[i-1] == '\r' {
				lineData = lineData[:i-1]
			}
			truncated = true
			break
		}
	}

	if !we.wordWrap || we.wrapWidth <= 0 {
		text := string(lineData)
		clusters := visualClusters(text)
		clusterEnds := make([]int, len(clusters))
		prefixWidths := make([]int, len(clusters)+1)
		for i, cluster := range clusters {
			width := cluster.width
			if cluster.text == "\t" {
				width = we.tabSize - (int(prefixWidths[i]) % we.tabSize)
			}
			if width <= 0 {
				width = 1
			}
			clusterEnds[i] = cluster.byteEnd
			prefixWidths[i+1] = prefixWidths[i] + width
		}
		frag := LineFragment{
			LogicalLineIdx:  logLineIdx,
			ByteOffsetStart: startOffset,
			ByteOffsetEnd:   startOffset + len(lineData),
			VisualWidth:     prefixWidths[len(prefixWidths)-1],
		}
		fragments := []LineFragment{frag}
		if !truncated && len(text) <= math.MaxInt32 {
			if we.noWrapCache == nil {
				we.noWrapCache = make(map[int]noWrapLayout)
			}
			if previous, ok := we.noWrapCache[logLineIdx]; ok {
				we.noWrapCached -= len(previous.clusterEnds)
			} else if we.noWrapCached+len(clusterEnds) > noWrapCacheBudget {
				we.noWrapCache = make(map[int]noWrapLayout)
				we.noWrapCached = 0
			}
			we.noWrapCache[logLineIdx] = noWrapLayout{
				fragments:    fragments,
				clusterEnds:  clusterEnds,
				prefixWidths: prefixWidths,
				hasRTL:       vtui.HasRTL(text),
			}
			we.noWrapCached += len(clusterEnds)
		}
		return fragments
	}

	if we.fragmentCache == nil || len(we.fragmentCache) != lineCount {
		we.fragmentCache = make([][]LineFragment, lineCount)
	}

	var fragments []LineFragment
	bytePos := 0
	dataLen := len(lineData)
	logicalClusters := logicalTextClusters(string(lineData))
	clusterPos := 0

	cumulativeVisualWidth := 0
	for bytePos < dataLen && clusterPos < len(logicalClusters) {
		visualWidth := 0
		fragStartByte := bytePos
		lastSpaceEnd := -1
		lastSpaceWidth := 0
		lastSpaceClusterPos := -1

		scanPos := bytePos
		scanClusterPos := clusterPos
		for scanClusterPos < len(logicalClusters) {
			cluster := logicalClusters[scanClusterPos]
			clusterText := cluster.text
			w := cluster.width
			if clusterText == "\t" {
				w = we.tabSize - ((cumulativeVisualWidth + visualWidth) % we.tabSize)
			}
			if w <= 0 {
				w = 1
			}

			if visualWidth+w > we.wrapWidth {
				if clusterText == " " {
					// Пробел не влезает, но мы его забираем в конец этой строки
					scanPos = cluster.byteEnd
					scanClusterPos++
					visualWidth += w
				} else if lastSpaceEnd != -1 {
					// Word Wrap: откатываемся к последнему пробелу
					scanPos = lastSpaceEnd
					scanClusterPos = lastSpaceClusterPos
					visualWidth = lastSpaceWidth
				} else if scanPos == fragStartByte {
					// Даже один символ не влез (CJK в узком окне) - поглощаем его
					scanPos = cluster.byteEnd
					scanClusterPos++
					visualWidth = w
				}
				break
			}

			visualWidth += w
			scanPos = cluster.byteEnd
			scanClusterPos++
			if clusterText == " " {
				lastSpaceEnd = scanPos
				lastSpaceWidth = visualWidth
				lastSpaceClusterPos = scanClusterPos
			}
		}

		fragments = append(fragments, LineFragment{
			LogicalLineIdx:  logLineIdx,
			ByteOffsetStart: startOffset + fragStartByte,
			ByteOffsetEnd:   startOffset + scanPos,
			VisualWidth:     visualWidth,
		})
		cumulativeVisualWidth += visualWidth
		bytePos = scanPos
		clusterPos = scanClusterPos
	}

	if len(fragments) == 0 {
		fragments = append(fragments, LineFragment{LogicalLineIdx: logLineIdx, ByteOffsetStart: startOffset, ByteOffsetEnd: startOffset})
	}

	if !truncated && logLineIdx < len(we.fragmentCache) {
		we.fragmentCache[logLineIdx] = fragments
	}
	return fragments
}

// ensureRowCountCache lays out every logical line up to until, however long
// that takes. A caller asking where one particular line starts needs the exact
// answer, so this one does not stop early.
func (we *WrapEngine) ensureRowCountCache(until int) {
	we.fillRowCountCache(until, 0)
}

// fillRowCountCache is ensureRowCountCache with an optional wall-clock budget.
// With one it lays out as far as it gets and leaves the rest to a later call,
// which only the document-wide total may settle for -- see GetTotalVisualRows.
func (we *WrapEngine) fillRowCountCache(until int, budget time.Duration) {
	if !we.wordWrap {
		return
	}
	lineCount := we.Li.LineCount()
	if until >= lineCount {
		until = lineCount - 1
	}
	if we.validUntil >= until && we.rowOffsets != nil && len(we.rowOffsets) == lineCount {
		return
	}

	if we.rowOffsets == nil || len(we.rowOffsets) != lineCount {
		oldOffsets := we.rowOffsets
		we.rowOffsets = make([]int, lineCount)
		if oldOffsets != nil {
			numToCopy := len(oldOffsets)
			if numToCopy > lineCount {
				numToCopy = lineCount
			}
			copy(we.rowOffsets, oldOffsets[:numToCopy])
			if we.validUntil >= numToCopy {
				we.validUntil = numToCopy - 1
			}
		} else {
			we.validUntil = -1
		}
	}

	currentOffset := 0
	start := we.validUntil + 1
	if start > 0 {
		currentOffset = we.rowOffsets[start-1] + len(we.GetFragments(start-1))
	}

	var deadline time.Time
	if budget > 0 {
		deadline = time.Now().Add(budget)
	}
	reached := we.validUntil
	for i := start; i <= until; i++ {
		we.rowOffsets[i] = currentOffset
		currentOffset += len(we.GetFragments(i))
		reached = i
		// The clock is read every rowCountClockStride lines rather than every
		// line: laying one line out is microseconds, and time.Now next to it
		// would be a measurable share of the work.
		if budget > 0 && i%rowCountClockStride == 0 && time.Now().After(deadline) {
			break
		}
	}
	if reached > we.validUntil {
		we.validUntil = reached
	}
	if we.validUntil == lineCount-1 {
		we.totalRows = currentOffset
	}
}

// RowCountComplete reports whether rowOffsets already describes every line, so
// that GetTotalVisualRows is exact rather than an estimate. Without wrapping
// there is nothing to lay out and the answer is always yes.
func (we *WrapEngine) RowCountComplete() bool {
	if !we.wordWrap {
		return true
	}
	lineCount := we.Li.LineCount()
	return we.validUntil == lineCount-1 && len(we.rowOffsets) == lineCount
}

// estimatedTotalRows stands in for the document's visual height while the
// row-count cache is still catching up: the rows already laid out, plus one
// for every line that has not been. A line never takes fewer than one row, so
// this is short of the truth and never past it -- callers clamp a scroll
// position against this number, and one that ran past the end would send the
// lazy row lookups it feeds back into the whole-document layout the budget
// exists to avoid.
func (we *WrapEngine) estimatedTotalRows() int {
	lineCount := we.Li.LineCount()
	if we.validUntil < 0 || we.validUntil >= lineCount {
		return lineCount
	}
	laidOut := we.rowOffsets[we.validUntil] + len(we.GetFragments(we.validUntil))
	return laidOut + (lineCount - 1 - we.validUntil)
}

// GetTotalVisualRows возвращает общее количество визуальных строк в документе.
//
// Wrapping makes that number cost a layout of the whole document, and the
// editor asks for it on every frame -- for the scroll bar, and to clamp PgDn.
// On a large file the first frame after the background line index finishes
// therefore used to stall for a second or more, in the middle of a scroll.
// So the layout is done a slice at a time and the tail is estimated until it
// catches up: the estimate is never above the truth, and
// RowCountComplete says when it has stopped being one.
func (we *WrapEngine) GetTotalVisualRows() int {
	if !we.wordWrap {
		return we.Li.LineCount()
	}
	we.fillRowCountCache(we.Li.LineCount()-1, rowCountSliceBudget)
	if we.RowCountComplete() {
		return we.totalRows
	}
	return we.estimatedTotalRows()
}

// AdvanceRowCount lays out one more slice of the row-count cache and reports
// whether it is now complete. A caller drawing a wrapped document calls it
// once a frame, so the document's height settles over a few frames without any
// one of them paying for all of it. It always makes progress while there is
// any left, so a caller may keep asking for frames until it answers true.
func (we *WrapEngine) AdvanceRowCount() bool {
	if we.RowCountComplete() {
		return true
	}
	we.fillRowCountCache(we.Li.LineCount()-1, rowCountSliceBudget)
	return we.RowCountComplete()
}

// GetRowOffset возвращает индекс первой визуальной строки для данной логической строки.
func (we *WrapEngine) GetRowOffset(logLineIdx int) int {
	if !we.wordWrap {
		if logLineIdx < 0 {
			return 0
		}
		lineCount := we.Li.LineCount()
		if logLineIdx >= lineCount {
			return lineCount
		}
		return logLineIdx
	}
	we.ensureRowCountCache(logLineIdx)
	if logLineIdx < 0 {
		return 0
	}
	if logLineIdx >= len(we.rowOffsets) {
		we.ensureRowCountCache(we.Li.LineCount() - 1)
		return we.totalRows
	}
	return we.rowOffsets[logLineIdx]
}

// GetLogLineAtVisualRow переводит абсолютный индекс визуальной строки в индекс
// логической строки и порядковый номер фрагмента внутри неё.
func (we *WrapEngine) GetLogLineAtVisualRow(visualRow int) (logLineIdx int, fragIdx int) {
	if visualRow < 0 {
		return 0, 0
	}
	if !we.wordWrap {
		lineCount := we.Li.LineCount()
		if visualRow >= lineCount {
			if lineCount <= 0 {
				return 0, 0
			}
			return lineCount - 1, 0
		}
		return visualRow, 0
	}

	// Lazy calculation until we find the row or hit EOF
	lineCount := we.Li.LineCount()
	for we.validUntil < lineCount-1 {
		var lastCalculatedRow int
		if we.validUntil >= 0 {
			lastCalculatedRow = we.rowOffsets[we.validUntil] + len(we.GetFragments(we.validUntil))
		}
		if lastCalculatedRow > visualRow {
			break
		}
		// Expand cache in chunks
		nextTarget := we.validUntil + 500
		if nextTarget >= lineCount {
			nextTarget = lineCount - 1
		}
		we.ensureRowCountCache(nextTarget)
	}

	if visualRow >= we.totalRows && we.validUntil == lineCount-1 {
		return lineCount - 1, 0
	}

	// Binary search on the valid portion of the cache
	logLineIdx = sort.Search(we.validUntil+1, func(i int) bool {
		return we.rowOffsets[i] > visualRow
	}) - 1

	if logLineIdx < 0 {
		logLineIdx = 0
	}
	fragIdx = visualRow - we.rowOffsets[logLineIdx]
	return
}

// LogicalToVisual переводит байтовый оффсет в документе в (строка, колонка) на экране.
func (we *WrapEngine) LogicalToVisual(byteOffset int) (visualRow, visualCol int) {
	if byteOffset < 0 {
		byteOffset = 0
	}
	logLineIdx := we.Li.GetLineAtOffset(byteOffset)
	totalRow := logLineIdx
	if we.wordWrap {
		we.ensureRowCountCache(logLineIdx)
		totalRow = we.rowOffsets[logLineIdx]
	}
	fragments := we.GetFragments(logLineIdx)

	if len(fragments) > 0 {
		lastFrag := fragments[len(fragments)-1]
		if byteOffset > lastFrag.ByteOffsetEnd {
			// Safety for capped binary lines: if offset is beyond indexed fragments,
			// snap to the end of the last visible fragment.
			byteOffset = lastFrag.ByteOffsetEnd
		}
	}
	if !we.wordWrap {
		if cached, ok := we.noWrapCache[logLineIdx]; ok && !cached.hasRTL {
			frag := cached.fragments[0]
			relative := byteOffset - frag.ByteOffsetStart
			if relative < 0 {
				relative = 0
			} else if relative > frag.ByteOffsetEnd-frag.ByteOffsetStart {
				relative = frag.ByteOffsetEnd - frag.ByteOffsetStart
			}
			clusterIndex := sort.Search(len(cached.clusterEnds), func(i int) bool {
				return relative < cached.clusterEnds[i]
			})
			return totalRow, cached.prefixWidths[clusterIndex]
		}
	}

	for i, frag := range fragments {
		isLastFragOfLine := (i == len(fragments)-1)
		if byteOffset >= frag.ByteOffsetStart && (byteOffset < frag.ByteOffsetEnd || (isLastFragOfLine && byteOffset == frag.ByteOffsetEnd)) {
			we.tmpBuf = we.tmpBuf[:0]
			we.tmpBuf, _ = we.Pt.AppendRange(we.tmpBuf, frag.ByteOffsetStart, frag.ByteOffsetEnd-frag.ByteOffsetStart)
			return totalRow + i, fragmentLogicalToVisual(string(we.tmpBuf), byteOffset-frag.ByteOffsetStart, we.tabSize)
		}
	}
	return totalRow, 0
}

// VisualToLogical переводит (строка, колонка) на экране в байтовый оффсет документа.
func (we *WrapEngine) VisualToLogical(visualRow, visualCol int) int {
	if visualRow < 0 {
		return 0
	}
	logLineIdx, fragIdx := we.GetLogLineAtVisualRow(visualRow)
	fragments := we.GetFragments(logLineIdx)
	if fragments == nil {
		vtui.DebugLog("DEBUG_V2L_FAIL: No fragments for LogLine %d", logLineIdx)
		return 0
	}
	if fragIdx >= len(fragments) {
		fragIdx = len(fragments) - 1
	}
	frag := fragments[fragIdx]

	vtui.DebugLog("DEBUG_V2L_START: Row:%d Col:%d -> LogLine:%d Frag:%d StartOff:%d EndOff:%d", visualRow, visualCol, logLineIdx, fragIdx, frag.ByteOffsetStart, frag.ByteOffsetEnd)

	if frag.ByteOffsetStart >= frag.ByteOffsetEnd {
		return frag.ByteOffsetStart
	}

	we.tmpBuf = we.tmpBuf[:0]
	we.tmpBuf, _ = we.Pt.AppendRange(we.tmpBuf, frag.ByteOffsetStart, frag.ByteOffsetEnd-frag.ByteOffsetStart)
	return frag.ByteOffsetStart + fragmentVisualToLogical(string(we.tmpBuf), visualCol, we.tabSize)
}
