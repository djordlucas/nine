package runner

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"

	"nine/internal/memory"
)

// ErrNoPostgres signals that the eval database is unreachable. Callers turn this
// into a skip (CI without a database still builds and runs the non-DB tests).
var ErrNoPostgres = fmt.Errorf("eval postgres unreachable")

// baseDSN is the connection string for the eval database, mirroring
// internal/memory/memtest so evals reuse `make pg`'s standalone Postgres.
// Overridable via NINE_TEST_DATABASE_URL.
func baseDSN() string {
	if v := os.Getenv("NINE_TEST_DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://nine:nine@localhost:5433/nine?sslmode=disable"
}

// openIsolatedStore creates a fresh, uniquely-named schema on the eval database
// and returns a memory.Store bound to it, plus a cleanup that drops the schema.
// It is the non-testing.T twin of memtest.Open: each eval run (Track L repetition
// or Track R replay) gets its own schema so cases never see each other's state
// (docs/evals.md §5, "Isolation per run"). A malformed/unreachable database
// returns ErrNoPostgres so callers can skip rather than fail.
func openIsolatedStore(base string) (*memory.Store, func(), error) {
	if base == "" {
		base = baseDSN()
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		return nil, nil, err
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		return nil, nil, fmt.Errorf("%w: %v", ErrNoPostgres, err)
	}
	// pgvector's type lives in public; ensure it exists before per-schema DDL.
	if _, err := admin.Exec(`CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		admin.Close()
		return nil, nil, fmt.Errorf("create extension vector: %w", err)
	}

	buf := make([]byte, 8)
	rand.Read(buf) //nolint:errcheck
	schema := "eval_" + hex.EncodeToString(buf)
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		admin.Close()
		return nil, nil, fmt.Errorf("create schema: %w", err)
	}

	dsn, err := withSearchPath(base, schema)
	if err != nil {
		admin.Close()
		return nil, nil, err
	}
	store, err := memory.Open(dsn)
	if err != nil {
		admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) //nolint:errcheck
		admin.Close()
		return nil, nil, err
	}

	cleanup := func() {
		store.Close()                                    //nolint:errcheck
		admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) //nolint:errcheck
		admin.Close()                                    //nolint:errcheck
	}
	return store, cleanup, nil
}

// withSearchPath returns dsn with search_path pointed at schema (falling back to
// public so the pgvector type resolves), matching memtest.
func withSearchPath(dsn, schema string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("search_path", schema+",public")
	u.RawQuery = q.Encode()
	return u.String(), nil
}
