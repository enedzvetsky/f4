package archive

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/unxed/f4/vfs"
	"github.com/unxed/zipper/archive"
)

type ArchiveProvider struct{}

func (p *ArchiveProvider) Name() string  { return "zipper/archive" }
func (p *ArchiveProvider) Priority() int { return 10 }

// PanelEnterAllowed keeps entries whose archive nature is known only from
// their first bytes out of the ordinary Enter and double-click path: a
// self-extracting installer, and an office document or any other package
// that happens to be a ZIP container. They remain available through the
// explicit Ctrl+PgDn action, which is the deliberate archive-entry gesture.
//
// The rule is that the name outranks the content for the default action. A
// name that declares an archive format keeps Enter; a name that declares
// something else (.docx, .jar, .exe) hands Enter to that extension's
// association, which is what the user asked the system to do with it; a name
// with no extension at all has no association to defer to, so there the
// content decides, minus the self-extracting case.
func (p *ArchiveProvider) PanelEnterAllowed(ctx context.Context, parent vfs.VFS, path string) bool {
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	if _, isLocal := parent.(*vfs.OSVFS); !isLocal {
		// Not the local file system: inside another archive or on a remote
		// one there is no association and no system opener to hand Enter
		// to, so the content stays the only thing to go on.
		return true
	}
	name := path
	if base := parent.Base(path); base != "" {
		name = base
	}
	if !nameDeclaresArchive(name) && filepath.Ext(name) != "" {
		return false
	}
	embedded, found, err := probeArchive(ctx, parent, path)
	return err != nil || !found || embedded.offset <= 0
}

// nameDeclaresArchive reports whether the file name itself claims an archive
// format. It answers from the name and nothing else: archive.DetectFormat
// falls back to reading the file when the extension tells it nothing, and it
// is handed a bare base name here, so its answer would depend on the process
// working directory. The suffixes are the ones DetectFormat picks an engine
// from, plus the split-volume names (name.7z.001, name.z01, name.r00) that
// the readers reach through the content probe instead.
func nameDeclaresArchive(name string) bool {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".zip"),
		strings.HasSuffix(lower, ".tar"),
		strings.Contains(lower, ".tar."),
		strings.HasSuffix(lower, ".tgz"),
		strings.HasSuffix(lower, ".txz"),
		strings.HasSuffix(lower, ".tbz2"),
		strings.HasSuffix(lower, ".tzst"):
		return true
	}
	ext := strings.TrimPrefix(filepath.Ext(lower), ".")
	switch ext {
	case "gz", "bz2", "xz", "zst", "rar", "7z":
		return true
	}
	// A volume suffix only ever reaches this test on a file whose content
	// already matched an archive signature, so recognizing it costs nothing
	// and keeps Enter on the first volume of a split archive.
	if isDigitString(ext) {
		return true
	}
	return len(ext) > 1 && (ext[0] == 'z' || ext[0] == 'r') && isDigitString(ext[1:])
}

func isDigitString(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func (p *ArchiveProvider) CanOpen(ctx context.Context, parent vfs.VFS, path string) bool {
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	if osvfs, ok := parent.(*vfs.OSVFS); ok {
		localPath, _ := osvfs.Abs(path)
		if fi, err := os.Stat(localPath); err == nil {
			if fi.Mode()&(os.ModeNamedPipe|os.ModeSocket|os.ModeDevice|os.ModeCharDevice) != 0 {
				return false
			}
		}
	}
	name := path
	if parent != nil {
		if base := parent.Base(path); base != "" {
			name = base
		}
	}
	format := archive.DetectFormat(name)
	if format != "" {
		return true
	}
	_, found, err := probeArchive(ctx, parent, path)
	return err == nil && found
}

// headProbeLimit is how far into a file the probe reads when the bytes are
// not on the local disk. It is far2l's PluginMaxReadData, the size of the
// window its plugin manager maps over a file before asking the format modules
// whether they recognize it (far2l/src/plug/plugins.cpp), and it is the same
// bet: an archive announces itself at the start of the file, and what sits in
// front of one is a stub.
const headProbeLimit = 256 << 10

// probeArchive finds the archive in a file by reading its first bytes, and it
// is the only thing in this package that decides what a file is. far2l
// decides the same way: its format modules are handed a window over the file
// and are never told its name, so a .jar is a zip there and opens as one.
//
// One question, one scanner, two budgets. On the local disk the bytes are
// free -- the file is right there and nothing has to be decoded to reach byte
// n -- so the scan runs to sfxProbeLimit, far enough to find the archive
// behind an installer stub of any size anyone ships. Everywhere else each
// byte is decompressed or fetched while the panel waits on the answer, so the
// window is far2l's, and only a file system that offered to produce it
// cheaply and silently is asked at all. The rest report nothing and are left
// to be judged by their names, which is what they were before this existed.
func probeArchive(ctx context.Context, parent vfs.VFS, path string) (embeddedArchive, bool, error) {
	if parent == nil {
		return embeddedArchive{}, false, nil
	}
	if osvfs, ok := parent.(*vfs.OSVFS); ok {
		localPath, err := osvfs.Abs(path)
		if err != nil {
			return embeddedArchive{}, false, err
		}
		return findEmbeddedArchive(localPath)
	}
	head, err := vfs.ReadFileHead(ctx, parent, path, headProbeLimit)
	if err != nil || len(head) == 0 {
		return embeddedArchive{}, false, err
	}
	// One buffer read twice: sequentially for the signature scan, and by
	// offset for the signatures that verify themselves. Both address the
	// same bytes from the start of the file, which is what the scan needs.
	window := bytes.NewReader(head)
	return scanEmbeddedArchive(window, window, int64(len(head)))
}

func (p *ArchiveProvider) Open(ctx context.Context, parent vfs.VFS, path string) (vfs.VFS, error) {
	v, err := NewArchiveVFSContext(ctx, parent, path)
	if err != nil {
		// Return an untyped nil so the interface itself is nil: a typed
		// (*ArchiveVFS)(nil) would still compare != nil to callers, and a
		// later Close() on it panics (nil dereference in v.mu.Lock()).
		return nil, err
	}
	return v, nil
}
