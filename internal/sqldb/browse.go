package sqldb

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// The operator's view of the database (adr/web-ui.md §10): its tables, their
// rows a page at a time, a row inserted, changed or deleted, and one statement
// run as written. Every statement still runs under a policy: reads under the
// read policy, writes under a program's, so the operator reaches what a
// process's program reaches and nothing of SQLite's own.

// Column is one column of a table, as PRAGMA table_info reports it.
type Column struct {
	Name    string  `json:"name"`
	Type    string  `json:"type"`
	NotNull bool    `json:"not_null"`
	Default *string `json:"default,omitempty"`
	// PK is the column's position in the primary key, from 1; 0 when it is not
	// part of it.
	PK int `json:"pk"`
}

// Table is one table or view of the database.
type Table struct {
	Name string `json:"name"`
	// Kind is "table", "view" or "virtual".
	Kind    string   `json:"kind"`
	Columns []Column `json:"columns"`
	Rows    int64    `json:"rows"`
	// RowID says rows are addressed by rowid, so they can be changed one at a
	// time. Views and WITHOUT ROWID tables have none.
	RowID bool `json:"rowid"`
}

// Page is a page of a table's rows, in rowid order where there is one.
type Page struct {
	Table   string   `json:"table"`
	Columns []string `json:"columns"`
	// RowIDs are the rows' rowids, in step with Rows; empty when the table
	// has none.
	RowIDs []int64 `json:"rowids,omitempty"`
	Rows   [][]any `json:"rows"`
	Total  int64   `json:"total"`
	Offset int     `json:"offset"`
}

// RunResult is what one statement did: a read's columns and rows, or a
// write's changes. Writes summarize the write for the journal.
type RunResult struct {
	Result
	Write        bool    `json:"write"`
	Changes      int64   `json:"changes"`
	LastInsertID int64   `json:"last_insert_rowid,omitempty"`
	Writes       []Write `json:"-"`
}

// MaxPage bounds a page of rows.
const MaxPage = 500

// Tables lists the database's tables and views, without SQLite's own and
// without the shadow tables a virtual table keeps its index in.
func (d *DB) Tables(ctx context.Context) ([]Table, error) {
	list, err := d.Query(ctx, "PRAGMA table_list")
	if err != nil {
		return nil, err
	}
	col := index(list.Columns)
	out := []Table{}
	for _, r := range list.Rows {
		name, _ := r[col["name"]].(string)
		kind, _ := r[col["type"]].(string)
		schema, _ := r[col["schema"]].(string)
		if schema != "main" || !isOwn(name) || kind == "shadow" {
			continue
		}
		t := Table{Name: name, Kind: kind, RowID: kind == "table" && !truthy(r[col["wr"]])}
		if t.Columns, err = d.columns(ctx, name); err != nil {
			return nil, err
		}
		n, err := d.Query(ctx, "SELECT count(*) FROM "+quote(name))
		if err != nil {
			return nil, err
		}
		if len(n.Rows) == 1 {
			t.Rows, _ = n.Rows[0][0].(int64)
		}
		out = append(out, t)
	}
	return out, nil
}

// Rows returns up to limit rows of a table from offset.
func (d *DB) Rows(ctx context.Context, table string, offset, limit int) (Page, error) {
	t, err := d.table(ctx, table)
	if err != nil {
		return Page{}, err
	}
	if limit <= 0 || limit > MaxPage {
		limit = MaxPage
	}
	offset = max(offset, 0)
	sel, order := "*", ""
	if t.RowID {
		sel, order = "_rowid_, *", " ORDER BY _rowid_"
	}
	res, err := d.Query(ctx, fmt.Sprintf("SELECT %s FROM %s%s LIMIT ? OFFSET ?", sel, quote(t.Name), order), limit, offset)
	if err != nil {
		return Page{}, err
	}
	p := Page{Table: t.Name, Columns: res.Columns, Rows: res.Rows, Total: t.Rows, Offset: offset}
	if t.RowID {
		p.Columns = res.Columns[1:]
		p.RowIDs = make([]int64, len(res.Rows))
		for i, r := range res.Rows {
			p.RowIDs[i], _ = r[0].(int64)
			p.Rows[i] = r[1:]
		}
	}
	return p, nil
}

// InsertRow adds a row; values not given take the column's default.
func (d *DB) InsertRow(ctx context.Context, table string, values map[string]any) (RunResult, error) {
	t, err := d.table(ctx, table)
	if err != nil {
		return RunResult{}, err
	}
	cols, args, err := assignments(t, values)
	if err != nil {
		return RunResult{}, err
	}
	q := "INSERT INTO " + quote(t.Name) + " DEFAULT VALUES"
	if len(cols) > 0 {
		q = fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", quote(t.Name), strings.Join(cols, ", "),
			strings.TrimSuffix(strings.Repeat("?, ", len(cols)), ", "))
	}
	return d.write(ctx, q, args)
}

// UpdateRow sets the given columns of the row with rowid.
func (d *DB) UpdateRow(ctx context.Context, table string, rowid int64, values map[string]any) (RunResult, error) {
	t, err := d.rowTable(ctx, table)
	if err != nil {
		return RunResult{}, err
	}
	cols, args, err := assignments(t, values)
	if err != nil {
		return RunResult{}, err
	}
	if len(cols) == 0 {
		return RunResult{}, errors.New("no values to set")
	}
	for i, c := range cols {
		cols[i] = c + " = ?"
	}
	q := fmt.Sprintf("UPDATE %s SET %s WHERE _rowid_ = ?", quote(t.Name), strings.Join(cols, ", "))
	return d.write(ctx, q, append(args, rowid))
}

// DeleteRow deletes the row with rowid.
func (d *DB) DeleteRow(ctx context.Context, table string, rowid int64) (RunResult, error) {
	t, err := d.rowTable(ctx, table)
	if err != nil {
		return RunResult{}, err
	}
	return d.write(ctx, "DELETE FROM "+quote(t.Name)+" WHERE _rowid_ = ?", []any{rowid})
}

// Run runs one statement as written: a read returns its rows, anything else
// runs under a program's policy and returns what it changed.
func (d *DB) Run(ctx context.Context, q string, args ...any) (RunResult, error) {
	res, err := d.Query(ctx, q, args...)
	if err == nil {
		return RunResult{Result: res}, nil
	}
	var ro readOnlyError
	if !errors.As(err, &ro) {
		return RunResult{}, err
	}
	return d.write(ctx, q, args)
}

func (d *DB) write(ctx context.Context, q string, args []any) (RunResult, error) {
	r, err := d.Exec(ctx, q, args...)
	return RunResult{Result: Result{Columns: []string{}, Rows: [][]any{}}, Write: true,
		Changes: r.Changes, LastInsertID: r.LastInsertID, Writes: r.Writes}, err
}

// table finds a table or view by its exact name, so a name never reaches a
// statement unless the database holds it.
func (d *DB) table(ctx context.Context, name string) (Table, error) {
	tables, err := d.Tables(ctx)
	if err != nil {
		return Table{}, err
	}
	for _, t := range tables {
		if t.Name == name {
			return t, nil
		}
	}
	return Table{}, fmt.Errorf("table not found: %s", name)
}

func (d *DB) rowTable(ctx context.Context, name string) (Table, error) {
	t, err := d.table(ctx, name)
	if err == nil && !t.RowID {
		err = fmt.Errorf("%s has no rowid (a view or a WITHOUT ROWID table): change it with a statement", name)
	}
	return t, err
}

func (d *DB) columns(ctx context.Context, table string) ([]Column, error) {
	// table names a table PRAGMA table_list reported, so quoting it is enough.
	res, err := d.Query(ctx, "PRAGMA table_info("+quote(table)+")")
	if err != nil {
		return nil, err
	}
	col := index(res.Columns)
	out := make([]Column, 0, len(res.Rows))
	for _, r := range res.Rows {
		c := Column{}
		c.Name, _ = r[col["name"]].(string)
		c.Type, _ = r[col["type"]].(string)
		c.NotNull = truthy(r[col["notnull"]])
		if s, ok := r[col["dflt_value"]].(string); ok {
			c.Default = &s
		}
		pk, _ := r[col["pk"]].(int64)
		c.PK = int(pk)
		out = append(out, c)
	}
	return out, nil
}

// assignments checks every value names a column of t and returns the quoted
// columns with their arguments, in column order.
func assignments(t Table, values map[string]any) ([]string, []any, error) {
	known := map[string]bool{}
	var cols []string
	var args []any
	for _, c := range t.Columns {
		known[c.Name] = true
		if v, ok := values[c.Name]; ok {
			cols = append(cols, quote(c.Name))
			args = append(args, v)
		}
	}
	for name := range values {
		if !known[name] {
			return nil, nil, fmt.Errorf("%s has no column %s", t.Name, name)
		}
	}
	return cols, args, nil
}

func quote(ident string) string { return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"` }

func index(cols []string) map[string]int {
	m := make(map[string]int, len(cols))
	for i, c := range cols {
		m[c] = i
	}
	return m
}

func truthy(v any) bool {
	n, ok := v.(int64)
	return ok && n != 0
}
