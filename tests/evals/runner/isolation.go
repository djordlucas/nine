package runner

import (
	"os"
	"path/filepath"

	"nine/internal/memory"
)

// openIsolatedStore creates a fresh SQLite database in its own directory and
// returns a memory.Store bound to it, plus a cleanup that closes the store and
// removes the directory. It is the non-testing.T twin of memtest.Open: each
// eval run (Track L repetition or Track R replay) gets its own database file so
// cases never see each other's state (docs/evals.md §5, "Isolation per run").
//
// baseDir, when set, overrides where that directory is created; empty means a
// fresh one under the system temp dir. Evals no longer need any external
// service, so there is no unreachable-database case to report.
func openIsolatedStore(baseDir string) (*memory.Store, func(), error) {
	dir, err := os.MkdirTemp(baseDir, "nine-eval-db-")
	if err != nil {
		return nil, nil, err
	}
	store, err := memory.Open(filepath.Join(dir, "nine.db"))
	if err != nil {
		os.RemoveAll(dir) //nolint:errcheck
		return nil, nil, err
	}
	cleanup := func() {
		store.Close()     //nolint:errcheck
		os.RemoveAll(dir) //nolint:errcheck
	}
	return store, cleanup, nil
}
