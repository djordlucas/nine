// Package memtest provides an isolated SQLite-backed memory.Store for tests.
// Each Open call creates a database file in the test's own temp directory, so
// tests are isolated by construction and safe to run in parallel — there is no
// shared server, and therefore nothing that can be unavailable.
package memtest

import (
	"path/filepath"
	"testing"

	"nine/internal/memory"
)

// Open returns a memory.Store backed by a fresh, empty database under
// t.TempDir(). The store is closed and the file removed via t.Cleanup.
//
// It returns (store, nil) on success, a shape kept so that call sites stay
// untouched — but the error is now only ever a genuine failure. There is no
// skip-if-the-database-is-unreachable path any more, and with it goes the
// possibility of the suite reporting green because most of it never ran.
//
// It takes a testing.TB rather than a *testing.T so fuzz targets can use it too.
//
// A temp file rather than an in-memory database, deliberately: with `:memory:`
// each pooled connection opens its own separate database, which the store's
// reader/writer pool split would expose immediately, and the shared-cache
// workaround swaps SQLite's locking model for one production never uses. A file
// exercises what actually ships — WAL, busy_timeout, and the read-only pool.
func Open(t testing.TB) (*memory.Store, error) {
	t.Helper()

	store, err := memory.Open(filepath.Join(t.TempDir(), "nine.db"))
	if err != nil {
		return nil, err
	}
	// t.TempDir registers its own removal when it is called, i.e. before this
	// one, and cleanups run last-in-first-out — so the store closes before the
	// directory is removed. The other order leaves the WAL sidecar files behind
	// and fails the removal.
	t.Cleanup(func() { store.Close() }) //nolint:errcheck // best-effort cleanup
	return store, nil
}
