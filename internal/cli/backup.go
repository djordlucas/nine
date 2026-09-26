package cli

import (
	"fmt"
	"os"

	"nine/internal/config"
	"nine/internal/memory"
)

// Backup writes a consistent snapshot of the store to path.
//
// It opens the store read-only and runs `VACUUM INTO` (memory.BackupTo), so it
// takes a WAL snapshot without ever blocking, or being blocked by, a running
// daemon's writer — the same posture `nine trace` uses. The daemon does not
// need to be stopped, and does not need to be running.
func (c *CLI) Backup(cfg *config.Config, path string) error {
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
