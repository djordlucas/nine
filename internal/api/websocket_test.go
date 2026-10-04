package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"nine/internal/config"
	"nine/internal/protocol"
)

// wsDaemon scripts the daemon behind a WebSocket: the watch connection gets
// events, and each user_turn is answered by its text — "busy" is queued,
// "nope" is refused, anything else completes. It records what it was sent.
type wsDaemon struct {
	mu   sync.Mutex
	seen []protocol.Msg
}

func (d *wsDaemon) reply(m protocol.Msg) []protocol.Msg {
	d.mu.Lock()
	d.seen = append(d.seen, m)
	d.mu.Unlock()
	switch m.Type {
	case protocol.TypeWatch:
		if m.AgentID != "abc" {
			return errReply("conversation " + m.AgentID + " not found")
		}
		return []protocol.Msg{
			{Type: protocol.TypeWatch, AgentID: "abc"},
			protocol.NewToolStartMsg("abc", "fetch", "", "", json.RawMessage(`{"url":"https://x"}`)),
			protocol.NewHumanInputRequiredMsg("abc", "req-1", "Proceed?", []string{"yes", "no"}, 60, ""),
			protocol.NewResponseMsg("abc", "hello"),
			protocol.NewDoneMsg("abc"),
		}
	case protocol.TypeUserTurn:
		switch m.Text {
		case "busy":
			n := protocol.NewNoticeMsg("abc", "Your message has been queued.")
			n.Status = protocol.StatusQueued
			return []protocol.Msg{n}
		case "nope":
			return errReply("conversation abc not found; use new_conversation first")
		}
		return []protocol.Msg{protocol.NewResponseMsg("abc", "ok"), protocol.NewDoneMsg("abc")}
	case protocol.TypeHumanInputAnswer:
		if m.RequestID == "stale" {
			return errReply("pending question stale not found: it was answered, timed out, or never asked")
		}
		return []protocol.Msg{{Type: protocol.TypeHumanInputAnswer, Text: "answered"}}
	}
	return errReply("unexpected " + string(m.Type))
}

func (d *wsDaemon) received(t protocol.MsgType) []protocol.Msg {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []protocol.Msg
	for _, m := range d.seen {
		if m.Type == t {
			out = append(out, m)
		}
	}
	return out
}

// wsServer serves the API with deliberately short server timeouts, so a test
// that outlives them proves they do not cut a WebSocket off.
func wsServer(t *testing.T, d *wsDaemon) *httptest.Server {
	t.Helper()
	sock := scriptedDaemon(t, d.reply)
	h := NewServer(Config{APIConfig: config.APIConfig{}, SocketPath: sock, Version: "test", StartTime: time.Now()}).createHandler()
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ReadTimeout = 500 * time.Millisecond
	srv.Config.WriteTimeout = 500 * time.Millisecond
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

func wsURL(srv *httptest.Server, id string) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/v1/conversations/" + id + "/messages"
}

// readUntil reads messages until one of the given type arrives, returning
// every message read.
func readUntil(t *testing.T, ctx context.Context, c *websocket.Conn, typ string) []map[string]any {
	t.Helper()
	var got []map[string]any
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read while waiting for %q: %v (got %v)", typ, err, got)
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("message is not a JSON object: %s", data)
		}
		got = append(got, m)
		if m["type"] == typ {
			return got
		}
	}
}

func writeJSON(t *testing.T, ctx context.Context, c *websocket.Conn, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestWebSocket_UnknownConversationIs404BeforeUpgrade(t *testing.T) {
	srv := wsServer(t, &wsDaemon{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, wsURL(srv, "nope"), nil)
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close() //nolint:errcheck
	}
	if err == nil {
		t.Fatal("dial succeeded for an unknown conversation")
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("response = %v, want 404", resp)
	}
}

func TestWebSocket_RelaysEventsAndAcceptsTurns(t *testing.T) {
	d := &wsDaemon{}
	srv := wsServer(t, d)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, wsURL(srv, "abc"), nil)
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close() //nolint:errcheck
	}
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow() //nolint:errcheck

	// The watch feed, as flat typed objects in the SSE schemas' shape.
	got := readUntil(t, ctx, c, "done")
	var types []string
	for _, m := range got {
		types = append(types, m["type"].(string))
	}
	if want := "connected,tool_start,human_input_required,response,done"; strings.Join(types, ",") != want {
		t.Fatalf("events = %v, want %s", types, want)
	}
	if ts := got[1]; ts["tool_name"] != "fetch" || ts["tool_input"].(map[string]any)["url"] != "https://x" {
		t.Errorf("tool_start = %v", ts)
	}
	if q := got[2]; q["request_id"] != "req-1" || q["question"] != "Proceed?" {
		t.Errorf("human_input_required = %v", q)
	}

	// Outlive the server's 500ms read and write timeouts before talking again.
	time.Sleep(1200 * time.Millisecond)

	// A turn queued behind a running one is reported; one that runs is not
	// (its events arrive through the watch like any turn's).
	writeJSON(t, ctx, c, map[string]any{"type": "user_turn", "text": "busy"})
	if q := readUntil(t, ctx, c, "queued"); q[len(q)-1]["text"] != "Your message has been queued." {
		t.Errorf("queued = %v", q[len(q)-1])
	}

	// A refused submission is an error naming why.
	writeJSON(t, ctx, c, map[string]any{"type": "user_turn", "text": "nope"})
	e := readUntil(t, ctx, c, "error")
	if errObj := e[len(e)-1]["error"].(map[string]any); errObj["code"] != "not_found" {
		t.Errorf("refused turn error = %v, want not_found", errObj)
	}

	// Answering the session's question goes to the daemon on the session's id.
	writeJSON(t, ctx, c, map[string]any{"type": "human_input_answer", "request_id": "req-1", "answer": "yes"})
	if a := readUntil(t, ctx, c, "human_input_answered"); a[len(a)-1]["request_id"] != "req-1" {
		t.Errorf("answered = %v", a[len(a)-1])
	}
	// An answer to a question that is no longer pending is the client's error.
	writeJSON(t, ctx, c, map[string]any{"type": "human_input_answer", "request_id": "stale", "answer": "yes"})
	if e := readUntil(t, ctx, c, "error"); e[len(e)-1]["error"].(map[string]any)["code"] != "not_found" {
		t.Errorf("stale answer error = %v, want not_found", e[len(e)-1])
	}
	answers := d.received(protocol.TypeHumanInputAnswer)
	if len(answers) != 2 || answers[0].AgentID != "abc" || answers[0].RequestID != "req-1" || answers[0].Answer != "yes" {
		t.Errorf("daemon received answers %+v", answers)
	}

	// Malformed input is answered, not fatal.
	for _, bad := range []string{`{"type":"user_turn","text":"  "}`, `{"type":"dance"}`, `not json`} {
		if err := c.Write(ctx, websocket.MessageText, []byte(bad)); err != nil {
			t.Fatal(err)
		}
		e := readUntil(t, ctx, c, "error")
		if code := e[len(e)-1]["error"].(map[string]any)["code"]; code != "invalid_request" {
			t.Errorf("%s: code = %v, want invalid_request", bad, code)
		}
	}

	turns := d.received(protocol.TypeUserTurn)
	if len(turns) != 2 || turns[0].AgentID != "abc" {
		t.Errorf("daemon received turns %+v, want busy and nope for abc", turns)
	}
}

// Origin checking follows the CORS configuration: a browser origin outside it
// is refused, and the wildcard default skips the check.
func TestWebSocket_OriginFollowsCORS(t *testing.T) {
	s := NewServer(Config{APIConfig: config.APIConfig{CORSOrigins: []string{"https://app.example.com"}}})
	opts := s.wsAcceptOptions()
	if opts.InsecureSkipVerify || len(opts.OriginPatterns) != 1 || opts.OriginPatterns[0] != "app.example.com" {
		t.Errorf("explicit origins → %+v", opts)
	}
	if !NewServer(Config{}).wsAcceptOptions().InsecureSkipVerify {
		t.Error("the default CORS wildcard should skip origin checking")
	}
}

func TestWSEncodeFlattensWithType(t *testing.T) {
	b, err := wsEncode("error", errorBody("not_found", "gone", nil))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["type"] != "error" || m["error"].(map[string]any)["code"] != "not_found" {
		t.Errorf("encoded = %s, want {\"type\":\"error\",\"error\":{...}}", b)
	}
}
