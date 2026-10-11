package toolvm

import (
	"context"
	"encoding/json"

	"github.com/tetratelabs/wazero/api"
)

// The `sql` capability's host function: nine:sql (adr/process-sessions.md §8).

type sqlRequest struct {
	Op   string `json:"op"`
	SQL  string `json:"sql"`
	Args []any  `json:"args"`
}

type sqlResponse struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
	Code   string          `json:"code,omitempty"`
}

// hostSQL is the guest's `nine:sql`. Like the other capability imports it is
// exported to every tool; a call is served only when the tool holds the sql
// grant and runs as a process program — a live process, or a slice process's
// call the runner marked (WithSQLHandler). A tool the model invoked is refused.
func (h *Host) hostSQL(ctx context.Context, mod api.Module, ptr, size uint32) uint64 {
	if g, _ := ctx.Value(grantKey{}).(*Grant); g == nil || !g.SQL {
		return writeSQL(ctx, mod, sqlResponse{
			Error: "blocked: this tool was not granted the sql capability (declare sql = true)", Code: "E_SQL"})
	}
	handler, _ := ctx.Value(sqlKey{}).(SQLHandler)
	if handler == nil {
		return writeSQL(ctx, mod, sqlResponse{
			Error: "nine:sql is for process programs; this tool was called, not run as a process", Code: "E_SQL"})
	}
	raw, ok := mod.Memory().Read(ptr, size)
	if !ok {
		return writeSQL(ctx, mod, sqlResponse{Error: "unreadable request", Code: "E_SQL"})
	}
	var req sqlRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return writeSQL(ctx, mod, sqlResponse{Error: "malformed request: " + err.Error(), Code: "E_SQL"})
	}
	if req.Op != "query" && req.Op != "exec" {
		return writeSQL(ctx, mod, sqlResponse{Error: "unknown sql op " + req.Op, Code: "E_SQL"})
	}
	res, err := handler.SQL(ctx, req.Op == "exec", req.SQL, req.Args)
	if err != nil {
		if ctx.Err() != nil {
			return writeSQL(ctx, mod, sqlResponse{Error: "process stopped", Code: "E_STOPPED"})
		}
		return writeSQL(ctx, mod, sqlResponse{Error: err.Error(), Code: "E_SQL"})
	}
	return writeSQL(ctx, mod, sqlResponse{Result: res})
}

func writeSQL(ctx context.Context, mod api.Module, resp sqlResponse) uint64 {
	out, err := json.Marshal(resp)
	if err != nil {
		out = []byte(`{"error":"cannot encode the sql response"}`)
	}
	return writeGuestBytes(ctx, mod, out)
}
