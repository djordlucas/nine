// Package memory provides a SQLite-backed store for all persistent nine data:
// key-value pairs, files, vectors, conversations, goals, notifications,
// workflows, the plugin registry, and the session event journal.
//
// The database is a single file — nine ships with no database server, so the
// daemon has nothing to wait for and nothing to provision.
package memory

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"nine/internal/workflow"
)

const (
	// writeDSN opens the one read-write connection. Each pragma earns its place:
	//
	//	busy_timeout(5000)   a lock wait rather than an error. In-process
	//	                     contention is already zero (the writer pool holds a
	//	                     single connection), so this exists for the second
	//	                     process: `nine trace` and `nine replay` open the same
	//	                     file while the daemon runs, and would far rather wait.
	//	journal_mode(WAL)    readers and the writer stop blocking each other.
	//	                     Without it the second-process CLI would stall the
	//	                     daemon. Persisted in the file header, so it is a no-op
	//	                     after the first Open.
	//	synchronous(NORMAL)  the correct pairing with WAL: durable across a process
	//	                     crash, with only the last commits at risk on power
	//	                     loss. FULL fsyncs every commit, and the event sink
	//	                     flushes every 100ms — a sustained 10 fsync/s for an
	//	                     observability journal is not a trade worth making.
	//	foreign_keys(ON)     per-connection, and off by default in SQLite. No
	//	                     foreign keys exist yet; this means the first one added
	//	                     is actually enforced.
	//
	// _txlock=immediate makes any future transaction take the write lock up
	// front. A deferred transaction that reads and then writes can fail with
	// SQLITE_BUSY_SNAPSHOT, which busy_timeout does not retry — it is an
	// unrecoverable snapshot conflict, not a lock wait. Taking the lock
	// immediately turns that into a plain, retriable wait.
	writeDSN = "file:%s?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(ON)" +
		"&_txlock=immediate"

	// readDSN opens the read-only pool. It deliberately omits journal_mode:
	// setting it writes the file header, which a read-only connection cannot do.
	// The writer has already persisted WAL mode.
	readDSN = "file:%s?mode=ro&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
)

// db routes each statement to the pool that can serve it. SQLite serializes
// writes, so every write goes through a single-connection pool — the wait
// becomes a fair queue in database/sql rather than a busy_timeout spin inside
// the driver — while readers get their own pool and run concurrently against
// the WAL snapshot.
//
// The routing key is the statement text, not the Go method: `UPDATE … RETURNING`
// arrives through Query but is a write. Anything that is not a plain SELECT is
// treated as a write, so a statement form nobody anticipated routes
// conservatively rather than failing against a read-only connection.
//
// (This replaces the placeholder-rewriting wrapper the Postgres version needed.
// `?` is SQLite's native placeholder, so every query string in the package —
// all of which were already written with `?` — passes through untouched.)
type db struct {
	w *sql.DB // writer: one connection, read-write
	r *sql.DB // readers: concurrent, mode=ro
}

// isRead reports whether query can be served by a read-only connection.
func isRead(query string) bool {
	q := strings.TrimLeft(query, " \t\r\n(")
	return len(q) >= 6 && strings.EqualFold(q[:6], "select")
}

func (d db) pool(query string) *sql.DB {
	if isRead(query) {
		return d.r
	}
	return d.w
}

// checkArgs panics if any argument is a time.Time.
//
// Timestamps are stored as fixed-width RFC3339 UTC microseconds (see
// timeLayout). The SQLite driver binds a time.Time using time.Time.String() by
// default — "2026-08-04 12:34:56.789 +0000 UTC" — whose 11th byte is a space
// where every stored value has a 'T'. Since SQLite compares TEXT bytewise, such
// a value is *less than every stored timestamp*: a predicate like
// `stored_at < ?` becomes permanently false and a retention sweep silently
// deletes nothing, forever, with no error anywhere.
//
// A panic is the right severity. It can only fire on a code path someone just
// wrote, it fires on that path's first test run, and the alternative is a bug
// that is invisible in production. Callers pass writeTime(t).
func checkArgs(args []any) {
	for i, a := range args {
		if _, bad := a.(time.Time); bad {
			panic(fmt.Sprintf("memory: argument %d is a time.Time; pass writeTime(t) "+
				"so it compares correctly against stored timestamps", i))
		}
	}
}

func (d db) Exec(query string, args ...any) (sql.Result, error) {
	checkArgs(args)
	return d.pool(query).Exec(query, args...)
}

func (d db) Query(query string, args ...any) (*sql.Rows, error) {
	checkArgs(args)
	return d.pool(query).Query(query, args...)
}

func (d db) QueryRow(query string, args ...any) *sql.Row {
	checkArgs(args)
	return d.pool(query).QueryRow(query, args...)
}

// BeginWrite starts a transaction on the writer pool, so a caller gets the
// writer (a transaction on the read-only pool would fail confusingly) and
// _txlock=immediate's safe locking semantics for free. The schema migration
// runner uses it to apply each step and its user_version bump atomically.
func (d db) BeginWrite() (*sql.Tx, error) { return d.w.Begin() }

// Store wraps the database and exposes typed methods for every domain.
type Store struct {
	db        db
	workflows *workflow.Service
}

// Open opens the SQLite database at path, creating the file and its parent
// directory if absent, applies the schema, and returns a ready-to-use Store.
// An unusable database is a startup error rather than a degraded mode — it
// holds primary state.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}

	w, err := sql.Open("sqlite", fmt.Sprintf(writeDSN, path))
	if err != nil {
		return nil, err
	}
	// One connection, held for the process lifetime. Pragmas are per-connection,
	// so churning connections would re-run them for nothing, and a second write
	// connection could only ever contend with the first.
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxIdleTime(0)
	w.SetConnMaxLifetime(0)
	if err := w.Ping(); err != nil {
		w.Close() //nolint:errcheck
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	d := db{w: w}
	if err := initSchema(d); err != nil {
		w.Close() //nolint:errcheck
		return nil, fmt.Errorf("init schema: %w", err)
	}

	// Readers open only after the writer has created the file and the schema:
	// mode=ro cannot create anything.
	r, err := sql.Open("sqlite", fmt.Sprintf(readDSN, path))
	if err != nil {
		w.Close() //nolint:errcheck
		return nil, err
	}
	r.SetMaxOpenConns(4)
	r.SetMaxIdleConns(4)
	d.r = r

	return &Store{
		db:        d,
		workflows: workflow.NewService(&sqlWorkflowRepo{db: d}),
	}, nil
}

// OpenReadOnly opens an existing database for reading only. It is what
// `nine trace` and `nine replay` use: those run in a second process while the
// daemon holds the same file, and under WAL a read-only connection takes a
// snapshot without ever blocking — or being blocked by — the daemon's writer.
//
// It skips initSchema entirely, so a stale CLI binary can never run DDL against
// a running daemon's database. Both pools point at the same read-only handle,
// so a write routed here fails with "attempt to write a readonly database",
// which is the correct answer.
func OpenReadOnly(path string) (*Store, error) {
	r, err := sql.Open("sqlite", fmt.Sprintf(readDSN, path))
	if err != nil {
		return nil, err
	}
	r.SetMaxOpenConns(2)
	r.SetMaxIdleConns(2)
	if err := r.Ping(); err != nil {
		r.Close() //nolint:errcheck
		return nil, fmt.Errorf("open %s for reading: %w (has the daemon run at least once?)", path, err)
	}
	d := db{w: r, r: r}
	return &Store{
		db:        d,
		workflows: workflow.NewService(&sqlWorkflowRepo{db: d}),
	}, nil
}

// Close closes both pools. PRAGMA optimize runs first: SQLite recommends it
// before closing a long-lived connection so the query planner's statistics
// survive the restart. It is best-effort — a failure there must not mask a
// shutdown.
func (s *Store) Close() error {
	s.db.w.Exec(`PRAGMA optimize`) //nolint:errcheck // best-effort
	rerr := s.db.r.Close()
	if werr := s.db.w.Close(); werr != nil {
		return werr
	}
	return rerr
}

func initSchema(d db) error {
	// Read the version before any DDL runs, so a database SQLite just created
	// (user_version 0, no tables) is distinguishable from one in the field.
	before, err := userVersion(d)
	if err != nil {
		return err
	}
	fresh := before == 0 && !hasTable(d, "conversations")

	stmts := []string{
		`CREATE TABLE IF NOT EXISTS kv (
			key        TEXT PRIMARY KEY,
			value      TEXT NOT NULL,
			updated_at TEXT NOT NULL DEFAULT ` + nowExpr + `
		)`,
		`CREATE TABLE IF NOT EXISTS files (
			path       TEXT PRIMARY KEY,
			content    TEXT NOT NULL,
			size       INTEGER NOT NULL,
			stored_at  TEXT NOT NULL DEFAULT ` + nowExpr + `
		)`,
		// files_fts indexes `files` in place: content='files' means the index
		// holds only postings, never a second copy of the text, which matters
		// because a spilled tool output can be megabytes. snippet() reads the
		// text back out of `files` by rowid, so a contentless index is not an
		// option either.
		//
		// `porter` is the parity choice for the Postgres 'english' configuration
		// this replaces: without a stemmer, a search for "databases" would stop
		// matching a file that says "database".
		`CREATE VIRTUAL TABLE IF NOT EXISTS files_fts USING fts5(
			content,
			content='files',
			content_rowid='rowid',
			tokenize='porter unicode61 remove_diacritics 2'
		)`,
		// These three triggers are the entire index-sync mechanism. Keeping them
		// in SQL rather than in FileStore/FileDeleteOlderThan means the bulk
		// retention DELETE — which never goes near any FTS-aware Go code — stays
		// consistent for free.
		//
		// The 'delete' command row is how an external-content FTS5 table is told
		// to retract postings: it must be handed the *old* text, because the
		// index cannot reconstruct it.
		`CREATE TRIGGER IF NOT EXISTS files_fts_ai AFTER INSERT ON files BEGIN
			INSERT INTO files_fts(rowid, content) VALUES (new.rowid, new.content);
		END`,
		`CREATE TRIGGER IF NOT EXISTS files_fts_ad AFTER DELETE ON files BEGIN
			INSERT INTO files_fts(files_fts, rowid, content) VALUES('delete', old.rowid, old.content);
		END`,
		`CREATE TRIGGER IF NOT EXISTS files_fts_au AFTER UPDATE ON files BEGIN
			INSERT INTO files_fts(files_fts, rowid, content) VALUES('delete', old.rowid, old.content);
			INSERT INTO files_fts(rowid, content)            VALUES (new.rowid, new.content);
		END`,
		// vectors.embedding holds packed little-endian float32 (see encodeVector).
		// Similarity is computed in Go: the Postgres version's index on
		// (namespace, dim) was a plain btree, never an ANN index, so ranking was
		// already a filtered sequential scan — that index is what makes the scan
		// selective, and it carries over unchanged.
		`CREATE TABLE IF NOT EXISTS vectors (
			id         TEXT PRIMARY KEY,
			namespace  TEXT NOT NULL,
			key        TEXT NOT NULL,
			embedding  BLOB NOT NULL,
			dim        INTEGER NOT NULL,
			stored_at  TEXT NOT NULL DEFAULT ` + nowExpr + `
		)`,
		`CREATE INDEX IF NOT EXISTS vectors_ns_dim_idx ON vectors (namespace, dim)`,
		`CREATE TABLE IF NOT EXISTS conversations (
			id         TEXT PRIMARY KEY,
			history    TEXT NOT NULL DEFAULT '[]',
			scratchpad TEXT NOT NULL DEFAULT '[]',
			status     TEXT NOT NULL DEFAULT 'active',
			created_at TEXT NOT NULL DEFAULT ` + nowExpr + `,
			updated_at TEXT NOT NULL DEFAULT ` + nowExpr + `
		)`,
		`CREATE TABLE IF NOT EXISTS goals (
			id          TEXT PRIMARY KEY,
			description TEXT NOT NULL,
			status      TEXT NOT NULL DEFAULT 'active',
			parent_id   TEXT,
			parent_type TEXT,
			created_at  TEXT NOT NULL DEFAULT ` + nowExpr + `,
			updated_at  TEXT NOT NULL DEFAULT ` + nowExpr + `
		)`,
		`CREATE TABLE IF NOT EXISTS notifications (
			id                TEXT PRIMARY KEY,
			conversation_id   TEXT,
			message           TEXT NOT NULL,
			requires_approval INTEGER NOT NULL DEFAULT 0,
			delivered         INTEGER NOT NULL DEFAULT 0,
			created_at        TEXT NOT NULL DEFAULT ` + nowExpr + `
		)`,
		`CREATE TABLE IF NOT EXISTS user_notifications (
			id         TEXT PRIMARY KEY,
			agent_id   TEXT NOT NULL DEFAULT '',
			message    TEXT NOT NULL,
			seen       INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL DEFAULT ` + nowExpr + `
		)`,
		`CREATE TABLE IF NOT EXISTS skills (
			name        TEXT PRIMARY KEY,
			description TEXT NOT NULL DEFAULT '',
			tags        TEXT NOT NULL DEFAULT '[]',
			content     TEXT NOT NULL DEFAULT '',
			source      TEXT NOT NULL DEFAULT 'agent',
			updated_at  TEXT NOT NULL DEFAULT ` + nowExpr + `
		)`,
		// Generated sandboxed tools (spec/contracts/toolvm.md R-TVM.14): code the
		// agent wrote, as store state. `capabilities` is the tool's DECLARATION,
		// never a grant — grants live in nine.toml and are the operator's.
		// `source` is the BUNDLED js (external npm deps inlined at write time,
		// §4.4); `lockfile` is the exact third-party code that bundle carries.
		`CREATE TABLE IF NOT EXISTS tools (
			name           TEXT PRIMARY KEY,
			description    TEXT NOT NULL DEFAULT '',
			input_schema   TEXT NOT NULL DEFAULT '{}',
			source         TEXT NOT NULL DEFAULT '',
			capabilities   TEXT NOT NULL DEFAULT '{}',
			lockfile       TEXT NOT NULL DEFAULT '{}',
			created_at     TEXT NOT NULL DEFAULT ` + nowExpr + `,
			updated_at     TEXT NOT NULL DEFAULT ` + nowExpr + `,
			last_called_at TEXT NOT NULL DEFAULT '',
			call_count     INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS workflows (
			id         TEXT PRIMARY KEY,
			name       TEXT NOT NULL,
			status     TEXT NOT NULL DEFAULT 'active',
			agent_id   TEXT NOT NULL DEFAULT '',
			steps      TEXT NOT NULL DEFAULT '[]',
			created_at TEXT NOT NULL DEFAULT ` + nowExpr + `,
			updated_at TEXT NOT NULL DEFAULT ` + nowExpr + `
		)`,
		`CREATE TABLE IF NOT EXISTS session_plans (
			id         TEXT PRIMARY KEY,
			status     TEXT NOT NULL DEFAULT 'active',
			aspects    TEXT NOT NULL DEFAULT '[]',
			created_at TEXT NOT NULL DEFAULT ` + nowExpr + `,
			updated_at TEXT NOT NULL DEFAULT ` + nowExpr + `
		)`,
		`CREATE TABLE IF NOT EXISTS human_requests (
			id          TEXT PRIMARY KEY,
			agent_id    TEXT NOT NULL,
			question    TEXT NOT NULL,
			options     TEXT,
			status      TEXT NOT NULL DEFAULT 'pending',
			answer      TEXT,
			created_at  TEXT NOT NULL,
			answered_at TEXT,
			expires_at  TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS interactive_sessions (
			id TEXT PRIMARY KEY
		)`,
		// session_events: append-only execution journal (docs/event-log.md §6).
		//
		// AUTOINCREMENT is required, not stylistic. A plain INTEGER PRIMARY KEY
		// is a rowid alias, and SQLite assigns max(rowid)+1 — so it *reuses*
		// values after a delete. SessionEventsScrub deletes aggressively and can
		// remove the row holding the maximum seq. Meanwhile event_cursors stores
		// absolute seq values durably and SessionEventsAfter filters `seq > ?`.
		// Without AUTOINCREMENT: scrub removes seq 1000, a subscriber's persisted
		// cursor is 1000, the next append is assigned 1000 again, `seq > 1000` is
		// false, and that event is never delivered to that subscriber — silently,
		// forever. AUTOINCREMENT keeps a high-water mark in sqlite_sequence and
		// guarantees strictly increasing, never-reused values, which is the
		// contract the cursor protocol was written against.
		`CREATE TABLE IF NOT EXISTS session_events (
			seq            INTEGER PRIMARY KEY AUTOINCREMENT,
			agent_id       TEXT NOT NULL,
			turn           INTEGER NOT NULL,
			span_id        TEXT NOT NULL,
			parent_span_id TEXT,
			type           TEXT NOT NULL,
			ts             TEXT NOT NULL DEFAULT ` + nowExpr + `,
			payload        TEXT NOT NULL DEFAULT '{}'
		)`,
		`CREATE INDEX IF NOT EXISTS session_events_agent ON session_events (agent_id, seq)`,
		`CREATE INDEX IF NOT EXISTS session_events_type ON session_events (type)`,
		// event_cursors: each journal subscriber's durable position, so a
		// subscriber resumes from where it left off after a restart
		// (docs/reactive-events.md §3).
		`CREATE TABLE IF NOT EXISTS event_cursors (
			subscriber_id TEXT PRIMARY KEY,
			seq           INTEGER NOT NULL DEFAULT 0
		)`,
		// related_sessions: a derived index maintained by the related-session
		// subscriber (docs/reactive-events.md §4) — a link from a session to a
		// topically-similar prior session, surfaced on later turns (pull, not push).
		`CREATE TABLE IF NOT EXISTS related_sessions (
			agent_id         TEXT NOT NULL,
			related_agent_id TEXT NOT NULL,
			score            REAL NOT NULL,
			updated_at       TEXT NOT NULL DEFAULT ` + nowExpr + `,
			PRIMARY KEY (agent_id, related_agent_id)
		)`,
		// plugin_jobs: the daemon-side registry of long-running plugin work
		// (docs/plugin-capabilities.md §5). The model sees `handle`; the daemon
		// polls (plugin, plugin_job_id) and notifies owner_id on completion. A
		// large result goes to the file store (spill_path), not the row.
		`CREATE TABLE IF NOT EXISTS plugin_jobs (
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
			created_at    TEXT NOT NULL DEFAULT ` + nowExpr + `,
			updated_at    TEXT NOT NULL DEFAULT ` + nowExpr + `,
			finished_at   TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS plugin_jobs_owner ON plugin_jobs (owner_id)`,
		`CREATE INDEX IF NOT EXISTS plugin_jobs_state ON plugin_jobs (state)`,
	}
	for _, s := range stmts {
		if _, err := d.Exec(s); err != nil {
			return fmt.Errorf("create schema: %w", err)
		}
	}

	// CREATE TABLE IF NOT EXISTS above handles new tables and indexes and nothing
	// else. Everything it cannot express — a column added to an existing table, a
	// backfill, an FTS rebuild after a tokenizer change — is a numbered step in
	// migrate.go, applied here.
	//
	// A fresh database is stamped at the current version without running any
	// step: the statements above already built the current shape, and a step that
	// renames or rewrites a column would fail against it.
	if fresh {
		return setUserVersion(d, schemaVersion())
	}
	return migrate(d, before)
}

// hasTable reports whether a user table of this name exists. Used only to tell a
// database SQLite has just created from one that predates version stamping;
// both report user_version 0.
//
// It goes to d.w directly rather than through d.QueryRow. This runs inside
// initSchema, which Open calls *before* it opens the reader pool — so d.r is
// still nil, and the pool router sends anything starting with SELECT there.
func hasTable(d db, name string) bool {
	var n int
	err := d.w.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name,
	).Scan(&n)
	return err == nil && n > 0
}
