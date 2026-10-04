package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nine/internal/config"
	"nine/internal/protocol"
)

// These cover the endpoints that used to answer 501 — history, trace, replay,
// goal create and delete, skills — and the event stream, which used to send a
// handshake and then nothing. Each runs the real router against a scripted
// daemon on a real socket, so what is tested is the HTTP mapping of the replies
// the daemon actually sends. The daemon side is covered in internal/runtime.

// scriptedDaemon answers each request with reply(msg). Replies are written as
// the slice of messages returned, in order.
func scriptedDaemon(t *testing.T, reply func(protocol.Msg) []protocol.Msg) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "nine-api")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close() //nolint:errcheck
				sc := protocol.NewScanner(conn)
				enc := json.NewEncoder(conn)
				for sc.Scan() {
					var m protocol.Msg
					if json.Unmarshal(sc.Bytes(), &m) != nil {
						return
					}
					for _, r := range reply(m) {
						if enc.Encode(r) != nil {
							return
						}
					}
				}
			}()
		}
	}()
	return sock
}

func apiServerAt(sock string) http.Handler {
	return NewServer(Config{
		APIConfig:  config.APIConfig{},
		SocketPath: sock,
		Version:    "test",
		StartTime:  time.Now(),
	}).createHandler()
}

func doAt(t *testing.T, sock, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	apiServerAt(sock).ServeHTTP(w, req)
	return w
}

func textReply(t *testing.T, typ protocol.MsgType, v any) []protocol.Msg {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return []protocol.Msg{protocol.NewTextMsg(typ, string(b))}
}

func errReply(text string) []protocol.Msg { return []protocol.Msg{protocol.NewErrorMsg(text)} }

func decodeJSON(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.NewDecoder(w.Body).Decode(v); err != nil {
		t.Fatalf("decode: %v (body: %s)", err, w.Body.String())
	}
}

// journal is one session's journal for trace and replay, shaped as the daemon
// writes it: an LLM call's request and response share a span, as do a tool
// call's start and end, and a tool's HTTP is parented to the tool's span.
func journal() []protocol.JournalEvent {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	ev := func(seq int64, turn int, typ, span, parent, payload string) protocol.JournalEvent {
		return protocol.JournalEvent{Seq: seq, AgentID: "abc", Turn: turn, Type: typ, SpanID: span,
			ParentSpanID: parent, TS: ts, Payload: json.RawMessage(payload)}
	}
	return []protocol.JournalEvent{
		ev(1, 2, "turn_start", "t2", "", `{"input":"fetch it","trigger":"user"}`),
		ev(2, 2, "llm_request", "t2.llm1", "t2", `{"system":"sys","messages":[{"Role":"user","Text":"fetch it"}],"tool_names":["fetch"],"tokens_used":120,"budget":1000,"llm_call_n":1}`),
		ev(3, 2, "llm_response", "t2.llm1", "t2", `{"text":"","tool_calls":[{"Name":"fetch","Input":{"url":"https://x"}}],"stop_reason":"tool_use","llm_call_n":1,"input_tokens":130,"output_tokens":9}`),
		ev(4, 2, "tool_start", "t2.tool1", "t2.llm1", `{"name":"fetch","input":{"url":"https://x"}}`),
		ev(5, 2, "tool_http", "t2.tool1.http", "t2.tool1", `{"tool":"fetch","method":"GET","host":"x","url":"https://x","status":200,"bytes":5,"duration_ms":3}`),
		ev(6, 2, "tool_end", "t2.tool1", "t2.llm1", `{"name":"fetch","output":"hello","duration_ms":4,"attempts":1}`),
		ev(7, 2, "sub_agent_start", "sub-1", "t2", `{"sub_id":"sub-1","task":"summarise","role":"researcher"}`),
		ev(8, 2, "sub_agent_end", "sub-1", "t2", `{"sub_id":"sub-1","status":"done"}`),
		ev(9, 2, "turn_end", "t2", "", `{"result":"got hello","tool_count":1,"duration_ms":50}`),
	}
}

func TestHistory_MapsTheTranscriptAndPages(t *testing.T) {
	sock := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		if m.Type != protocol.TypeSessionHistory || m.AgentID != "abc" {
			return errReply("unexpected " + string(m.Type))
		}
		user := protocol.NewHistoryUserMsg("abc", "hi")
		user.Turn, user.Timestamp = 1, 1700000000000
		tool := protocol.NewToolEndMsg("abc", "fetch", "", "", json.RawMessage(`{"url":"https://x"}`), "hello")
		tool.Turn = 1
		resp := protocol.NewResponseMsg("abc", "hello back")
		resp.Turn = 1
		return textReply(t, protocol.TypeSessionHistory, []protocol.Msg{user, tool, resp})
	})

	w := doAt(t, sock, "GET", "/api/v1/conversations/abc/history?limit=2", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Data []struct {
			Type       string          `json:"type"`
			Text       string          `json:"text"`
			ToolName   string          `json:"tool_name"`
			ToolInput  json.RawMessage `json:"tool_input"`
			ToolOutput string          `json:"tool_output"`
			Timestamp  string          `json:"timestamp"`
			TurnNumber int             `json:"turn_number"`
		} `json:"data"`
		Pagination struct {
			Total   int  `json:"total"`
			HasMore bool `json:"has_more"`
		} `json:"pagination"`
	}
	decodeJSON(t, w, &got)
	if len(got.Data) != 2 || got.Pagination.Total != 3 || !got.Pagination.HasMore {
		t.Fatalf("page = %+v", got)
	}
	if d := got.Data[0]; d.Type != "user_turn" || d.Text != "hi" || d.TurnNumber != 1 || d.Timestamp == "" {
		t.Errorf("first entry = %+v, want the user prompt of turn 1", d)
	}
	if d := got.Data[1]; d.Type != "tool_end" || d.ToolName != "fetch" || d.ToolOutput != "hello" ||
		string(d.ToolInput) != `{"url":"https://x"}` {
		t.Errorf("second entry = %+v, want the tool call with its input passed through", d)
	}
}

func TestHistory_UnknownConversationIs404(t *testing.T) {
	sock := scriptedDaemon(t, func(protocol.Msg) []protocol.Msg { return errReply("session nope not found") })
	if w := doAt(t, sock, "GET", "/api/v1/conversations/nope/history", ""); w.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404: %s", w.Code, w.Body.String())
	}
}

func TestTrace_NestsSubAgentsWhenAsked(t *testing.T) {
	var asked []protocol.Msg
	sock := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		asked = append(asked, m)
		switch m.AgentID {
		case "abc":
			return textReply(t, protocol.TypeSessionEvents, journal())
		case "sub-1":
			return textReply(t, protocol.TypeSessionEvents, []protocol.JournalEvent{
				{Seq: 1, AgentID: "sub-1", Turn: 1, Type: "turn_start", Payload: json.RawMessage(`{"input":"summarise"}`)},
			})
		}
		return errReply("session " + m.AgentID + " not found")
	})

	w := doAt(t, sock, "GET", "/api/v1/conversations/abc/trace?turn=2&sub_agents=true", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if asked[0].Turn != 2 {
		t.Errorf("asked the daemon for turn %d, want 2", asked[0].Turn)
	}
	var got struct {
		AgentID string `json:"agent_id"`
		Turns   int    `json:"turns"`
		Events  []struct {
			Seq     int64           `json:"seq"`
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		} `json:"events"`
		SubAgents []struct {
			AgentID      string `json:"agent_id"`
			ParentID     string `json:"parent_agent_id"`
			SpawnedAtSeq int64  `json:"spawned_at_seq"`
			Events       []struct {
				Type string `json:"type"`
			} `json:"events"`
		} `json:"sub_agents"`
	}
	decodeJSON(t, w, &got)
	if got.AgentID != "abc" || got.Turns != 1 || len(got.Events) != len(journal()) {
		t.Fatalf("trace = %+v", got)
	}
	if string(got.Events[0].Payload) != `{"input":"fetch it","trigger":"user"}` {
		t.Errorf("payload = %s, want it exactly as journaled", got.Events[0].Payload)
	}
	if len(got.SubAgents) != 1 {
		t.Fatalf("sub_agents = %+v, want sub-1", got.SubAgents)
	}
	if sa := got.SubAgents[0]; sa.AgentID != "sub-1" || sa.ParentID != "abc" || sa.SpawnedAtSeq != 7 || len(sa.Events) != 1 {
		t.Errorf("sub-agent = %+v, want sub-1's trace linked to the spawn at seq 7", sa)
	}

	// Without sub_agents there is no sub-agent list and no second read.
	asked = nil
	w = doAt(t, sock, "GET", "/api/v1/conversations/abc/trace", "")
	if w.Code != http.StatusOK || len(asked) != 1 || asked[0].Turn != 0 {
		t.Fatalf("plain trace: status %d, asked %+v", w.Code, asked)
	}
}

func TestReplay_ReconstructsTheTurn(t *testing.T) {
	sock := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		if m.Turn == 2 {
			return textReply(t, protocol.TypeSessionEvents, journal())
		}
		return textReply(t, protocol.TypeSessionEvents, []protocol.JournalEvent{})
	})

	w := doAt(t, sock, "POST", "/api/v1/conversations/abc/replay", `{"turn":2}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Turn     int    `json:"turn"`
		Trigger  string `json:"trigger"`
		Input    string `json:"input"`
		LLMCalls []struct {
			N            int    `json:"n"`
			MessageCount int    `json:"message_count"`
			TokensUsed   int    `json:"tokens_used"`
			InputTokens  int    `json:"input_tokens"`
			StopReason   string `json:"stop_reason"`
			ToolCalls    []struct {
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"tool_calls"`
		} `json:"llm_calls"`
		ToolCalls []struct {
			Name   string          `json:"name"`
			Input  json.RawMessage `json:"input"`
			Output string          `json:"output"`
			HTTP   []struct {
				Method string `json:"method"`
				Status int    `json:"status"`
			} `json:"http"`
		} `json:"tool_calls"`
		SubAgents []struct {
			ID     string `json:"id"`
			Task   string `json:"task"`
			Status string `json:"status"`
		} `json:"sub_agents"`
		Result struct {
			Text      string `json:"text"`
			ToolCount int    `json:"tool_count"`
		} `json:"result"`
	}
	decodeJSON(t, w, &got)
	if got.Turn != 2 || got.Trigger != "user" || got.Input != "fetch it" {
		t.Errorf("header = %+v", got)
	}
	if len(got.LLMCalls) != 1 {
		t.Fatalf("llm_calls = %+v, want the request and response joined into one call", got.LLMCalls)
	}
	if c := got.LLMCalls[0]; c.N != 1 || c.MessageCount != 1 || c.TokensUsed != 120 || c.InputTokens != 130 ||
		c.StopReason != "tool_use" || len(c.ToolCalls) != 1 || c.ToolCalls[0].Name != "fetch" {
		t.Errorf("llm call = %+v", c)
	}
	if len(got.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %+v, want start and end joined into one call", got.ToolCalls)
	}
	if tc := got.ToolCalls[0]; tc.Name != "fetch" || tc.Output != "hello" || string(tc.Input) != `{"url":"https://x"}` ||
		len(tc.HTTP) != 1 || tc.HTTP[0].Status != 200 {
		t.Errorf("tool call = %+v", tc)
	}
	if len(got.SubAgents) != 1 || got.SubAgents[0].Task != "summarise" || got.SubAgents[0].Status != "done" {
		t.Errorf("sub_agents = %+v", got.SubAgents)
	}
	if got.Result.Text != "got hello" || got.Result.ToolCount != 1 {
		t.Errorf("result = %+v", got.Result)
	}

	if w := doAt(t, sock, "POST", "/api/v1/conversations/abc/replay", `{"turn":9}`); w.Code != http.StatusNotFound {
		t.Errorf("missing turn: status %d, want 404", w.Code)
	}
	if w := doAt(t, sock, "POST", "/api/v1/conversations/abc/replay", `{}`); w.Code != http.StatusBadRequest {
		t.Errorf("no turn: status %d, want 400", w.Code)
	}
}

func TestCreateGoal(t *testing.T) {
	var asked protocol.Msg
	sock := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		asked = m
		if m.ID == "missing" {
			return errReply("parent goal missing not found")
		}
		return textReply(t, protocol.TypeGoalCreate, protocol.GoalCreateResult{
			Goal:          json.RawMessage(`{"id":"g1","description":"watch the repo","status":"active","parent_type":"operator","subtree":null,"created_at":"2026-01-02T03:04:05.000000Z","updated_at":"2026-01-02T03:04:05.000000Z"}`),
			PursueSession: "spawned",
		})
	})

	w := doAt(t, sock, "POST", "/api/v1/goals", `{"description":"watch the repo"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if asked.Type != protocol.TypeGoalCreate || asked.Text != "watch the repo" || asked.ID != "" {
		t.Errorf("daemon asked %+v", asked)
	}
	var got struct {
		Goal struct {
			ID         string `json:"id"`
			ParentType string `json:"parent_type"`
		} `json:"goal"`
		PursueSession string `json:"pursue_session"`
		SessionID     string `json:"session_id"`
	}
	decodeJSON(t, w, &got)
	if got.Goal.ID != "g1" || got.PursueSession != "spawned" || got.SessionID != "g1" {
		t.Errorf("response = %+v", got)
	}

	if w := doAt(t, sock, "POST", "/api/v1/goals", `{"description":"x","parent_id":"missing"}`); w.Code != http.StatusNotFound {
		t.Errorf("missing parent: status %d, want 404", w.Code)
	}
	if w := doAt(t, sock, "POST", "/api/v1/goals", `{"description":""}`); w.Code != http.StatusBadRequest {
		t.Errorf("empty description: status %d, want 400", w.Code)
	}
}

func TestDeleteGoal(t *testing.T) {
	sock := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		switch m.Text {
		case "g1":
			return textReply(t, protocol.TypeGoalDelete, protocol.GoalDeleteResult{Deleted: []string{"g1", "g2"}, SessionStopped: true})
		case "cfg":
			return errReply(protocol.ConflictPrefix + "goal cfg is declared by an [[agent]] block in nine.toml")
		}
		return errReply("goal " + m.Text + " not found")
	})

	w := doAt(t, sock, "DELETE", "/api/v1/goals/g1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		ID             string   `json:"id"`
		Deleted        []string `json:"deleted"`
		SessionStopped bool     `json:"session_stopped"`
	}
	decodeJSON(t, w, &got)
	if got.ID != "g1" || len(got.Deleted) != 2 || !got.SessionStopped {
		t.Errorf("response = %+v", got)
	}

	w = doAt(t, sock, "DELETE", "/api/v1/goals/cfg", "")
	if w.Code != http.StatusConflict {
		t.Fatalf("config goal: status %d, want 409", w.Code)
	}
	if e := decodeErr(t, w); e.Error.Code != "conflict" || strings.HasPrefix(e.Error.Message, protocol.ConflictPrefix) ||
		!strings.Contains(e.Error.Message, "nine.toml") {
		t.Errorf("conflict error = %+v, want the reason without the wire prefix", e.Error)
	}
	if w := doAt(t, sock, "DELETE", "/api/v1/goals/nope", ""); w.Code != http.StatusNotFound {
		t.Errorf("missing goal: status %d, want 404", w.Code)
	}
}

func TestListSkills(t *testing.T) {
	sock := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		return textReply(t, protocol.TypeListSkills, map[string]any{"skills": []protocol.SkillInfo{
			{Name: "a", Description: "first", Tags: []string{"x"}, Source: "builtin"},
			{Name: "b", Source: "agent"},
		}})
	})
	w := doAt(t, sock, "GET", "/api/v1/skills", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Data []struct {
			Name   string   `json:"name"`
			Tags   []string `json:"tags"`
			Source string   `json:"source"`
		} `json:"data"`
		Pagination struct {
			Total int `json:"total"`
		} `json:"pagination"`
	}
	decodeJSON(t, w, &got)
	if got.Pagination.Total != 2 || got.Data[0].Name != "a" || got.Data[1].Source != "agent" || got.Data[1].Tags == nil {
		t.Errorf("skills = %+v", got)
	}
}

// The stream forwards what the daemon's watch feed carries, mapped to the
// declared event names, and drops the types it does not declare.
func TestStream_ForwardsTheWatchFeed(t *testing.T) {
	sock := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		if m.Type != protocol.TypeWatch {
			return errReply("unexpected")
		}
		if m.AgentID != "abc" {
			return errReply("conversation " + m.AgentID + " not found")
		}
		return []protocol.Msg{
			{Type: protocol.TypeWatch, AgentID: "abc"},
			protocol.NewToolStartMsg("abc", "fetch", "", "", json.RawMessage(`{"url":"https://x"}`)),
			protocol.NewThinkingChunkMsg("abc", "hmm"), // not declared by the API: skipped
			protocol.NewResponseChunkMsg("abc", "hel"),
			protocol.NewResponseMsg("abc", "hello"),
			protocol.NewDoneMsg("abc"),
		}
	})

	srv := httptest.NewServer(apiServerAt(sock))
	defer srv.Close()

	if resp, err := http.Get(srv.URL + "/api/v1/conversations/nope/messages/stream"); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close() //nolint:errcheck
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("unknown conversation: status %d, want 404", resp.StatusCode)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/conversations/abc/messages/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d, content type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	var names []string
	var response string
	sc := bufio.NewScanner(resp.Body)
	event := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
			names = append(names, event)
		case strings.HasPrefix(line, "data: ") && event == "response":
			var p struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &p)
			response = p.Text
		}
		if event == "done" {
			break
		}
	}
	want := []string{"connected", "tool_start", "response_chunk", "response", "done"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("events = %v, want %v", names, want)
	}
	if response != "hello" {
		t.Errorf("response text = %q, want hello", response)
	}
}

// The stream must outlive the server's WriteTimeout and still flush through the
// logging middleware's writer wrapper. Before Unwrap, the wrapper hid
// http.Flusher, so nothing reached the client until the handler returned.
func TestStream_FlushesThroughMiddleware(t *testing.T) {
	rw := &responseWriter{ResponseWriter: httptest.NewRecorder()}
	rc := http.NewResponseController(rw)
	if err := rc.Flush(); err != nil {
		t.Fatalf("flush through the middleware wrapper: %v", err)
	}
}
