package memory

import (
	"fmt"
	"os"
	"path/filepath"
)

// BackupTo writes a consistent snapshot of the database to path.
//
// SQLite's `VACUUM INTO` does the work, and the choice matters. The store runs
// in WAL mode, so `nine.db` on its own is not the database: the `-wal` sidecar
// holds committed transactions the main file has not absorbed yet. Copying the
// three files with `cp` while the daemon writes gives a torn set — each file
// read at a different instant — and copying `nine.db` alone silently loses
// whatever is still in the WAL. `VACUUM INTO` instead reads one transaction and
// writes a single fully-formed file with no sidecars, so it is consistent by
// construction and needs no downtime.
//
// The destination must not exist. SQLite refuses to overwrite one; the check
// here only exists to say so in a sentence an operator can act on, and SQLite
// remains the real guard if the file appears in between.
func (s *Store) BackupTo(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("a destination path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", path, err)
	}
	if _, err := os.Stat(abs); err == nil {
		return "", fmt.Errorf("%s already exists; write the snapshot to a new path rather than replacing one", abs)
	}
	if dir := filepath.Dir(abs); dir != "" {
		if _, err := os.Stat(dir); err != nil {
			return "", fmt.Errorf("directory %s does not exist; create it first", dir)
		}
	}
	// Bound parameter, not string concatenation: the path comes from an
	// operator's command line and has no business being spliced into SQL.
	if _, err := s.db.Exec("VACUUM INTO ?", abs); err != nil {
		return "", fmt.Errorf("snapshot to %s: %w", abs, err)
	}
	return abs, nil
}
