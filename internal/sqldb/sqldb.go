// Package sqldb is the database: one SQLite file per instance, beside the store
// and never inside it, that Nine's processes write through `nine:sql` and its
// conversations read through `sql_query` (adr/process-sessions.md §8). Every
// statement runs under an authorizer policy (policy.go); writes are summarized,
// kind and table, for the journal.
package sqldb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Defaults for [processes] database quotas.
const (
	DefaultMaxMB   = 100
	DefaultTimeout = 5 * time.Second
	// MaxRows bounds what one query returns.
	MaxRows = 1000
	// pageSize is SQLite's default page size, which max_page_count counts in.
	pageSize = 4096
)

// Options are the database's quotas.
type Options struct {
	MaxMB   int
	Timeout time.Duration
}

// DB is the database.
type DB struct {
	db      *sql.DB
	opts    Options
	mu      sync.Mutex // one statement at a time: the authorizer is per connection
	maxRows int
}

// Open opens or creates the database at path.
func Open(path string, o Options) (*DB, error) {
	if o.MaxMB <= 0 {
		o.MaxMB = DefaultMaxMB
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	pages := int64(o.MaxMB) * 1024 * 1024 / pageSize
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"+
		"&_pragma=trusted_schema(OFF)&_pragma=max_page_count(%d)", path, pages)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqldb: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqldb: open %s: %w", path, err)
	}
	// Prove the authorizer works on this driver before anything relies on it.
	if err := withRaw(context.Background(), db, func(rawConn) error { return nil }); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &DB{db: db, opts: o, maxRows: MaxRows}, nil
}

// Close closes the database.
func (d *DB) Close() error { return d.db.Close() }

// Result is a query's columns and rows.
type Result struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	Truncated bool     `json:"truncated,omitempty"`
}

// ExecResult is what a write did.
type ExecResult struct {
	Changes      int64   `json:"changes"`
	LastInsertID int64   `json:"last_insert_rowid"`
	Writes       []Write `json:"-"`
}

// Query runs a read-only statement: sql_query's policy.
func (d *DB) Query(ctx context.Context, q string, args ...any) (Result, error) {
	return d.query(ctx, readPolicy, q, args)
}

// Exec runs statements under a program's policy: reads, writes and DDL on
// Nine's tables.
func (d *DB) Exec(ctx context.Context, q string, args ...any) (ExecResult, error) {
	var writes []Write
	var out ExecResult
	_, err := d.run(ctx, writePolicy, &writes, func(ctx context.Context, c *sql.Conn) (Result, error) {
		r, err := c.ExecContext(ctx, q, args...)
		if err != nil {
			return Result{}, err
		}
		out.Changes, _ = r.RowsAffected()
		out.LastInsertID, _ = r.LastInsertId()
		return Result{}, nil
	})
	out.Writes = writes
	return out, err
}

func (d *DB) query(ctx context.Context, policy authorizer, q string, args []any) (Result, error) {
	return d.run(ctx, policy, nil, func(ctx context.Context, c *sql.Conn) (Result, error) {
		return scan(ctx, c, q, args, d.maxRows)
	})
}

// run executes fn on the connection with policy installed and the statement
// time bounded, and turns SQLite's errors into reasons.
func (d *DB) run(ctx context.Context, policy authorizer, writes *[]Write,
	fn func(context.Context, *sql.Conn) (Result, error)) (Result, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, d.opts.Timeout)
	defer cancel()
	c, err := d.db.Conn(ctx)
	if err != nil {
		return Result{}, d.reason(ctx, nil, err)
	}
	defer c.Close() //nolint:errcheck // returning the connection to the pool
	st := &authState{policy: policy, readOnly: writes == nil}
	var raw rawConn
	if err := c.Raw(func(dc any) error {
		var err error
		raw, err = rawOf(dc.(driver.Conn))
		return err
	}); err != nil {
		return Result{}, err
	}
	uninstall := raw.install(st)
	res, err := fn(ctx, c)
	uninstall()
	if writes != nil {
		*writes = st.writes
	}
	if err != nil {
		return Result{}, d.reason(ctx, st, err)
	}
	return res, nil
}

// reason turns a statement's failure into one a model or program can act on.
func (d *DB) reason(ctx context.Context, st *authState, err error) error {
	msg := err.Error()
	switch {
	case st != nil && st.writable:
		return fmt.Errorf("denied: %s — this is a read: query() and sql_query only read. "+
			"Write with exec() from nine:sql, in a process's program", st.denied)
	case st != nil && st.denied != "":
		return fmt.Errorf("denied: %s is not allowed in the database (only reads, writes and schema changes "+
			"on your own tables; no ATTACH, transactions, temp objects or pragmas beyond table introspection)", st.denied)
	case errors.Is(ctx.Err(), context.DeadlineExceeded) || strings.Contains(msg, "interrupted"):
		return fmt.Errorf("the statement ran longer than the database's limit of %s", d.opts.Timeout)
	case strings.Contains(msg, "database or disk is full") || strings.Contains(msg, "SQLITE_FULL"):
		return fmt.Errorf("the database is at its size limit of %d MB; delete rows or drop tables to make room", d.opts.MaxMB)
	}
	return fmt.Errorf("sql: %s", strings.TrimPrefix(msg, "SQL logic error: "))
}

// scan runs a query and reads up to max rows.
func scan(ctx context.Context, c *sql.Conn, q string, args []any, max int) (Result, error) {
	rows, err := c.QueryContext(ctx, q, args...)
	if err != nil {
		return Result{}, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return Result{}, err
	}
	res := Result{Columns: cols, Rows: [][]any{}}
	for rows.Next() {
		if len(res.Rows) == max {
			res.Truncated = true
			break
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return Result{}, err
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				if utf8.Valid(b) {
					vals[i] = string(b)
				} else {
					vals[i] = fmt.Sprintf("<blob %d bytes>", len(b))
				}
			}
		}
		res.Rows = append(res.Rows, vals)
	}
	return res, rows.Err()
}

// withRaw runs fn with a connection's raw handle, checking the driver still
// has the fields the authorizer reads.
func withRaw(ctx context.Context, db *sql.DB, fn func(rawConn) error) error {
	c, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close() //nolint:errcheck // returning the connection to the pool
	return c.Raw(func(dc any) error {
		raw, err := rawOf(dc.(driver.Conn))
		if err != nil {
			return err
		}
		return fn(raw)
	})
}
