package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"nine/internal/protocol"
)

// The Builder's endpoints (adr/agents-concept.md A8, Q23): skill, role and
// memory writes over REST, mapped onto the daemon's skill_*/memory_* verbs —
// the same store rows the model's tools write. Each test runs the real router
// against a scripted daemon, so what is tested is the HTTP mapping.

var roleSkillContent = "---\n" +
	"name: coder\n" +
	"description: writes code\n" +
	"tags: [role]\n" +
	"role:\n" +
	"  tools: [shell, file_write]\n" +
	"  delegates: false\n" +
	"  spawns_goals: false\n" +
	"  persists: false\n" +
	"  interactive: false\n" +
	"---\n" +
	"writes code\n"

func roleSkillDetail() protocol.SkillDetail {
	return protocol.SkillDetail{
		SkillInfo: protocol.SkillInfo{Name: "coder", Description: "writes code", Tags: []string{"role"}, Source: "agent"},
		Content:   roleSkillContent,
	}
}

func TestListRolesExposesStructuralFlags(t *testing.T) {
	sock := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		if m.Type != protocol.TypeRolesList {
			return errReply("unexpected " + string(m.Type))
		}
		return textReply(t, protocol.TypeRolesList, []protocol.RoleInfo{
			{Name: "coder", AllTools: false, Tools: []string{"shell"}, Delegates: false, Leaf: true, SpawnsGoals: false, Persists: false, Interactive: true},
		})
	})
	w := doAt(t, sock, http.MethodGet, "/api/v1/roles", "")
	if w.Code != 200 {
		t.Fatalf("roles: %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Data []map[string]any `json:"data"`
	}
	decodeJSON(t, w, &body)
	if len(body.Data) != 1 {
		t.Fatalf("roles body: %s", w.Body.String())
	}
	r := body.Data[0]
	for _, f := range []string{"spawns_goals", "persists", "interactive", "leaf"} {
		if _, ok := r[f]; !ok {
			t.Errorf("role JSON misses %s: %s", f, w.Body.String())
		}
	}
	if r["interactive"] != true || r["leaf"] != true || r["spawns_goals"] != false {
		t.Errorf("role flags wrong: %s", w.Body.String())
	}
}

func TestCreateSkillEndpoint(t *testing.T) {
	sock := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		if m.Type != protocol.TypeSkillCreate {
			return errReply("unexpected " + string(m.Type))
		}
		var req struct {
			Name, Content string
		}
		if json.Unmarshal([]byte(m.Text), &req) != nil {
			t.Errorf("bad skill_create text: %s", m.Text)
		}
		if req.Name != "triage" || req.Content == "" {
			t.Errorf("skill_create payload: %s", m.Text)
		}
		return textReply(t, protocol.TypeSkillCreate, protocol.SkillDetail{
			SkillInfo: protocol.SkillInfo{Name: req.Name, Description: "sort", Tags: []string{"ops"}, Source: "agent"},
			Content:   req.Content,
		})
	})
	w := doAt(t, sock, http.MethodPost, "/api/v1/skills", `{"name":"triage","description":"sort","tags":["ops"],"content":"# Triage\n"}`)
	if w.Code != 200 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var sk struct {
		Name, Source, Content string
	}
	decodeJSON(t, w, &sk)
	if sk.Name != "triage" || sk.Source != "agent" || sk.Content == "" {
		t.Fatalf("create body: %s", w.Body.String())
	}

	// Empty name is refused before the daemon is asked.
	w = doAt(t, sock, http.MethodPost, "/api/v1/skills", `{"name":"  ","content":"x"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty name: %d, want 400", w.Code)
	}

	// A daemon refusal (source-owned name) maps to 400.
	sock2 := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		return errReply("skill triage is owned by its source file")
	})
	w = doAt(t, sock2, http.MethodPost, "/api/v1/skills", `{"name":"triage","content":"x"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("refusal: %d, want 400", w.Code)
	}
}

func TestUpdateSkillEndpoint(t *testing.T) {
	sock := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		if m.Type != protocol.TypeSkillUpdate {
			return errReply("unexpected " + string(m.Type))
		}
		if m.ID != "triage" {
			t.Errorf("skill_update id: %q", m.ID)
		}
		return textReply(t, protocol.TypeSkillUpdate, protocol.SkillDetail{
			SkillInfo: protocol.SkillInfo{Name: "triage", Source: "agent"},
			Content:   "# Triage v2\n",
		})
	})
	w := doAt(t, sock, http.MethodPatch, "/api/v1/skills/triage", `{"content":"# Triage v2\n"}`)
	if w.Code != 200 {
		t.Fatalf("update: %d %s", w.Code, w.Body.String())
	}

	// An empty patch is refused before the daemon is asked.
	w = doAt(t, sock, http.MethodPatch, "/api/v1/skills/triage", `{}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty patch: %d, want 400", w.Code)
	}

	// A missing skill maps to 404.
	sock2 := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		return errReply("skill not found: nope")
	})
	w = doAt(t, sock2, http.MethodPatch, "/api/v1/skills/nope", `{"content":"x"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("not found: %d, want 404", w.Code)
	}
}

func TestDeleteSkillEndpoint(t *testing.T) {
	sock := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		if m.Type != protocol.TypeSkillDelete || m.ID != "triage" {
			return errReply("unexpected " + string(m.Type) + " " + m.ID)
		}
		return []protocol.Msg{protocol.NewTextMsg(protocol.TypeSkillDelete, "deleted triage")}
	})
	w := doAt(t, sock, http.MethodDelete, "/api/v1/skills/triage", "")
	if w.Code != 200 {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	var msg struct {
		Message string `json:"message"`
	}
	decodeJSON(t, w, &msg)
	if msg.Message != "deleted triage" {
		t.Fatalf("delete body: %s", w.Body.String())
	}

	sock2 := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		return errReply("skill not found: nope")
	})
	if w := doAt(t, sock2, http.MethodDelete, "/api/v1/skills/nope", ""); w.Code != http.StatusNotFound {
		t.Fatalf("delete unknown: %d, want 404", w.Code)
	}
	sock3 := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		return errReply("skill builtin is owned by its source file")
	})
	if w := doAt(t, sock3, http.MethodDelete, "/api/v1/skills/builtin", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("delete immutable: %d, want 400", w.Code)
	}
}

func TestCreateRoleEndpoint(t *testing.T) {
	sock := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		switch m.Type {
		case protocol.TypeSkillCreate:
			// The role is composed as a role skill: the content must carry the
			// role frontmatter block with the requested flags.
			var req struct {
				Name, Content string
			}
			if json.Unmarshal([]byte(m.Text), &req) != nil {
				t.Errorf("bad skill_create text: %s", m.Text)
			}
			for _, want := range []string{"name: coder", "tags: [role]", "role:", "tools: [shell, file_write]"} {
				if !strings.Contains(req.Content, want) {
					t.Errorf("role skill misses %q:\n%s", want, req.Content)
				}
			}
			return textReply(t, protocol.TypeSkillCreate, roleSkillDetail())
		case protocol.TypeRolesList:
			return textReply(t, protocol.TypeRolesList, []protocol.RoleInfo{
				{Name: "coder", Description: "writes code", Tools: []string{"shell", "file_write"}, Leaf: true, Interactive: false},
			})
		}
		return errReply("unexpected " + string(m.Type))
	})
	w := doAt(t, sock, http.MethodPost, "/api/v1/roles", `{"name":"coder","description":"writes code","tools":["shell","file_write"]}`)
	if w.Code != 200 {
		t.Fatalf("create role: %d %s", w.Code, w.Body.String())
	}
	var role struct {
		Name  string   `json:"name"`
		Tools []string `json:"tools"`
		Leaf  bool     `json:"leaf"`
	}
	decodeJSON(t, w, &role)
	if role.Name != "coder" || len(role.Tools) != 2 || !role.Leaf {
		t.Fatalf("create role body: %s", w.Body.String())
	}

	// A role needs all_tools or a non-empty allowlist.
	w = doAt(t, sock, http.MethodPost, "/api/v1/roles", `{"name":"empty"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("no tools: %d, want 400", w.Code)
	}
	w = doAt(t, sock, http.MethodPost, "/api/v1/roles", `{"name":"  "}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("no name: %d, want 400", w.Code)
	}
}

func TestUpdateRoleEndpoint(t *testing.T) {
	sock := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		switch m.Type {
		case protocol.TypeSkillGet:
			return textReply(t, protocol.TypeSkillGet, roleSkillDetail())
		case protocol.TypeSkillUpdate:
			var patch map[string]any
			if json.Unmarshal([]byte(m.Text), &patch) != nil {
				t.Errorf("bad skill_update text: %s", m.Text)
			}
			content, _ := patch["content"].(string)
			if !strings.Contains(content, "delegates: true") {
				t.Errorf("re-rendered role misses delegates: true:\n%s", content)
			}
			return textReply(t, protocol.TypeSkillUpdate, roleSkillDetail())
		case protocol.TypeRolesList:
			return textReply(t, protocol.TypeRolesList, []protocol.RoleInfo{
				{Name: "coder", Tools: []string{"shell", "file_write"}, Delegates: true, Leaf: true},
			})
		}
		return errReply("unexpected " + string(m.Type))
	})
	w := doAt(t, sock, http.MethodPatch, "/api/v1/roles/coder", `{"delegates":true}`)
	if w.Code != 200 {
		t.Fatalf("update role: %d %s", w.Code, w.Body.String())
	}
	var role struct {
		Name      string `json:"name"`
		Delegates bool   `json:"delegates"`
	}
	decodeJSON(t, w, &role)
	if role.Name != "coder" || !role.Delegates {
		t.Fatalf("update role body: %s", w.Body.String())
	}

	// A skill without a role block is not a role.
	sock2 := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		if m.Type == protocol.TypeSkillGet {
			return textReply(t, protocol.TypeSkillGet, protocol.SkillDetail{
				SkillInfo: protocol.SkillInfo{Name: "triage", Source: "user"},
				Content:   "---\nname: triage\n---\njust a skill\n",
			})
		}
		return errReply("unexpected " + string(m.Type))
	})
	if w := doAt(t, sock2, http.MethodPatch, "/api/v1/roles/triage", `{"delegates":true}`); w.Code != http.StatusBadRequest {
		t.Fatalf("not a role: %d, want 400", w.Code)
	}
	if w := doAt(t, sock2, http.MethodDelete, "/api/v1/roles/triage", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("delete non-role: %d, want 400", w.Code)
	}
}

func TestDeleteRoleEndpoint(t *testing.T) {
	sock := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		switch m.Type {
		case protocol.TypeSkillGet:
			return textReply(t, protocol.TypeSkillGet, roleSkillDetail())
		case protocol.TypeSkillDelete:
			if m.ID != "coder" {
				t.Errorf("skill_delete id: %q", m.ID)
			}
			return []protocol.Msg{protocol.NewTextMsg(protocol.TypeSkillDelete, "deleted coder")}
		}
		return errReply("unexpected " + string(m.Type))
	})
	w := doAt(t, sock, http.MethodDelete, "/api/v1/roles/coder", "")
	if w.Code != 200 {
		t.Fatalf("delete role: %d %s", w.Code, w.Body.String())
	}
	var msg struct {
		Message string `json:"message"`
	}
	decodeJSON(t, w, &msg)
	if msg.Message != "deleted coder" {
		t.Fatalf("delete role body: %s", w.Body.String())
	}

	sock2 := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		return errReply("skill not found: nope")
	})
	if w := doAt(t, sock2, http.MethodDelete, "/api/v1/roles/nope", ""); w.Code != http.StatusNotFound {
		t.Fatalf("delete unknown role: %d, want 404", w.Code)
	}
}

func TestSetMemoryValueEndpoint(t *testing.T) {
	sock := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		if m.Type != protocol.TypeMemorySet {
			return errReply("unexpected " + string(m.Type))
		}
		var req struct {
			Key, Value string
		}
		if json.Unmarshal([]byte(m.Text), &req) != nil || req.Key != "self/identity" || req.Value != "v" {
			t.Errorf("memory_set payload: %s", m.Text)
		}
		return []protocol.Msg{protocol.NewTextMsg(protocol.TypeMemorySet, "ok")}
	})
	w := doAt(t, sock, http.MethodPut, "/api/v1/memory/value", `{"key":"self/identity","value":"v"}`)
	if w.Code != 200 {
		t.Fatalf("set: %d %s", w.Code, w.Body.String())
	}
	var kv struct {
		Key, Value string
	}
	decodeJSON(t, w, &kv)
	if kv.Key != "self/identity" || kv.Value != "v" {
		t.Fatalf("set body: %s", w.Body.String())
	}

	w = doAt(t, sock, http.MethodPut, "/api/v1/memory/value", `{"key":"","value":"v"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing key: %d, want 400", w.Code)
	}

	sock2 := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		return errReply("store is read-only")
	})
	if w := doAt(t, sock2, http.MethodPut, "/api/v1/memory/value", `{"key":"k","value":"v"}`); w.Code != http.StatusInternalServerError {
		t.Fatalf("daemon error: %d, want 500", w.Code)
	}
}

func TestDeleteMemoryValueEndpoint(t *testing.T) {
	sock := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		if m.Type != protocol.TypeMemoryDelete || m.Text != "self/identity" {
			return errReply("unexpected " + string(m.Type) + " " + m.Text)
		}
		return []protocol.Msg{protocol.NewTextMsg(protocol.TypeMemoryDelete, "ok")}
	})
	w := doAt(t, sock, http.MethodDelete, "/api/v1/memory/value?key=self%2Fidentity", "")
	if w.Code != 200 {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	var msg struct {
		Message string `json:"message"`
	}
	decodeJSON(t, w, &msg)
	if msg.Message != "deleted self/identity" {
		t.Fatalf("delete body: %s", w.Body.String())
	}

	if w := doAt(t, sock, http.MethodDelete, "/api/v1/memory/value", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("missing key param: %d, want 400", w.Code)
	}
}
