package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"nine/internal/memory"
	"nine/internal/protocol"
	"nine/internal/sqldb"
)

// The operator's database (adr/web-ui.md §10): its tables, a page of rows, a
// row inserted, changed or deleted, and one statement run as written. Only
// the database processes keep their records in; the store stays private.

// OperatorJournalID is the journal's id for the operator's writes to the
// database, which a process's are journaled under its own.
const OperatorJournalID = "operator"

func (d *Daemon) handleDatabase(ctx context.Context, enc *json.Encoder, r protocol.DatabaseReq) {
	reply := func(v any, err error) {
		if err != nil {
			enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
			return
		}
		data, _ := json.Marshal(v)
		enc.Encode(protocol.NewTextMsg(protocol.TypeDatabase, string(data))) //nolint:errcheck
	}
	db := d.database
	if db == nil {
		reply(nil, errors.New("this instance has no database"))
		return
	}
	var args protocol.DatabaseArgs
	if len(r.Args) > 0 {
		// Numbers stay exact: 7 is stored as an integer, not 7.0.
		dec := json.NewDecoder(bytes.NewReader(r.Args))
		dec.UseNumber()
		if err := dec.Decode(&args); err != nil {
			reply(nil, fmt.Errorf("database: %w", err))
			return
		}
	}
	for k, v := range args.Values {
		args.Values[k] = sqlValue(v)
	}
	for i, v := range args.Args {
		args.Args[i] = sqlValue(v)
	}
	switch r.Action {
	case protocol.DatabaseTables:
		tables, err := db.Tables(ctx)
		reply(map[string]any{"tables": tables}, err)
	case protocol.DatabaseRows:
		reply(db.Rows(ctx, args.Table, args.Offset, args.Limit))
	case protocol.DatabaseInsert:
		reply(d.journalWrite(db.InsertRow(ctx, args.Table, args.Values)))
	case protocol.DatabaseUpdate:
		reply(d.journalWrite(db.UpdateRow(ctx, args.Table, args.RowID, args.Values)))
	case protocol.DatabaseDelete:
		reply(d.journalWrite(db.DeleteRow(ctx, args.Table, args.RowID)))
	case protocol.DatabaseRun:
		if strings.TrimSpace(args.SQL) == "" {
			reply(nil, errors.New("sql is required"))
			return
		}
		reply(d.journalWrite(db.Run(ctx, args.SQL, args.Args...)))
	default:
		reply(nil, fmt.Errorf("unknown database action %q (want one of %s)", r.Action,
			strings.Join(protocol.DatabaseActions, ", ")))
	}
}

// journalWrite records an operator's write as a process's are: kinds and
// tables, never values.
func (d *Daemon) journalWrite(res sqldb.RunResult, err error) (sqldb.RunResult, error) {
	// The production store; a test's stand-in journals nothing.
	if store, ok := d.store.(*memory.Store); ok && len(res.Writes) > 0 {
		journalTransition(store, OperatorJournalID, evSQLWrite, map[string]any{
			"by": OperatorJournalID, "writes": res.Writes, "changes": res.Changes, "error": errText(err),
		})
	}
	return res, err
}

// sqlValue makes a JSON value a statement parameter: a whole number an integer,
// any other number a float, an object or array its JSON text.
func sqlValue(v any) any {
	switch x := v.(type) {
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return n
		}
		f, _ := x.Float64()
		return f
	case map[string]any, []any:
		b, _ := json.Marshal(x)
		return string(b)
	}
	return v
}
