package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nine/internal/protocol"
)

// A workspace file is served as bytes no browser runs, with its version as the
// ETag; a write sends the body and the If-Match version to the daemon.
func TestWorkspaceFileEndpoints(t *testing.T) {
	var asked protocol.Msg
	sock := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		asked = m
		var args protocol.WorkspaceArgs
		_ = json.Unmarshal(m.ToolInput, &args)
		switch {
		case m.Text == protocol.WorkspaceRead && args.Path == "page.html":
			return textReply(t, protocol.TypeWorkspace, protocol.WorkspaceFile{
				WorkspaceEntry: protocol.WorkspaceEntry{Name: "page.html", Path: "page.html", Bytes: 15,
					Modified: "2026-10-15T09:00:00Z", Version: "v1"},
				Content: []byte("<script></script>"),
			})
		case m.Text == protocol.WorkspaceRead:
			return errReply("not found: " + args.Path)
		case m.Text == protocol.WorkspaceWrite && args.IfVersion == "stale":
			return errReply("page.html: the file changed since it was read")
		case m.Text == protocol.WorkspaceWrite:
			return textReply(t, protocol.TypeWorkspace, protocol.WorkspaceEntry{Name: "page.html", Path: args.Path,
				Bytes: int64(len(args.Content)), Modified: "2026-10-15T09:01:00Z", Version: "v2", Trashed: "20261015T090100Z-ab"})
		}
		return errReply("unexpected")
	})

	w := doAt(t, sock, "GET", "/api/v1/workspace/file?path=page.html&download=true", "")
	if w.Code != http.StatusOK || w.Body.String() != "<script></script>" {
		t.Fatalf("read: %d %s", w.Code, w.Body.String())
	}
	h := w.Header()
	if h.Get("Content-Type") != "application/octet-stream" || h.Get("X-Content-Type-Options") != "nosniff" ||
		h.Get("Content-Security-Policy") != "sandbox" || h.Get("ETag") != `"v1"` ||
		!strings.HasPrefix(h.Get("Content-Disposition"), "attachment") {
		t.Errorf("headers = %v", h)
	}
	if w := doAt(t, sock, "GET", "/api/v1/workspace/file?path=nope", ""); w.Code != http.StatusNotFound {
		t.Errorf("missing file: %d", w.Code)
	}

	put := func(ifMatch string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PUT", "/api/v1/workspace/file?path=page.html", strings.NewReader("new body"))
		req.Header.Set("Content-Type", "application/octet-stream")
		if ifMatch != "" {
			req.Header.Set("If-Match", ifMatch)
		}
		w := httptest.NewRecorder()
		apiServerAt(sock).ServeHTTP(w, req)
		return w
	}
	w = put(`"v1"`)
	if w.Code != http.StatusOK {
		t.Fatalf("write: %d %s", w.Code, w.Body.String())
	}
	var args protocol.WorkspaceArgs
	_ = json.Unmarshal(asked.ToolInput, &args)
	if string(args.Content) != "new body" || args.IfVersion != "v1" {
		t.Errorf("daemon asked to write %+v", args)
	}
	if w := put(`"stale"`); w.Code != http.StatusConflict {
		t.Errorf("stale write: %d, want 409", w.Code)
	}
}

// Rows, a row change and a statement reach the daemon with their arguments;
// a refusal is the caller's 400 and an unknown table a 404.
func TestDatabaseEndpoints(t *testing.T) {
	var asked protocol.Msg
	sock := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		asked = m
		var args protocol.DatabaseArgs
		_ = json.Unmarshal(m.ToolInput, &args)
		switch {
		case args.Table == "nope":
			return errReply("table not found: nope")
		case m.Text == protocol.DatabaseRows:
			return textReply(t, protocol.TypeDatabase, map[string]any{"table": "seen", "columns": []string{"url"},
				"rowids": []int64{4}, "rows": [][]any{{"a"}}, "total": 1, "offset": args.Offset})
		case m.Text == protocol.DatabaseRun && strings.HasPrefix(args.SQL, "ATTACH"):
			return errReply("denied: attach is not allowed in the database")
		}
		return textReply(t, protocol.TypeDatabase, map[string]any{"columns": []string{}, "rows": [][]any{}, "write": true, "changes": 1})
	})

	w := doAt(t, sock, "GET", "/api/v1/database/tables/seen/rows?offset=10&limit=5", "")
	if w.Code != http.StatusOK {
		t.Fatalf("rows: %d %s", w.Code, w.Body.String())
	}
	var args protocol.DatabaseArgs
	_ = json.Unmarshal(asked.ToolInput, &args)
	if args.Table != "seen" || args.Offset != 10 || args.Limit != 5 {
		t.Errorf("asked %+v", args)
	}
	if w := doAt(t, sock, "GET", "/api/v1/database/tables/nope/rows", ""); w.Code != http.StatusNotFound {
		t.Errorf("unknown table: %d", w.Code)
	}

	w = doAt(t, sock, "PATCH", "/api/v1/database/tables/seen/rows/4", `{"values":{"url":"b","n":null}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update: %d %s", w.Code, w.Body.String())
	}
	args = protocol.DatabaseArgs{}
	_ = json.Unmarshal(asked.ToolInput, &args)
	if asked.Text != protocol.DatabaseUpdate || args.RowID != 4 || args.Values["url"] != "b" {
		t.Errorf("asked %s %+v", asked.Text, args)
	}
	if v, ok := args.Values["n"]; !ok || v != nil {
		t.Errorf("a null value was dropped: %+v", args.Values)
	}

	if w := doAt(t, sock, "POST", "/api/v1/database/sql", `{"sql":"ATTACH DATABASE ':memory:' AS o"}`); w.Code != http.StatusBadRequest {
		t.Errorf("refused statement: %d, want 400", w.Code)
	}
	if w := doAt(t, sock, "POST", "/api/v1/database/sql", `{"sql":""}`); w.Code != http.StatusBadRequest {
		t.Errorf("empty statement: %d, want 400", w.Code)
	}
}
