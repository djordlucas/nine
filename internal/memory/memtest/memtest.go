// Package memtest provides an isolated SQLite-backed memory.Store for tests.
// Each Open call creates a database file in the test's own temp directory, so
// tests are isolated by construction and safe to run in parallel — there is no
// shared server, and therefore nothing that can be unavailable.
package memtest

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"nine/internal/memory"

	_ "modernc.org/sqlite" // the driver Exec opens its own connection with
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

// OpenAt is Open against a caller-chosen path, for a test that also wants to
// reach the same database through Exec below.
func OpenAt(t testing.TB, path string) (*memory.Store, error) {
	t.Helper()
	store, err := memory.Open(path)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { store.Close() }) //nolint:errcheck // best-effort cleanup
	return store, nil
}

// Exec runs raw SQL against the database at path, on its own connection.
//
// It exists so a test outside package memory can set up state the public API
// deliberately does not expose — backdating a conversation's updated_at to test
// retention, most obviously. memory's own tests use an internal helper for this;
// this is the same affordance for everyone else, and it lives in the test-support
// package rather than becoming a method on Store that production could call.
//
// WAL means a second connection is fine alongside the store's own pools.
func Exec(t testing.TB, path, query string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("memtest.Exec open: %v", err)
	}
	defer db.Close() //nolint:errcheck
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("memtest.Exec %q: %v", query, err)
	}
}

// Backdate moves a conversation's last-activity timestamp into the past, in the
// exact format the store writes — a test that formats it by hand and gets the
// precision wrong compares as *older* than every real row, silently.
func Backdate(t testing.TB, path, sessionID string, age time.Duration) {
	t.Helper()
	Exec(t, path, `UPDATE conversations SET updated_at = ? WHERE id = ?`,
		time.Now().Add(-age).UTC().Format("2006-01-02T15:04:05.000000Z"), sessionID)
}
