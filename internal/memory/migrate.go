package memory

import (
	"database/sql"
	"fmt"
)

// schemaVersion is the schema version this binary expects, and the value
// PRAGMA user_version carries once Open returns.
//
// It is *derived* from the step list rather than declared beside it:
// migrations[i] moves a database from version i to version i+1, so the current
// version is simply how many steps exist. A hand-maintained constant would be a
// second source of truth for the same fact, and the failure mode when the two
// disagree is silent — a step that never runs, or an index past the end of the
// slice. Appending a step is therefore the only way to bump the version.
func schemaVersion() int { return len(migrations) }

// sqlExec is the subset of database/sql shared by db and *sql.Tx. Migration
// steps take it rather than a concrete type so a step body is identical whether
// it runs inside the runner's transaction or is called directly from a test.
//
// A step MUST do all of its work through the sqlExec it is handed. Reaching for
// the enclosing db instead **deadlocks**: the writer pool holds exactly one
// connection (Open sets MaxOpenConns(1)), the running transaction already owns
// it, and a second write waits for a connection that cannot be released until
// the transaction it is blocking finishes.
type sqlExec interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
}

// migrationStep is one forward move between adjacent schema versions.
//
// There is deliberately no down-step. A rollback that has to reverse a
// destructive change cannot restore the data the change dropped, so the honest
// recovery for a bad migration is to restore the file and run a corrected
// forward step — which is also why every step runs in a transaction.
type migrationStep struct {
	name string
	fn   func(sqlExec) error
}

// migrations is the ordered list of schema steps, indexed by the version each
// one migrates *from*. Append only: renumbering an existing entry silently
// re-runs or skips it against databases already in the field.
//
// A step MUST be safe to run against any database at its "from" version,
// including one where an earlier binary already applied the equivalent change
// ad hoc. It need NOT be safe against a fresh database — a fresh database is
// stamped at schemaVersion() without running any step (see initSchema), because
// initSchema already creates the current shape and a step like a column rename
// would fail against it.
//
// A step also MUST NOT assume the reader pool exists. Migrations run from
// initSchema, which Open calls before opening readers, so db.r is still nil and
// anything routed there by isRead (i.e. any SELECT) would panic. Use the sqlExec
// passed in, which is the transaction on the writer.
var migrations = []migrationStep{
	// 0 → 1: the baseline. Every SQLite database Nine has ever created was
	// stamped user_version = 1 by initSchema, so nothing in the field sits at 0
	// and this step has no work to do. It exists to keep the index aligned with
	// the version it migrates from, so migrations[i] is always i → i+1.
	{name: "baseline", fn: func(sqlExec) error { return nil }},

	// 1 → 2: the `tools` table shipped in v2.1.0 without a lockfile column, and
	// CREATE TABLE IF NOT EXISTS cannot add one to a table that already exists.
	// This was an unconditional ALTER on every Open before the runner existed;
	// it is idempotent either way.
	{name: "tools_lockfile", fn: func(q sqlExec) error {
		return addColumnIfMissing(q, "tools", "lockfile", "TEXT NOT NULL DEFAULT '{}'")
	}},

	// 2 → 3: drop the `reflections` table. It recorded a reflection turn's text
	// with no agent id, which was survivable while exactly one session could
	// reflect and wrong as soon as any session can carry a reflect aspect. The
	// journal already holds every turn under its own agent_id, and the durable
	// product of a reflection is the `self/*` KV write the prompt asks for — not
	// the transcript, which nothing read back (docs/concept-consolidation.md C5).
	//
	// This is the first destructive step: rows written before the journal existed
	// have no equivalent elsewhere and are lost. That is the accepted cost of the
	// change, not an oversight.
	{name: "drop_reflections", fn: func(q sqlExec) error {
		_, err := q.Exec(`DROP TABLE IF EXISTS reflections`)
		return err
	}},

	// 3 → 4: drop `goals.subtree`. The parent/child edge is `parent_id`, written
	// by goal_create; `subtree` was a second, free-text copy of the same relation
	// that nothing in the code read, reaching the model only by riding along in
	// goal_get. Keeping it meant asking a language model to maintain a
	// denormalized index of a relation the schema already enforces — wrong at
	// some rate and unverifiable (docs/concept-consolidation.md C6).
	//
	// goal_get still returns a `subtree` key, now derived from parent_id, so
	// nothing the model sees changes shape.
	{name: "drop_goal_subtree", fn: func(q sqlExec) error {
		return dropColumnIfPresent(q, "goals", "subtree")
	}},
}

// dropColumnIfPresent removes a column only when it is there, so the step is
// safe against a database that never had it. SQLite has supported
// ALTER TABLE ... DROP COLUMN since 3.35.
func dropColumnIfPresent(q sqlExec, table, column string) error {
	has, err := hasColumnTx(q, table, column)
	if err != nil || !has {
		return err
	}
	_, err = q.Exec("ALTER TABLE " + table + " DROP COLUMN " + column)
	return err
}

// hasColumnTx reports whether table has column, using the passed handle so it
// participates in the migration's transaction.
func hasColumnTx(q sqlExec, table, column string) (bool, error) {
	rows, err := q.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return false, err
	}
	defer rows.Close() //nolint:errcheck
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, ctype      string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, rows.Close()
		}
	}
	return false, rows.Err()
}

// userVersion reads PRAGMA user_version. A database SQLite has just created
// reports 0.
//
// The query routes to the writer pool (isRead matches only SELECT), which is
// what callers during Open need: the reader pool is not open yet.
func userVersion(d db) (int, error) {
	var v int
	if err := d.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return 0, fmt.Errorf("read user_version: %w", err)
	}
	return v, nil
}

// migrate brings a database from version `from` up to schemaVersion, running
// each step in its own transaction together with the version bump that records
// it. A step and its bump therefore commit or roll back as one: an interrupted
// migration leaves the database at the last version that fully applied, never
// half-way through a step, so re-running Open resumes at the right place.
func migrate(d db, from int) error {
	target := schemaVersion()
	if from > target {
		return fmt.Errorf("database schema version %d is newer than this binary understands (%d); "+
			"upgrade nine rather than downgrading the database", from, target)
	}
	for v := from; v < target; v++ {
		step := migrations[v]
		if err := runMigration(d, v, step); err != nil {
			return fmt.Errorf("migrate %d→%d (%s): %w", v, v+1, step.name, err)
		}
	}
	return nil
}

// runMigration applies one step and its version bump atomically.
func runMigration(d db, from int, step migrationStep) error {
	tx, err := d.BeginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit succeeds

	if err := step.fn(tx); err != nil {
		return err
	}
	// PRAGMA user_version takes no bind parameter, so the value is formatted in.
	// from is loop-bound over the migrations slice, never caller input.
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", from+1)); err != nil {
		return fmt.Errorf("bump user_version: %w", err)
	}
	return tx.Commit()
}

// setUserVersion stamps a version outside the step runner. It is used for a
// freshly created database, which initSchema has already built at the current
// shape and which therefore must skip every step.
func setUserVersion(d db, v int) error {
	if _, err := d.Exec(fmt.Sprintf("PRAGMA user_version = %d", v)); err != nil {
		return fmt.Errorf("stamp user_version: %w", err)
	}
	return nil
}

// addColumnIfMissing adds column to table only when it is absent, so it is safe
// to run against a database that already has it. Needed for a column added to a
// table that predates it; CREATE TABLE IF NOT EXISTS is a no-op against an
// existing table.
func addColumnIfMissing(q sqlExec, table, column, def string) error {
	rows, err := q.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return err
	}
	defer rows.Close() //nolint:errcheck
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, ctype      string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return rows.Close()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = q.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + def)
	return err
}
