package runtime

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Nine's own bookkeeping inside the workspace. The workspace is the operator's
// directory — frequently a bind mount of one that already holds their data — so
// everything Nine keeps there is confined to one dotted directory that the file
// tools refuse to write and the scan ignores.
const (
	// WorkspaceStateDir holds the trash and anything else Nine keeps beside the
	// operator's files.
	WorkspaceStateDir = ".nine"
	// WorkspaceTrashDir is where a deleted or overwritten file goes instead of
	// being destroyed. Inside the workspace, because a path outside the root is
	// one the operator never offered — and because a rename within the mount
	// costs nothing while a copy across volumes costs the whole file.
	WorkspaceTrashDir = WorkspaceStateDir + "/trash"
)

// trashSweepInterval matches the spill sweeper's: both clear debris from work
// that has already finished.
const trashSweepInterval = time.Hour

// PrepareWorkspaceState creates the workspace's state directory and keeps it out
// of the operator's version control.
//
// The `.gitignore` is not a courtesy. A workspace is routinely a git repository
// the operator mounted, and a trash directory appearing in `git status` after
// every edit would be noise they cannot turn off — and, worse, something they
// might commit. `*` covers the file itself, so the directory is invisible whole.
func PrepareWorkspaceState(root string) error {
	if root == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(root, WorkspaceTrashDir), 0o755); err != nil {
		return err
	}
	ignore := filepath.Join(root, WorkspaceStateDir, ".gitignore")
	if _, err := os.Stat(ignore); err == nil {
		return nil
	}
	return os.WriteFile(ignore, []byte("*\n"), 0o644)
}

// trashEntry is one deletion or overwrite: the directory holding the file, when
// it was trashed, and how much disk it holds.
type trashEntry struct {
	dir   string
	at    time.Time
	bytes int64
}

// RunTrashSweeper bounds the workspace trash, once at startup and then on every
// tick until ctx is cancelled.
//
// Two bounds, because one is not enough. Age alone lets a week of large
// deletions outgrow the volume; size alone would keep a single stale file
// forever on a quiet system. Production-only housekeeping, like the spill
// sweeper: a harness that throws its workspace away never starts one.
func RunTrashSweeper(ctx context.Context, root string, retention time.Duration, maxBytes int64) {
	if root == "" {
		return
	}
	sweep := func() { sweepTrash(root, retention, maxBytes) }
	sweep()
	t := time.NewTicker(trashSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweep()
		}
	}
}

// sweepTrash removes expired entries, then the oldest remaining entries until
// the trash fits its size bound. Errors are logged rather than returned:
// housekeeping must never take the daemon down.
func sweepTrash(root string, retention time.Duration, maxBytes int64) {
	trash := filepath.Join(root, WorkspaceTrashDir)
	entries, err := readTrashEntries(trash)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("trash sweep: cannot read the trash", "path", trash, "err", err)
		}
		return
	}

	var removed int
	var freed int64
	kept := entries[:0]
	cutoff := time.Now().Add(-retention)
	for _, e := range entries {
		// A non-positive retention disables the age bound; the size bound below
		// still applies, so "keep everything" is never what this means.
		if retention > 0 && e.at.Before(cutoff) {
			if err := os.RemoveAll(e.dir); err != nil {
				slog.Warn("trash sweep: cannot remove entry", "dir", e.dir, "err", err)
				continue
			}
			removed++
			freed += e.bytes
			continue
		}
		kept = append(kept, e)
	}

	// Oldest first, so what survives a size sweep is what was deleted most
	// recently — the entries someone is most likely to want back.
	sort.Slice(kept, func(i, j int) bool { return kept[i].at.Before(kept[j].at) })
	var total int64
	for _, e := range kept {
		total += e.bytes
	}
	for i := 0; i < len(kept) && maxBytes > 0 && total > maxBytes; i++ {
		if err := os.RemoveAll(kept[i].dir); err != nil {
			slog.Warn("trash sweep: cannot remove entry", "dir", kept[i].dir, "err", err)
			continue
		}
		total -= kept[i].bytes
		removed++
		freed += kept[i].bytes
	}

	if removed > 0 {
		slog.Info("swept workspace trash", "entries", removed, "bytes", freed)
	}
}

// readTrashEntries lists the trash's top-level entries with their age and size.
//
// Age comes from the timestamp in the entry's name, never from the file's own
// mtime: a rename preserves mtime, so a trashed file still reports when it was
// last written, which may be months before anyone deleted it.
func readTrashEntries(trash string) ([]trashEntry, error) {
	dirents, err := os.ReadDir(trash)
	if err != nil {
		return nil, err
	}
	out := make([]trashEntry, 0, len(dirents))
	for _, d := range dirents {
		if !d.IsDir() {
			continue
		}
		at, ok := trashEntryTime(d.Name())
		if !ok {
			// A directory nobody here created. Left alone rather than swept:
			// deleting an unrecognized directory in the operator's workspace is
			// not a housekeeping decision to make quietly.
			continue
		}
		dir := filepath.Join(trash, d.Name())
		out = append(out, trashEntry{dir: dir, at: at, bytes: dirSize(dir)})
	}
	return out, nil
}

// trashEntryTime parses the timestamp prefix of an entry name, which the tools
// write as <RFC3339-ish UTC>-<hex>: 20260920T143015Z-1f2e3d4c.
func trashEntryTime(name string) (time.Time, bool) {
	cut := strings.LastIndex(name, "-")
	if cut <= 0 {
		return time.Time{}, false
	}
	at, err := time.Parse("20060102T150405Z", name[:cut])
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}

func dirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // a size estimate must not fail the sweep
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}
