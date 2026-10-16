package sqldb

import (
	"context"
	"strings"
	"testing"
)

// The operator lists tables without SQLite's own or a virtual table's shadows,
// pages rows by rowid, and changes one row at a time.
func TestOperatorBrowsesAndEditsRows(t *testing.T) {
	d := open(t, Options{})
	ctx := context.Background()
	for _, q := range []string{
		"CREATE TABLE notes (id INTEGER PRIMARY KEY AUTOINCREMENT, body TEXT NOT NULL, n INTEGER DEFAULT 0)",
		"CREATE TABLE pairs (k TEXT PRIMARY KEY, v TEXT) WITHOUT ROWID",
		"CREATE VIEW bodies AS SELECT body FROM notes",
		"CREATE VIRTUAL TABLE search USING fts5(body)",
	} {
		if _, err := d.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	tables, err := d.Tables(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Table{}
	for _, tb := range tables {
		got[tb.Name] = tb
	}
	if len(got) != 4 || !got["notes"].RowID || got["pairs"].RowID || got["bodies"].Kind != "view" || got["search"].Kind != "virtual" {
		t.Fatalf("tables = %+v, want notes (rowid), pairs (none), the view and the virtual table only", tables)
	}
	if c := got["notes"].Columns; len(c) != 3 || c[0].PK != 1 || !c[1].NotNull || c[2].Default == nil {
		t.Errorf("notes columns = %+v", c)
	}

	for _, body := range []string{"a", "b", "c"} {
		if _, err := d.InsertRow(ctx, "notes", map[string]any{"body": body}); err != nil {
			t.Fatal(err)
		}
	}
	p, err := d.Rows(ctx, "notes", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if p.Total != 3 || len(p.Rows) != 1 || p.RowIDs[0] != 2 || p.Rows[0][1] != "b" || p.Columns[0] != "id" {
		t.Fatalf("page = %+v, want row 2 of 3", p)
	}

	r, err := d.UpdateRow(ctx, "notes", 2, map[string]any{"body": "B", "n": "7"})
	if err != nil || r.Changes != 1 || len(r.Writes) == 0 || r.Writes[0].Kind != "update" {
		t.Fatalf("update = %+v, %v", r, err)
	}
	if _, err := d.DeleteRow(ctx, "notes", 1); err != nil {
		t.Fatal(err)
	}
	p, _ = d.Rows(ctx, "notes", 0, 10)
	if len(p.Rows) != 2 || p.Rows[0][1] != "B" || p.Rows[0][2] != int64(7) {
		t.Fatalf("after update and delete = %+v", p.Rows)
	}

	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"unknown table", func() error { _, err := d.Rows(ctx, "sqlite_schema", 0, 1); return err }(), "table not found"},
		{"unknown column", func() error { _, err := d.InsertRow(ctx, "notes", map[string]any{"nope": 1}); return err }(), "no column nope"},
		{"no rowid", func() error { _, err := d.DeleteRow(ctx, "pairs", 1); return err }(), "has no rowid"},
	} {
		if tc.err == nil || !strings.Contains(tc.err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, tc.err, tc.want)
		}
	}
}

// Run reads with the read policy and writes with a program's; what neither
// allows is refused with the reason.
func TestRunReadsAndWrites(t *testing.T) {
	d := open(t, Options{})
	ctx := context.Background()
	r, err := d.Run(ctx, "CREATE TABLE t (x)")
	if err != nil || !r.Write {
		t.Fatalf("create = %+v, %v", r, err)
	}
	if r, err = d.Run(ctx, "INSERT INTO t VALUES (?), (?)", 1, 2); err != nil || r.Changes != 2 {
		t.Fatalf("insert = %+v, %v", r, err)
	}
	// A statement that writes no rows reports none, not the previous one's.
	if r, err = d.Run(ctx, "CREATE INDEX t_x ON t(x)"); err != nil || r.Changes != 0 || r.LastInsertID != 0 {
		t.Fatalf("create index after an insert = %+v, %v", r, err)
	}
	if r, err = d.Run(ctx, "SELECT x FROM t ORDER BY x"); err != nil || r.Write || len(r.Rows) != 2 {
		t.Fatalf("select = %+v, %v", r, err)
	}
	if _, err = d.Run(ctx, "ATTACH DATABASE ':memory:' AS o"); err == nil || !strings.Contains(err.Error(), "denied: attach") {
		t.Fatalf("attach: err = %v, want refused", err)
	}
}
