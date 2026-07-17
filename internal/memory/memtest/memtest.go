// Package memtest provides an isolated PostgreSQL-backed memory.Store for tests.
// Each Open call creates a fresh schema on the shared test database and drops it
// on cleanup, so tests are isolated without needing a database per test.
//
// The base connection string is taken from NINE_TEST_DATABASE_URL, falling back
// to the local docker-compose default (see docker-compose.yml). If the database
// is unreachable the test is skipped rather than failed, so the suite still runs
// on machines without Postgres.
package memtest

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"nine/internal/memory"
)

func baseDSN() string {
	if v := os.Getenv("NINE_TEST_DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://nine:nine@localhost:5433/nine?sslmode=disable"
}

// Open returns a memory.Store backed by a fresh, isolated schema on the test
// database. The schema is dropped and the store closed via t.Cleanup. It returns
// (store, nil) on success to preserve the `store, err := ...` call shape of the
// SQLite-era tests; it skips the test if Postgres is unreachable.
func Open(t *testing.T) (*memory.Store, error) {
	t.Helper()

	base := baseDSN()
	admin, err := sql.Open("pgx", base)
	if err != nil {
		return nil, err
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		t.Skipf("postgres unavailable (%v); set NINE_TEST_DATABASE_URL or run docker compose up -d", err)
	}

	// pgvector's type lives in public; ensure it exists before per-schema DDL.
	if _, err := admin.Exec(`CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		admin.Close()
		return nil, fmt.Errorf("create extension vector: %w", err)
	}

	buf := make([]byte, 8)
	rand.Read(buf) //nolint:errcheck
	schema := "test_" + hex.EncodeToString(buf)
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		admin.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}

	dsn, err := withSearchPath(base, schema)
	if err != nil {
		admin.Close()
		return nil, err
	}
	store, err := memory.Open(dsn)
	if err != nil {
		admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) //nolint:errcheck
		admin.Close()
		return nil, err
	}

	t.Cleanup(func() {
		store.Close()
		admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) //nolint:errcheck
		admin.Close()
	})
	return store, nil
}

// withSearchPath returns dsn with search_path set to the test schema (falling
// back to public so the pgvector type resolves).
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
