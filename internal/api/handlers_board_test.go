package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"nine/internal/protocol"
)

// TestBoardCRUDEndpoints maps the /api/v1/board surface over a scripted daemon:
// list, create, get, patch, delete, and the 404 mapping.
func TestBoardCRUDEndpoints(t *testing.T) {
	task := protocol.BoardTask{
		ID: "task_1", Title: "Fix queue", Spec: "spec", PostedBy: "operator",
		Status: "queued", Source: "post", Depth: 0,
		CreatedAt: "2026-11-10T12:00:00.000Z", UpdatedAt: "2026-11-10T12:00:00.000Z",
	}
	sock := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		switch m.Type {
		case protocol.TypeBoardList:
			return textReply(t, protocol.TypeBoardList, []protocol.BoardTask{task})
		case protocol.TypeBoardGet:
			if m.ID == "task_nope" {
				return errReply(`board task "task_nope" not found`)
			}
			return textReply(t, protocol.TypeBoardGet, task)
		case protocol.TypeBoardCreate:
			return textReply(t, protocol.TypeBoardCreate, task)
		case protocol.TypeBoardUpdate:
			if m.ID == "task_nope" {
				return errReply(`board task "task_nope" not found`)
			}
			return textReply(t, protocol.TypeBoardUpdate, task)
		case protocol.TypeBoardDelete:
			if m.ID == "task_nope" {
				return errReply(`board task "task_nope" not found`)
			}
			return []protocol.Msg{protocol.NewTextMsg(protocol.TypeBoardDelete, "deleted "+m.ID)}
		}
		return errReply("unexpected " + string(m.Type))
	})

	// list
	w := doAt(t, sock, http.MethodGet, "/api/v1/board?status=queued", "")
	if w.Code != 200 {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	var page struct {
		Data []struct {
			Id     string `json:"id"`
			Status string `json:"status"`
		} `json:"data"`
	}
	decodeJSON(t, w, &page)
	if len(page.Data) != 1 || page.Data[0].Id != "task_1" || page.Data[0].Status != "queued" {
		t.Fatalf("list body: %s", w.Body.String())
	}

	// bad status filter
	w = doAt(t, sock, http.MethodGet, "/api/v1/board?status=bogus", "")
	if w.Code != 400 {
		t.Fatalf("list bogus status: %d, want 400", w.Code)
	}

	// create
	w = doAt(t, sock, http.MethodPost, "/api/v1/board", `{"title":"Fix queue","spec":"spec"}`)
	if w.Code != 200 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	// create without title
	w = doAt(t, sock, http.MethodPost, "/api/v1/board", `{"spec":"x"}`)
	if w.Code != 400 {
		t.Fatalf("create no title: %d, want 400", w.Code)
	}

	// get + 404
	w = doAt(t, sock, http.MethodGet, "/api/v1/board/task_1", "")
	if w.Code != 200 {
		t.Fatalf("get: %d", w.Code)
	}
	w = doAt(t, sock, http.MethodGet, "/api/v1/board/task_nope", "")
	if w.Code != 404 {
		t.Fatalf("get unknown: %d, want 404", w.Code)
	}

	// patch + 404
	w = doAt(t, sock, http.MethodPatch, "/api/v1/board/task_1", `{"status":"done","result":"ok"}`)
	if w.Code != 200 {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	w = doAt(t, sock, http.MethodPatch, "/api/v1/board/task_1", `{}`)
	if w.Code != 400 {
		t.Fatalf("patch empty: %d, want 400", w.Code)
	}
	w = doAt(t, sock, http.MethodPatch, "/api/v1/board/task_nope", `{"title":"x"}`)
	if w.Code != 404 {
		t.Fatalf("patch unknown: %d, want 404", w.Code)
	}

	// delete + 404
	w = doAt(t, sock, http.MethodDelete, "/api/v1/board/task_1", "")
	if w.Code != 200 {
		t.Fatalf("delete: %d", w.Code)
	}
	var del struct {
		Id      string `json:"id"`
		Message string `json:"message"`
	}
	decodeJSON(t, w, &del)
	if del.Id != "task_1" || del.Message == "" {
		t.Fatalf("delete body: %s", w.Body.String())
	}
	w = doAt(t, sock, http.MethodDelete, "/api/v1/board/task_nope", "")
	if w.Code != 404 {
		t.Fatalf("delete unknown: %d, want 404", w.Code)
	}
}

var _ = json.Marshal // keep encoding/json if unused after edits

// TestRolesEndpointAndRoleOnCreate maps GET /roles and the role field on
// conversation create over a scripted daemon.
func TestRolesEndpointAndRoleOnCreate(t *testing.T) {
	var sawRole string
	sock := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		switch m.Type {
		case protocol.TypeRolesList:
			return textReply(t, protocol.TypeRolesList, []protocol.RoleInfo{
				{Name: "orchestrator", Description: "root", Tools: []string{"board_post"}},
				{Name: "executor", AllTools: true, Delegates: true},
			})
		case protocol.TypeNewConversation:
			sawRole = m.Role
			return []protocol.Msg{protocol.NewConversationIDMsg("c1")}
		}
		return errReply("unexpected " + string(m.Type))
	})

	w := doAt(t, sock, http.MethodGet, "/api/v1/roles", "")
	if w.Code != 200 {
		t.Fatalf("roles: %d %s", w.Code, w.Body.String())
	}
	var roles struct {
		Data []struct {
			Name     string   `json:"name"`
			AllTools bool     `json:"all_tools"`
			Tools    []string `json:"tools"`
		} `json:"data"`
	}
	decodeJSON(t, w, &roles)
	if len(roles.Data) != 2 || roles.Data[0].Name != "orchestrator" || roles.Data[1].AllTools != true {
		t.Fatalf("roles body: %s", w.Body.String())
	}

	w = doAt(t, sock, http.MethodPost, "/api/v1/conversations", `{"interactive":true,"role":"monitor"}`)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	if sawRole != "monitor" {
		t.Fatalf("daemon saw role %q, want monitor", sawRole)
	}
}

// TestBoardRolePassthrough checks the task role travels the wire: create
// sends it in the board_create payload, patch sends it in board_update, and
// the daemon's refusal (unknown/non-leaf role) maps to 400 (adr/agents-concept.md A6).
func TestBoardRolePassthrough(t *testing.T) {
	var gotCreate, gotUpdate string
	sock := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		switch m.Type {
		case protocol.TypeBoardCreate:
			gotCreate = m.Text
			if strings.Contains(m.Text, "no-such-role") {
				return errReply(`board task role "no-such-role" is unknown or not a leaf role`)
			}
			return textReply(t, protocol.TypeBoardCreate, protocol.BoardTask{ID: "task_1", Title: "t", Status: "queued", Role: "researcher"})
		case protocol.TypeBoardUpdate:
			gotUpdate = m.Text
			return textReply(t, protocol.TypeBoardUpdate, protocol.BoardTask{ID: m.ID, Title: "t", Status: "queued", Role: "executor"})
		}
		return errReply("unexpected " + string(m.Type))
	})

	w := doAt(t, sock, http.MethodPost, "/api/v1/board", `{"title":"t","role":"researcher"}`)
	if w.Code != 200 {
		t.Fatalf("create with role: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(gotCreate, `"role":"researcher"`) {
		t.Fatalf("board_create payload = %s, want role researcher", gotCreate)
	}
	var created struct {
		Role *string `json:"role"`
	}
	decodeJSON(t, w, &created)
	if created.Role == nil || *created.Role != "researcher" {
		t.Fatalf("create response role: %s", w.Body.String())
	}

	w = doAt(t, sock, http.MethodPatch, "/api/v1/board/task_1", `{"role":"executor"}`)
	if w.Code != 200 {
		t.Fatalf("patch role: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(gotUpdate, `"role":"executor"`) {
		t.Fatalf("board_update payload = %s, want role executor", gotUpdate)
	}

	// A refused role is a 400 with the daemon's reason.
	w = doAt(t, sock, http.MethodPost, "/api/v1/board", `{"title":"t","role":"no-such-role"}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "not a leaf") {
		t.Fatalf("create with bad role: %d %s", w.Code, w.Body.String())
	}
}
