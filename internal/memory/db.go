// Package memory provides a PostgreSQL-backed store for all persistent nine
// data: key-value pairs, files, vectors, conversations, goals, notifications,
// reflections, workflows, the plugin registry, and the session event journal.
package memory

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"

	"nine/internal/workflow"
)

// db wraps *sql.DB and rewrites SQLite-style `?` placeholders to Postgres
// `$N` placeholders on every call, so query strings elsewhere in the package
// stay driver-agnostic. Exec/Query/QueryRow are overridden; Close, Ping, Begin,
// and the pool setters are promoted from the embedded *sql.DB.
type db struct {
	*sql.DB
}

// rebind rewrites each `?` in query to a positional `$1`, `$2`, … placeholder.
// None of the package's queries embed a literal `?`, so a straight scan is safe.
func rebind(query string) string {
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(query[i])
	}
	return b.String()
}

func (d db) Exec(query string, args ...any) (sql.Result, error) {
	return d.DB.Exec(rebind(query), args...)
}

func (d db) Query(query string, args ...any) (*sql.Rows, error) {
	return d.DB.Query(rebind(query), args...)
}

func (d db) QueryRow(query string, args ...any) *sql.Row {
	return d.DB.QueryRow(rebind(query), args...)
}

// Store wraps a PostgreSQL database and exposes typed methods for every domain.
type Store struct {
	db        db
	workflows *workflow.Service
}

// Open connects to the PostgreSQL database at dsn, verifies reachability
// (fail-fast — Postgres holds primary state, so an unavailable database is a
// startup error, not a degraded mode), applies the schema, and returns a
// ready-to-use Store.
func Open(dsn string) (*Store, error) {
	sqldb, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	if err := sqldb.Ping(); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}

	d := db{sqldb}
	if err := initSchema(d); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return &Store{
		db:        d,
		workflows: workflow.NewService(&sqlWorkflowRepo{db: d}),
	}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

func initSchema(d db) error {
	stmts := []string{
		`CREATE EXTENSION IF NOT EXISTS vector`,
		`CREATE TABLE IF NOT EXISTS kv (
			key        TEXT PRIMARY KEY,
			value      TEXT NOT NULL,
			updated_at TIMESTAMPTZ DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS files (
			path       TEXT PRIMARY KEY,
			content    TEXT NOT NULL,
			size       INTEGER NOT NULL,
			stored_at  TIMESTAMPTZ DEFAULT now(),
			search_tsv tsvector GENERATED ALWAYS AS (to_tsvector('english', coalesce(content, ''))) STORED
		)`,
		`CREATE INDEX IF NOT EXISTS files_search_idx ON files USING GIN (search_tsv)`,
		`CREATE TABLE IF NOT EXISTS vectors (
			id         TEXT PRIMARY KEY,
			namespace  TEXT NOT NULL,
			key        TEXT NOT NULL,
			embedding  vector NOT NULL,
			dim        INTEGER NOT NULL,
			stored_at  TIMESTAMPTZ DEFAULT now()
		)`,
		`CREATE INDEX IF NOT EXISTS vectors_ns_dim_idx ON vectors (namespace, dim)`,
		`CREATE TABLE IF NOT EXISTS conversations (
			id         TEXT PRIMARY KEY,
			history    TEXT NOT NULL DEFAULT '[]',
			scratchpad TEXT NOT NULL DEFAULT '[]',
			status     TEXT NOT NULL DEFAULT 'active',
			created_at TIMESTAMPTZ DEFAULT now(),
			updated_at TIMESTAMPTZ DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS goals (
			id          TEXT PRIMARY KEY,
			description TEXT NOT NULL,
			status      TEXT NOT NULL DEFAULT 'active',
			parent_id   TEXT,
			parent_type TEXT,
			subtree     TEXT NOT NULL DEFAULT '[]',
			created_at  TIMESTAMPTZ DEFAULT now(),
			updated_at  TIMESTAMPTZ DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS notifications (
			id                TEXT PRIMARY KEY,
			conversation_id   TEXT,
			message           TEXT NOT NULL,
			requires_approval INTEGER NOT NULL DEFAULT 0,
			delivered         INTEGER NOT NULL DEFAULT 0,
			created_at        TIMESTAMPTZ DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS user_notifications (
			id         TEXT PRIMARY KEY,
			agent_id   TEXT NOT NULL DEFAULT '',
			message    TEXT NOT NULL,
			seen       INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMPTZ DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS skills (
			name        TEXT PRIMARY KEY,
			description TEXT NOT NULL DEFAULT '',
			tags        TEXT NOT NULL DEFAULT '[]',
			content     TEXT NOT NULL DEFAULT '',
			source      TEXT NOT NULL DEFAULT 'agent',
			updated_at  TIMESTAMPTZ DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS reflections (
			id       TEXT PRIMARY KEY,
			ran_at   TIMESTAMPTZ DEFAULT now(),
			summary  TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS workflows (
			id         TEXT PRIMARY KEY,
			name       TEXT NOT NULL,
			status     TEXT NOT NULL DEFAULT 'active',
			agent_id   TEXT NOT NULL DEFAULT '',
			steps      TEXT NOT NULL DEFAULT '[]',
			created_at TIMESTAMPTZ DEFAULT now(),
			updated_at TIMESTAMPTZ DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS session_plans (
			id         TEXT PRIMARY KEY,
			status     TEXT NOT NULL DEFAULT 'active',
			stages     TEXT NOT NULL DEFAULT '[]',
			created_at TIMESTAMPTZ DEFAULT now(),
			updated_at TIMESTAMPTZ DEFAULT now()
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
		`CREATE TABLE IF NOT EXISTS session_events (
			seq            BIGSERIAL PRIMARY KEY,
			agent_id       TEXT NOT NULL,
			turn           INTEGER NOT NULL,
			span_id        TEXT NOT NULL,
			parent_span_id TEXT,
			type           TEXT NOT NULL,
			ts             TIMESTAMPTZ NOT NULL DEFAULT now(),
			payload        JSONB NOT NULL DEFAULT '{}'
		)`,
		`CREATE INDEX IF NOT EXISTS session_events_agent ON session_events (agent_id, seq)`,
		`CREATE INDEX IF NOT EXISTS session_events_type ON session_events (type)`,
		// event_cursors: each journal subscriber's durable position, so a
		// subscriber resumes from where it left off after a restart
		// (docs/reactive-events.md §3).
		`CREATE TABLE IF NOT EXISTS event_cursors (
			subscriber_id TEXT PRIMARY KEY,
			seq           BIGINT NOT NULL DEFAULT 0
		)`,
		// related_sessions: a derived index maintained by the related-session
		// subscriber (docs/reactive-events.md §4) — a link from a session to a
		// topically-similar prior session, surfaced on later turns (pull, not push).
		`CREATE TABLE IF NOT EXISTS related_sessions (
			agent_id         TEXT NOT NULL,
			related_agent_id TEXT NOT NULL,
			score            REAL NOT NULL,
			updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (agent_id, related_agent_id)
		)`,
	}
	for _, s := range stmts {
		if _, err := d.Exec(s); err != nil {
			return fmt.Errorf("create schema: %w", err)
		}
	}
	return nil
}
