package sqldb

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T, o Options) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "database.sqlite"), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// The shim installs a working authorizer: a denied statement is refused with
// the action named, and an allowed one runs.
func TestAuthorizerShim(t *testing.T) {
	d := open(t, Options{})
	ctx := context.Background()
	if _, err := d.Exec(ctx, "CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT)"); err != nil {
		t.Fatal(err)
	}
	_, err := d.Exec(ctx, "ATTACH DATABASE ':memory:' AS other")
	if err == nil || !strings.Contains(err.Error(), "denied: attach") {
		t.Fatalf("ATTACH: err = %v, want a denial naming attach", err)
	}
}

// Programs may read, write and change the schema of their own tables; every
// write is summarized by kind and table.
func TestProgramsWriteAndAreSummarized(t *testing.T) {
	d := open(t, Options{})
	ctx := context.Background()
	steps := []struct {
		sql  string
		want []Write
	}{
		{"CREATE TABLE notes (id INTEGER PRIMARY KEY AUTOINCREMENT, body TEXT)", []Write{{"create table", "notes"}}},
		{"CREATE INDEX notes_body ON notes(body)", []Write{{"create index", "notes"}}},
		{"INSERT INTO notes(body) VALUES ('a'), ('b')", []Write{{"insert into", "notes"}}},
		{"UPDATE notes SET body = 'c' WHERE id = 1", []Write{{"update", "notes"}}},
		{"ALTER TABLE notes ADD COLUMN at TEXT", []Write{{"alter table", "notes"}}},
		{"CREATE VIRTUAL TABLE search USING fts5(body)", nil},
		{"DELETE FROM notes WHERE id = 2", []Write{{"delete from", "notes"}}},
	}
	for _, s := range steps {
		r, err := d.Exec(ctx, s.sql)
		if err != nil {
			t.Fatalf("%s: %v", s.sql, err)
		}
		if s.want != nil && (len(r.Writes) == 0 || r.Writes[0] != s.want[0]) {
			t.Errorf("%s: writes = %v, want %v", s.sql, r.Writes, s.want)
		}
	}
	res, err := d.Query(ctx, "SELECT id, body FROM notes ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || res.Rows[0][1] != "c" {
		t.Errorf("rows = %v", res.Rows)
	}
	if _, err := d.Query(ctx, "PRAGMA table_info(notes)"); err != nil {
		t.Errorf("introspection: %v", err)
	}
}

// Denied statements are refused with a reason (phase 5's acceptance).
func TestDeniedStatementsAreRefusedWithAReason(t *testing.T) {
	d := open(t, Options{})
	ctx := context.Background()
	if _, err := d.Exec(ctx, "CREATE TABLE notes (body TEXT)"); err != nil {
		t.Fatal(err)
	}
	for sql, want := range map[string]string{
		"ATTACH DATABASE '/tmp/x.db' AS x":      "denied: attach",
		"BEGIN":                                 "denied: transaction",
		"SAVEPOINT s":                           "denied: savepoint",
		"PRAGMA writable_schema = ON":           "denied: pragma writable_schema",
		"PRAGMA max_page_count = 1000000":       "denied: pragma max_page_count",
		"CREATE TEMP TABLE t (x)":               "denied: creating temp objects",
		"SELECT load_extension('x')":            "denied: function load_extension",
		"UPDATE sqlite_master SET sql = ''":     "may not be modified",
		"VACUUM INTO '/tmp/copy.db'":            "denied",
		"CREATE VIRTUAL TABLE v USING rtree(i)": "denied: create virtual table v",
	} {
		_, err := d.Exec(ctx, sql)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", sql, err, want)
		}
	}
	// sql_query's policy is read-only, and a write refused there points to
	// exec() rather than claiming writes are allowed.
	for _, sql := range []string{"INSERT INTO notes VALUES ('x')", "CREATE TABLE t2 (x)", "DROP TABLE notes"} {
		_, err := d.Query(ctx, sql)
		if err == nil || !strings.Contains(err.Error(), "only read") || !strings.Contains(err.Error(), "exec()") {
			t.Errorf("read-only %s: err = %v, want the read-only refusal pointing to exec()", sql, err)
		}
	}
	// What no policy allows keeps the general reason, through query() too.
	if _, err := d.Query(ctx, "ATTACH DATABASE ':memory:' AS x"); err == nil ||
		!strings.Contains(err.Error(), "denied: attach is not allowed") || strings.Contains(err.Error(), "exec()") {
		t.Errorf("ATTACH through query: err = %v", err)
	}
}

// The size quota and the statement time limit are enforced, with reasons.
func TestQuotas(t *testing.T) {
	d := open(t, Options{MaxMB: 1, Timeout: time.Minute})
	ctx := context.Background()
	if _, err := d.Exec(ctx, "CREATE TABLE blobs (b BLOB)"); err != nil {
		t.Fatal(err)
	}
	_, err := d.Exec(ctx, "INSERT INTO blobs SELECT randomblob(100000) FROM (WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 50) SELECT i FROM n)")
	if err == nil || !strings.Contains(err.Error(), "size limit of 1 MB") {
		t.Errorf("over the size quota: err = %v", err)
	}
	d = open(t, Options{Timeout: 200 * time.Millisecond})
	_, err = d.Query(ctx, "WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n) SELECT count(*) FROM n")
	if err == nil || !strings.Contains(err.Error(), "longer than") {
		t.Errorf("over the time limit: err = %v", err)
	}
}
