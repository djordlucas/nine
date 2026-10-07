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

// hasTableAt answers from sqlite_master rather than the Store, so it can inspect a
// database the way an older binary left it.
func hasTableAt(t *testing.T, path, table string) bool {
	t.Helper()
	w := openRaw(t, path)
	rows, err := w.Query(`SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?`, table)
	if err != nil {
		t.Fatalf("sqlite_master: %v", err)
	}
	defer rows.Close() //nolint:errcheck
	return rows.Next()
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

// C5: the reflections table is dropped from a database that has one, and its
// absence is not mistaken for a fresh install.
func TestMigrationDropsReflectionsTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nine.db")

	// A database as an earlier binary left it: reflections present, stamped at
	// the version before the drop.
	func() {
		w := openRaw(t, path)
		mustExec(t, w, `CREATE TABLE conversations (id TEXT PRIMARY KEY)`)
		mustExec(t, w, `CREATE TABLE reflections (id TEXT PRIMARY KEY, ran_at TEXT, summary TEXT)`)
		mustExec(t, w, `INSERT INTO reflections (id, summary) VALUES ('r1', 'old reflection')`)
		mustExec(t, w, `PRAGMA user_version = 2`)
	}()

	store, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	store.Close() //nolint:errcheck

	w := openRaw(t, path)
	var n int
	if err := w.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'reflections'`,
	).Scan(&n); err != nil {
		t.Fatalf("check table: %v", err)
	}
	if n != 0 {
		t.Error("reflections table survived the migration")
	}
	if got := versionAt(t, path); got != schemaVersion() {
		t.Errorf("user_version = %d, want %d", got, schemaVersion())
	}
}

// A fresh database never creates the table in the first place, so the drop step
// is skipped entirely (fresh databases run no steps) and nothing is left behind.
func TestFreshDatabaseHasNoReflectionsTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nine.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	store.Close() //nolint:errcheck

	w := openRaw(t, path)
	var n int
	if err := w.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'reflections'`,
	).Scan(&n); err != nil {
		t.Fatalf("check table: %v", err)
	}
	if n != 0 {
		t.Error("a fresh database created the reflections table")
	}
}

// C6: the subtree column is dropped from a database that has one, without
// disturbing the rows or the parent_id edge that replaces it.
func TestMigrationDropsGoalSubtreeColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nine.db")

	func() {
		w := openRaw(t, path)
		mustExec(t, w, `CREATE TABLE conversations (id TEXT PRIMARY KEY)`)
		mustExec(t, w, `CREATE TABLE goals (
			id          TEXT PRIMARY KEY,
			description TEXT NOT NULL DEFAULT '',
			status      TEXT NOT NULL DEFAULT 'active',
			subtree     TEXT NOT NULL DEFAULT '[]',
			parent_id   TEXT,
			parent_type TEXT,
			created_at  TEXT NOT NULL DEFAULT '',
			updated_at  TEXT NOT NULL DEFAULT ''
		)`)
		mustExec(t, w, `INSERT INTO goals (id, description, subtree, parent_id)
		                VALUES ('root', 'a goal', '["stale-entry"]', NULL)`)
		mustExec(t, w, `INSERT INTO goals (id, description, parent_id, parent_type)
		                VALUES ('child', 'a child', 'root', 'goal')`)
		mustExec(t, w, `PRAGMA user_version = 3`)
	}()

	store, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close() //nolint:errcheck

	if hasColumn(t, path, "goals", "subtree") {
		t.Error("goals.subtree survived the migration")
	}

	// The rows are intact, and the edge that replaces the column still resolves —
	// including for a parent whose stored subtree said something else entirely.
	g, err := store.GoalGet("root")
	if err != nil {
		t.Fatalf("GoalGet: %v", err)
	}
	if g == nil || g.Description != "a goal" {
		t.Fatalf("root goal = %+v, want it preserved", g)
	}
	if len(g.Subtree) != 1 || g.Subtree[0] != "child" {
		t.Errorf("derived subtree = %v, want [child] — not the stale stored value", g.Subtree)
	}
}

// The step is safe against a database that never had the column.
func TestMigrationDropGoalSubtreeIsSafeWhenAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nine.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	store.Close() //nolint:errcheck

	if hasColumn(t, path, "goals", "subtree") {
		t.Error("a fresh database created goals.subtree")
	}
	// Re-opening runs the runner again against a database already at the target.
	store2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	store2.Close() //nolint:errcheck
}

// The rename moves only the column; the stored JSON array and its object keys
// are untouched, so an existing plan must load unchanged afterwards.
func TestMigrationRenamesStagesToRoutines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nine.db")
	const stored = `[{"name":"pursue","kind":"pursue","status":"active"}]`

	func() {
		w := openRaw(t, path)
		mustExec(t, w, `CREATE TABLE conversations (id TEXT PRIMARY KEY)`)
		mustExec(t, w, `CREATE TABLE session_plans (
			id         TEXT PRIMARY KEY,
			status     TEXT NOT NULL DEFAULT 'active',
			stages     TEXT NOT NULL DEFAULT '[]',
			created_at TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL DEFAULT ''
		)`)
		mustExec(t, w, `INSERT INTO session_plans (id, status, stages) VALUES ('a1','active','`+stored+`')`)
		mustExec(t, w, `PRAGMA user_version = 4`)
	}()

	store, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close() //nolint:errcheck

	if hasColumn(t, path, "session_plans", "stages") {
		t.Error("the stages column survived the rename")
	}
	if !hasColumn(t, path, "session_plans", "routines") {
		t.Fatal("no routines column after the rename")
	}

	plan, err := store.SessionPlanGet("a1")
	if err != nil {
		t.Fatalf("SessionPlanGet: %v", err)
	}
	if plan == nil {
		t.Fatal("plan vanished across the rename")
	}
	if len(plan.Routines) != 1 || plan.Routines[0].Kind != "pursue" || plan.Routines[0].Status != "active" {
		t.Errorf("routines = %+v, want the stored pursue routine unchanged", plan.Routines)
	}
}

// A fresh database has the new name and never the old one.
func TestFreshDatabaseUsesRoutinesColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nine.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	store.Close() //nolint:errcheck

	if hasColumn(t, path, "session_plans", "stages") {
		t.Error("a fresh database created the old stages column")
	}
	if !hasColumn(t, path, "session_plans", "routines") {
		t.Error("a fresh database is missing the routines column")
	}
}

// A database migrated by the intervening build sits at version 5 with an
// `aspects` column. It must reach `routines` without passing through the
// stages→aspects step, which no longer applies to it.
func TestMigrationRenamesAspectsToRoutines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nine.db")
	const stored = `[{"name":"pursue","kind":"pursue","status":"active"}]`

	func() {
		w := openRaw(t, path)
		mustExec(t, w, `CREATE TABLE conversations (id TEXT PRIMARY KEY)`)
		mustExec(t, w, `CREATE TABLE session_plans (
			id         TEXT PRIMARY KEY,
			status     TEXT NOT NULL DEFAULT 'active',
			aspects    TEXT NOT NULL DEFAULT '[]',
			created_at TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL DEFAULT ''
		)`)
		mustExec(t, w, `INSERT INTO session_plans (id, status, aspects) VALUES ('a1','active','`+stored+`')`)
		mustExec(t, w, `PRAGMA user_version = 5`)
	}()

	store, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close() //nolint:errcheck

	if hasColumn(t, path, "session_plans", "aspects") {
		t.Error("the aspects column survived")
	}
	if !hasColumn(t, path, "session_plans", "routines") {
		t.Fatal("no routines column")
	}
	plan, err := store.SessionPlanGet("a1")
	if err != nil || plan == nil {
		t.Fatalf("SessionPlanGet = %v, %v", plan, err)
	}
	if len(plan.Routines) != 1 || plan.Routines[0].Kind != "pursue" {
		t.Errorf("routines = %+v, want the stored pursue routine intact", plan.Routines)
	}
}

// A database left by a binary that knew only plugin_jobs. Its rows must survive
// into the generalized `jobs` table as backend='plugin', because a job in flight
// when the operator upgraded is exactly the row whose loss would be noticed.
func TestMigratesPluginJobsIntoJobs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nine.db")

	func() {
		w := openRaw(t, path)
		mustExec(t, w, `CREATE TABLE conversations (id TEXT PRIMARY KEY)`)
		mustExec(t, w, `CREATE TABLE plugin_jobs (
			handle        TEXT PRIMARY KEY,
			plugin        TEXT NOT NULL,
			tool          TEXT NOT NULL,
			plugin_job_id TEXT NOT NULL,
			owner_id      TEXT NOT NULL DEFAULT '',
			state         TEXT NOT NULL DEFAULT 'running',
			ack           TEXT NOT NULL DEFAULT '',
			progress      TEXT NOT NULL DEFAULT '',
			output        TEXT NOT NULL DEFAULT '',
			spill_path    TEXT NOT NULL DEFAULT '',
			error         TEXT NOT NULL DEFAULT '',
			created_at    TEXT NOT NULL DEFAULT '',
			updated_at    TEXT NOT NULL DEFAULT '',
			finished_at   TEXT
		)`)
		mustExec(t, w, `INSERT INTO plugin_jobs
			(handle, plugin, tool, plugin_job_id, owner_id, state, ack, progress, created_at, updated_at)
			VALUES ('job_old', 'downloader', 'download_file', 'pj-1', 'agent-7', 'running', 'started', '41%', '2026-01-01T00:00:00.000000Z', '2026-01-01T00:00:00.000000Z')`)
		mustExec(t, w, `PRAGMA user_version = 6`)
	}()

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close() //nolint:errcheck

	j, found, err := store.JobGet("job_old")
	if err != nil || !found {
		t.Fatalf("JobGet = found %v err %v; the in-flight job did not survive the migration", found, err)
	}
	if j.Backend != JobBackendPlugin {
		t.Errorf("backend = %q, want %q — a migrated row predates the tool backend by definition", j.Backend, JobBackendPlugin)
	}
	if j.BackendRef != "pj-1" {
		t.Errorf("backend_ref = %q, want the old plugin_job_id %q", j.BackendRef, "pj-1")
	}
	for _, c := range []struct{ name, got, want string }{
		{"plugin", j.Plugin, "downloader"},
		{"tool", j.Tool, "download_file"},
		{"owner", j.OwnerID, "agent-7"},
		{"state", j.State, "running"},
		{"ack", j.Ack, "started"},
		{"progress", j.Progress, "41%"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}

	// The old table is gone, so nothing writes to it by accident afterwards.
	if hasTable(store.db, "plugin_jobs") {
		t.Error("plugin_jobs still exists after the migration")
	}
}

// A database left by a binary from before the capability tables exist must gain
// them, and gain them empty: an approved grant is a decision nobody made yet.
func TestMigratesDatabaseMissingCapabilityTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nine.db")

	// The shape a binary at version 11 left: conversations present so Open does
	// not treat this as a fresh database, and stamped one step behind the tables
	// this step adds.
	func() {
		w := openRaw(t, path)
		mustExec(t, w, `CREATE TABLE conversations (id TEXT PRIMARY KEY)`)
		mustExec(t, w, `PRAGMA user_version = 11`)
	}()

	if hasTableAt(t, path, "capability_grants") {
		t.Fatal("precondition failed: the fixture already has capability_grants")
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close() //nolint:errcheck

	for _, table := range []string{"capability_requests", "capability_grants"} {
		if !hasTableAt(t, path, table) {
			t.Errorf("migration did not create %s", table)
		}
	}
	if got := versionAt(t, path); got != schemaVersion() {
		t.Errorf("user_version = %d, want %d", got, schemaVersion())
	}

	// Usable, not merely present.
	if err := store.CapabilityRequestCreate("r1", "c1", "t", "net.http", "", ""); err != nil {
		t.Errorf("the migrated table does not accept a request: %v", err)
	}
	if grants, err := store.CapabilityGrantList(); err != nil || len(grants) != 0 {
		t.Errorf("grants = %v, err = %v; a migrated database confers nothing", grants, err)
	}
}

// Standing tools become processes in place: a version-12 database's standing
// runs keep their definition and run state, and a condition trigger's
// wake_agent becomes report_to. initSchema creates an empty `processes` table
// before migrating, which the step must replace rather than collide with.
func TestMigratesStandingToolsIntoProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nine.db")

	func() {
		w := openRaw(t, path)
		mustExec(t, w, `CREATE TABLE conversations (id TEXT PRIMARY KEY)`)
		mustExec(t, w, `CREATE TABLE standing_tools (
			id            TEXT PRIMARY KEY,
			tool          TEXT NOT NULL,
			args          TEXT NOT NULL DEFAULT '{}',
			interval_secs INTEGER NOT NULL DEFAULT 0,
			schedule      TEXT NOT NULL DEFAULT '',
			state         TEXT NOT NULL DEFAULT 'running',
			cursor        TEXT NOT NULL DEFAULT '',
			calls         INTEGER NOT NULL DEFAULT 0,
			cycles        INTEGER NOT NULL DEFAULT 0,
			failures      INTEGER NOT NULL DEFAULT 0,
			last_error    TEXT NOT NULL DEFAULT '',
			last_call_at  TEXT NOT NULL DEFAULT '',
			next_at       TEXT NOT NULL DEFAULT '',
			generated     INTEGER NOT NULL DEFAULT 0,
			wake_agent    TEXT NOT NULL DEFAULT '',
			created_at    TEXT NOT NULL DEFAULT '',
			updated_at    TEXT NOT NULL DEFAULT ''
		)`)
		mustExec(t, w, `INSERT INTO standing_tools (id, tool, interval_secs, state, cycles, wake_agent)
			VALUES ('when:sec-watch', 'cve_scan', 10, 'stopped', 7, 'sec-watch')`)
		mustExec(t, w, `PRAGMA user_version = 12`)
	}()

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close() //nolint:errcheck

	p, found, err := store.ProcessGet("when:sec-watch")
	if err != nil || !found {
		t.Fatalf("ProcessGet = found %v err %v; the standing run did not survive", found, err)
	}
	if p.Tool != "cve_scan" || p.IntervalSecs != 10 || p.State != ProcessStopped || p.Cycles != 7 {
		t.Errorf("process = %+v, want the standing run's definition and state", p)
	}
	if p.ReportTo != "sec-watch" {
		t.Errorf("report_to = %q, want the condition trigger's agent", p.ReportTo)
	}
}
