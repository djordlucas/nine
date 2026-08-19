package memory

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// openRaw opens a bare writer connection to path, bypassing Open (and therefore
// initSchema), so a test can shape a database the way an older binary left it.
func openRaw(t *testing.T, path string) *sql.DB {
	t.Helper()
	w, err := sql.Open("sqlite", fmt.Sprintf(writeDSN, path))
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	w.SetMaxOpenConns(1)
	t.Cleanup(func() { w.Close() }) //nolint:errcheck // best-effort
	return w
}

func versionAt(t *testing.T, path string) int {
	t.Helper()
	w := openRaw(t, path)
	var v int
	if err := w.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return v
}

func hasColumn(t *testing.T, path, table, column string) bool {
	t.Helper()
	w := openRaw(t, path)
	rows, err := w.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer rows.Close() //nolint:errcheck
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, ctype      string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if name == column {
			return true
		}
	}
	return false
}

// Step hygiene. The version itself needs no test — it is len(migrations) — but
// a step with no name produces an unreadable migration failure, and a duplicate
// name makes two different changes indistinguishable in that message.
func TestMigrationStepsAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for i, m := range migrations {
		if m.name == "" {
			t.Errorf("migrations[%d] has no name", i)
		}
		if seen[m.name] {
			t.Errorf("migrations[%d]: duplicate name %q", i, m.name)
		}
		seen[m.name] = true
		if m.fn == nil {
			t.Errorf("migrations[%d] (%s) has no function", i, m.name)
		}
	}
}

// A database created from scratch is stamped at the current version, because
// initSchema already built the current shape.
func TestFreshDatabaseStampedAtCurrentVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nine.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	store.Close() //nolint:errcheck

	if got := versionAt(t, path); got != schemaVersion() {
		t.Errorf("user_version = %d, want %d", got, schemaVersion())
	}
	if !hasColumn(t, path, "tools", "lockfile") {
		t.Error("fresh database is missing tools.lockfile")
	}
}

// The real case the runner exists for: a database left by a binary whose
// `tools` table predates the lockfile column. CREATE TABLE IF NOT EXISTS cannot
// add it, so the step must.
func TestMigratesLegacyDatabaseMissingLockfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nine.db")

	// Shape the database the way v2.1.0 left it: a tools table without
	// lockfile, stamped at version 1.
	func() {
		w := openRaw(t, path)
		mustExec(t, w, `CREATE TABLE tools (
			name         TEXT PRIMARY KEY,
			description  TEXT NOT NULL DEFAULT '',
			input_schema TEXT NOT NULL DEFAULT '{}',
			source       TEXT NOT NULL DEFAULT '',
			capabilities TEXT NOT NULL DEFAULT '{}'
		)`)
		mustExec(t, w, `INSERT INTO tools (name) VALUES ('legacy_tool')`)
		mustExec(t, w, `PRAGMA user_version = 1`)
	}()

	if hasColumn(t, path, "tools", "lockfile") {
		t.Fatal("precondition failed: the legacy fixture already has lockfile")
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	store.Close() //nolint:errcheck

	if !hasColumn(t, path, "tools", "lockfile") {
		t.Error("migration did not add tools.lockfile")
	}
	if got := versionAt(t, path); got != schemaVersion() {
		t.Errorf("user_version = %d, want %d", got, schemaVersion())
	}

	// The existing row survives, with the column's default rather than NULL.
	w := openRaw(t, path)
	var lockfile string
	if err := w.QueryRow(`SELECT lockfile FROM tools WHERE name = 'legacy_tool'`).Scan(&lockfile); err != nil {
		t.Fatalf("read migrated row: %v", err)
	}
	if lockfile != "{}" {
		t.Errorf("migrated lockfile = %q, want the column default %q", lockfile, "{}")
	}
}

// Opening repeatedly must not re-run a step or move the version.
func TestMigrationIsIdempotentAcrossOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nine.db")
	for i := range 3 {
		store, err := Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		store.Close() //nolint:errcheck
		if got := versionAt(t, path); got != schemaVersion() {
			t.Fatalf("after open %d: user_version = %d, want %d", i, got, schemaVersion())
		}
	}
}

// An older binary opening a database a newer one has already migrated must
// refuse rather than run against a shape it does not understand.
func TestRefusesDatabaseNewerThanBinary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nine.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	store.Close() //nolint:errcheck

	func() {
		w := openRaw(t, path)
		mustExec(t, w, fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion()+5))
	}()

	_, err = Open(path)
	if err == nil {
		t.Fatal("Open succeeded against a database newer than the binary; want an error")
	}
	if !strings.Contains(err.Error(), "newer than this binary") {
		t.Errorf("error = %v, want it to name the version mismatch", err)
	}
	// The version is left alone: refusing must not rewrite the header.
	if got := versionAt(t, path); got != schemaVersion()+5 {
		t.Errorf("user_version = %d, want %d untouched", got, schemaVersion()+5)
	}
}

// The atomicity guarantee: a step that fails commits nothing, including its own
// version bump, so the next Open retries it from the same version rather than
// resuming half-applied.
func TestFailedStepRollsBackWithItsVersionBump(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nine.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	store.Close() //nolint:errcheck

	boom := errors.New("step exploded")
	// The version the database is actually at on disk, captured before the extra
	// step raises schemaVersion().
	before := schemaVersion()
	restore := migrations
	migrations = append(append([]migrationStep{}, migrations...), migrationStep{
		name: "failing_step",
		fn: func(q sqlExec) error {
			// Write first, then fail: the write must be rolled back too.
			if _, err := q.Exec(`CREATE TABLE should_not_survive (x TEXT)`); err != nil {
				return err
			}
			return boom
		},
	})
	t.Cleanup(func() { migrations = restore })

	w := openRaw(t, path)
	d := db{w: w, r: w}

	err = migrate(d, before)
	if err == nil {
		t.Fatal("migrate succeeded with a failing step; want an error")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want it to wrap the step's error", err)
	}
	if !strings.Contains(err.Error(), "failing_step") {
		t.Errorf("error = %v, want it to name the step that failed", err)
	}

	var v int
	if err := w.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if v != before {
		t.Errorf("user_version = %d, want %d — the failed step's bump must roll back too",
			v, before)
	}

	var n int
	if err := w.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'should_not_survive'`,
	).Scan(&n); err != nil {
		t.Fatalf("check rollback: %v", err)
	}
	if n != 0 {
		t.Error("the failed step's write survived; it was not rolled back")
	}
}

// A step is applied exactly once: a database already at the target version runs
// nothing, even if the step would error were it re-applied.
func TestStepsRunOnlyForVersionsBelowTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nine.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	store.Close() //nolint:errcheck

	var ran int
	before := schemaVersion()
	restore := migrations
	migrations = append(append([]migrationStep{}, migrations...), migrationStep{
		name: "counted_step",
		fn:   func(sqlExec) error { ran++; return nil },
	})
	t.Cleanup(func() { migrations = restore })

	w := openRaw(t, path)
	d := db{w: w, r: w}

	if err := migrate(d, before); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if ran != 1 {
		t.Fatalf("step ran %d times on the first pass, want 1", ran)
	}

	// The database is at the new target now; a second pass has nothing to do.
	if err := migrate(d, schemaVersion()); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if ran != 1 {
		t.Errorf("step ran %d times total, want 1 — it re-ran against an up-to-date database", ran)
	}
}

func mustExec(t *testing.T, w *sql.DB, query string) {
	t.Helper()
	if _, err := w.Exec(query); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}
