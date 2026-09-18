package editor

import (
	"strings"
	"testing"
	"time"

	"github.com/unxed/vtui"
)

// With wrapping off a fragment is the whole logical line, and a log line of
// tens of kilobytes was segmented into grapheme clusters and turned into cells
// in full on every frame it was on screen — for a viewport that shows a couple
// of hundred columns of it. That was the stutter of scrolling a log.
func TestFillCells_StopsAtTheRightEdgeOfTheViewport(t *testing.T) {
	ev := &EditorView{TabSize: 8}
	const width = 200
	line := []byte(strings.Repeat("x", 1<<20))

	start := time.Now()
	cells := ev.fillCellsWithLinks(nil, line, 0, 0, 0, false, 0, 0, nil, nil, 0, width, false, -1, 0, 0, 0)
	elapsed := time.Since(start)

	if len(cells) != width {
		t.Errorf("built %d cells for a %d column viewport", len(cells), width)
	}
	// Building the megabyte took hundreds of milliseconds; a viewport's worth
	// is microseconds. The bound is loose enough for a loaded machine and
	// still nowhere near the whole line.
	if elapsed > 50*time.Millisecond {
		t.Errorf("building one row of a 1 MB line took %v", elapsed)
	}

	// Without a limit the whole fragment is still built, which is what the
	// callers that hand over a line already cut to size rely on.
	if got := ev.fillCells(nil, []byte("abcde"), 0, 0, 0, false, 0, 0, nil, 0, false, -1, 0, 0, 0); len(got) != 5 {
		t.Errorf("unlimited fill built %d cells, want 5", len(got))
	}
}

func TestEditorRenderClip(t *testing.T) {
	long := strings.Repeat("x", 50000)

	if got := editorRenderClip(long, 200); len(got) >= len(long) {
		t.Errorf("a 50 KB line was not clipped for a 200 column viewport: %d bytes", len(got))
	} else if got != long[:len(got)] {
		t.Error("the clip is not a prefix of the line")
	} else if cols := clusterColumnsForTest(got); cols < 200 {
		t.Errorf("the clip covers %d columns, short of the 200 asked for", cols)
	}

	// Nothing to gain, and nothing may be lost.
	if got := editorRenderClip("short line", 200); got != "short line" {
		t.Errorf("a short line was clipped to %q", got)
	}
	if got := editorRenderClip(long, 0); got != long {
		t.Error("an unlimited request clipped the line")
	}

	// A line laid out by the bidi algorithm is reordered as a whole: the
	// reordering of a prefix is not the prefix of the reordering, so it is
	// left alone however long it is.
	oldMode := vtui.DefaultBidiMode
	vtui.DefaultBidiMode = vtui.BidiFull
	defer func() { vtui.DefaultBidiMode = oldMode }()
	rtl := strings.Repeat("שלום ", 10000)
	if got := editorRenderClip(rtl, 200); got != rtl {
		t.Errorf("a right-to-left line was clipped to %d of %d bytes", len(got), len(rtl))
	}
}

func clusterColumnsForTest(s string) int {
	cols := 0
	vtui.ForEachCluster(s, func(_ string, width, _ int) { cols += width })
	return cols
}
