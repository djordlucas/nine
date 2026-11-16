package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"nine/internal/protocol"
)

// TestAgentCRUDEndpoints maps the /api/v1/agents surface over a scripted daemon:
// list, create, get, patch, delete, run, and the 404 mapping.
func TestAgentCRUDEndpoints(t *testing.T) {
	agent := protocol.Agent{
		ID: "agent_1", Version: 2,
		Definition: protocol.AgentDefinition{Name: "Researcher", Role: "researcher", Skills: []string{"web"}},
		CreatedAt:  "2026-11-16T10:00:00.000Z", UpdatedAt: "2026-11-16T11:00:00.000Z",
	}
	sock := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		switch m.Type {
		case protocol.TypeAgentList:
			return textReply(t, protocol.TypeAgentList, []protocol.Agent{agent})
		case protocol.TypeAgentGet:
			if m.ID == "agent_nope" {
				return errReply(`agent "agent_nope" not found`)
			}
			return textReply(t, protocol.TypeAgentGet, agent)
		case protocol.TypeAgentCreate:
			return textReply(t, protocol.TypeAgentCreate, agent)
		case protocol.TypeAgentUpdate:
			if m.ID == "agent_nope" {
				return errReply(`agent "agent_nope" not found`)
			}
			return textReply(t, protocol.TypeAgentUpdate, agent)
		case protocol.TypeAgentDelete:
			if m.ID == "agent_nope" {
				return errReply(`agent "agent_nope" not found`)
			}
			return []protocol.Msg{protocol.NewTextMsg(protocol.TypeAgentDelete, "deleted "+m.ID)}
		case protocol.TypeAgentRun:
			if m.ID == "agent_nope" {
				return errReply(`agent "agent_nope" not found`)
			}
			return textReply(t, protocol.TypeAgentRun, protocol.AgentRunResult{
				AgentID: m.ID, ConversationID: "conv_1", Role: "researcher",
			})
		}
		return errReply("unexpected " + string(m.Type))
	})

	// list
	w := doAt(t, sock, http.MethodGet, "/api/v1/agents", "")
	if w.Code != 200 {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	var page struct {
		Data []struct {
			Id         string `json:"id"`
			Version    int    `json:"version"`
			Definition struct {
				Name string `json:"name"`
				Role string `json:"role"`
			} `json:"definition"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("list decode: %v", err)
	}
	if len(page.Data) != 1 || page.Data[0].Id != "agent_1" || page.Data[0].Definition.Role != "researcher" {
		t.Fatalf("list body: %s", w.Body.String())
	}

	// create
	w = doAt(t, sock, http.MethodPost, "/api/v1/agents", `{"name":"Researcher","role":"researcher","skills":["web"]}`)
	if w.Code != 200 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}

	// get + 404
	w = doAt(t, sock, http.MethodGet, "/api/v1/agents/agent_1", "")
	if w.Code != 200 {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}
	w = doAt(t, sock, http.MethodGet, "/api/v1/agents/agent_nope", "")
	if w.Code != 404 {
		t.Fatalf("get missing: %d %s", w.Code, w.Body.String())
	}

	// patch + 404
	w = doAt(t, sock, http.MethodPatch, "/api/v1/agents/agent_1", `{"role":"writer"}`)
	if w.Code != 200 {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	w = doAt(t, sock, http.MethodPatch, "/api/v1/agents/agent_nope", `{"role":"writer"}`)
	if w.Code != 404 {
		t.Fatalf("patch missing: %d %s", w.Code, w.Body.String())
	}

	// delete + 404
	w = doAt(t, sock, http.MethodDelete, "/api/v1/agents/agent_1", "")
	if w.Code != 200 {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	w = doAt(t, sock, http.MethodDelete, "/api/v1/agents/agent_nope", "")
	if w.Code != 404 {
		t.Fatalf("delete missing: %d %s", w.Code, w.Body.String())
	}

	// run
	w = doAt(t, sock, http.MethodPost, "/api/v1/agents/agent_1/run", `{"interactive":true}`)
	if w.Code != 200 {
		t.Fatalf("run: %d %s", w.Code, w.Body.String())
	}
	var run struct {
		AgentId        string `json:"agent_id"`
		ConversationId string `json:"conversation_id"`
		Role           string `json:"role"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &run); err != nil {
		t.Fatalf("run decode: %v", err)
	}
	if run.ConversationId != "conv_1" || run.AgentId != "agent_1" || run.Role != "researcher" {
		t.Fatalf("run body: %s", w.Body.String())
	}
	w = doAt(t, sock, http.MethodPost, "/api/v1/agents/agent_nope/run", "")
	if w.Code != 404 {
		t.Fatalf("run missing: %d %s", w.Code, w.Body.String())
	}
}
