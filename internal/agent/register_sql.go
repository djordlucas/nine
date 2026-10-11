package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"nine/internal/llm"
)

// SQLReader reads the database (adr/process-sessions.md §8): the SQLite
// database processes write through nine:sql. A turn only reads it; writes stay
// in programs.
type SQLReader interface {
	// Query runs one read-only statement and returns its result.
	Query(ctx context.Context, query string, args ...any) (any, error)
}

// SQLToolName is the database's read tool.
const SQLToolName = "sql_query"

var sqlToolDefs = []llm.ToolDef{{
	Name:        SQLToolName,
	DisplayName: "Query the Database",
	Description: "Read the database: the SQLite database Nine's processes keep their structured records in — what they collected, counted and decided. One read-only statement: SELECT, or PRAGMA table_list / table_info(<table>) to see the schema. Returns columns and rows, at most 1000. Writing is for processes' programs (nine:sql), never a turn.",
	InputSchema: json.RawMessage(`{"type":"object","required":["sql"],"properties":{` +
		`"sql":{"type":"string","description":"One SELECT or introspection PRAGMA, with ? for parameters."},` +
		`"args":{"type":"array","description":"Values for the ? parameters, in order."}}}`),
}}

// RegisterSQLTool registers sql_query into d.
func RegisterSQLTool(d *Dispatcher, r SQLReader) {
	d.handlers[SQLToolName] = func(ctx context.Context, args json.RawMessage) (string, error) {
		var req struct {
			SQL  string `json:"sql"`
			Args []any  `json:"args"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("sql_query: %w", err)
		}
		if strings.TrimSpace(req.SQL) == "" {
			return "", fmt.Errorf("sql_query: sql is required")
		}
		res, err := r.Query(ctx, req.SQL, req.Args...)
		if err != nil {
			return "", err
		}
		b, err := json.Marshal(res)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
}
