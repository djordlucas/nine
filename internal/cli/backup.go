package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"nine/internal/config"
	"nine/internal/memory"
)

// Backup writes a consistent snapshot to path. The extension names the shape:
// `.db` is the store alone, `.tar.gz` is the store and the workspace together.
//
// It opens the store read-only and runs `VACUUM INTO` (memory.BackupTo), so it
// takes a WAL snapshot without ever blocking, or being blocked by, a running
// daemon's writer — the same posture `nine trace` uses. The daemon does not
// need to be stopped, and does not need to be running.
func (c *CLI) Backup(cfg *config.Config, path string) error {
	if isArchivePath(path) {
		return c.backupArchive(cfg, path)
	}
	return c.backupStore(cfg, path)
}

// backupStore writes the database snapshot alone.
func (c *CLI) backupStore(cfg *config.Config, path string) error {
	dbPath, err := cfg.DatabasePath()
	if err != nil {
		return err
	}
	store, err := memory.OpenReadOnly(dbPath)
	if err != nil {
		return fmt.Errorf("open memory store: %w", err)
	}
	defer store.Close() //nolint:errcheck

	abs, err := store.BackupTo(path)
	if err != nil {
		return err
	}
	size := "unknown size"
	if info, err := os.Stat(abs); err == nil {
		size = humanBytes(info.Size())
	}
	fmt.Fprintf(c.Out, "snapshot written: %s (%s)\nsource: %s\n", abs, size, dbPath)
	return nil
}

// humanBytes renders a byte count for a person reading one line of output.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// backupArchive writes the store and the workspace into one gzipped tar.
//
// The snapshot is staged beside the destination rather than in the system temp
// directory: a workspace-sized archive wants the space where the operator
// pointed it, and a same-filesystem staging area keeps the copy local.
func (c *CLI) backupArchive(cfg *config.Config, path string) error {
	dbPath, err := cfg.DatabasePath()
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", path, err)
	}
	if _, err := os.Stat(abs); err == nil {
		return fmt.Errorf("%s already exists; write the archive to a new path rather than replacing one", abs)
	}
	workRoot := cfg.Workspace.Root
	if workRoot == "" {
		return fmt.Errorf("no workspace is configured ([workspace].root), so there are no files to archive; " +
			"back up the store alone with a .db destination")
	}
	if _, err := os.Stat(workRoot); err != nil {
		return fmt.Errorf("workspace %s: %w", workRoot, err)
	}

	stage, err := os.MkdirTemp(filepath.Dir(abs), ".nine-backup-")
	if err != nil {
		return fmt.Errorf("create staging directory: %w", err)
	}
	defer os.RemoveAll(stage) //nolint:errcheck

	store, err := memory.OpenReadOnly(dbPath)
	if err != nil {
		return fmt.Errorf("open memory store: %w", err)
	}
	staged, err := store.BackupTo(filepath.Join(stage, archiveDBEntry))
	store.Close() //nolint:errcheck
	if err != nil {
		return err
	}

	files, skipped, err := writeArchive(abs, staged, workRoot)
	if err != nil {
		os.Remove(abs) //nolint:errcheck // a half-written archive is worse than none
		return err
	}

	size := "unknown size"
	if info, err := os.Stat(abs); err == nil {
		size = humanBytes(info.Size())
	}
	fmt.Fprintf(c.Out, "archive written: %s (%s)\n", abs, size)
	fmt.Fprintf(c.Out, "  store:     %s\n", dbPath)
	fmt.Fprintf(c.Out, "  workspace: %s (%d files)\n", workRoot, files)
	if skipped > 0 {
		fmt.Fprintf(c.Out, "  skipped:   %d non-regular entries (symlinks, sockets, devices)\n", skipped)
	}
	return nil
}
