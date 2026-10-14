package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"nine/internal/protocol"
)

// sseBlock is one block of an event stream: what an EventSource parses
// between blank lines.
type sseBlock struct {
	id, event, data string
}

// readSSE reads n blocks from GET url, sending header (if any), then hangs up.
func readSSE(t *testing.T, url string, header http.Header, n int) []sseBlock {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d, content type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var out []sseBlock
	var cur sseBlock
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for len(out) < n && sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if cur != (sseBlock{}) {
				out = append(out, cur)
			}
			cur = sseBlock{}
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "id: "):
			cur.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			cur.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
		}
	}
	if len(out) < n {
		t.Fatalf("read %d blocks %+v, want %d", len(out), out, n)
	}
	return out
}

func journalMsg(t *testing.T, evs ...protocol.JournalEvent) protocol.Msg {
	t.Helper()
	b, err := json.Marshal(evs)
	if err != nil {
		t.Fatal(err)
	}
	return protocol.NewTextMsg(protocol.TypeJournalFollow, string(b))
}

func TestStreamEvents(t *testing.T) {
	ts := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	big := `{"system":"` + strings.Repeat("x", maxEventPayload) + `"}`
	var mu sync.Mutex
	var asked []protocol.Msg
	sock := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		if m.Type != protocol.TypeJournalFollow {
			return errReply("unexpected " + string(m.Type))
		}
		mu.Lock()
		asked = append(asked, m)
		mu.Unlock()
		start := m.After
		if start == 0 {
			start = 5
		}
		return []protocol.Msg{
			{Type: protocol.TypeJournalFollow, After: start},
			journalMsg(t,
				protocol.JournalEvent{Seq: 6, AgentID: "a", Turn: 1, SpanID: "t1", Type: "turn_start", TS: ts, Payload: json.RawMessage(`{"input":"hi"}`)},
				protocol.JournalEvent{Seq: 7, AgentID: "a", Turn: 1, SpanID: "l1", ParentSpanID: "t1", Type: "llm_request", TS: ts, Payload: json.RawMessage(big)},
				protocol.JournalEvent{Seq: 8, AgentID: "g1", Type: "goal_created", SpanID: "state", TS: ts, Payload: json.RawMessage(`{"id":"g1"}`)},
			),
			journalMsg(t,
				protocol.JournalEvent{Seq: 9, AgentID: "b", Turn: 2, Type: "tool_end", TS: ts},
			),
		}
	})
	srv := httptest.NewServer(apiServerAt(sock))
	defer srv.Close()

	t.Run("everything, payloads capped", func(t *testing.T) {
		got := readSSE(t, srv.URL+"/api/v1/events?tail=20", nil, 5)
		if got[0].event != "connected" || got[0].id != "5" || got[0].data != `{"seq":5}` {
			t.Errorf("handshake = %+v", got[0])
		}
		for i, want := range []string{"6", "7", "8", "9"} {
			if b := got[i+1]; b.event != "journal" || b.id != want {
				t.Errorf("block %d = %+v, want journal %s", i+1, b, want)
			}
		}
		var first struct {
			AgentID   string          `json:"agent_id"`
			Turn      int             `json:"turn"`
			Type      string          `json:"type"`
			Timestamp time.Time       `json:"timestamp"`
			Payload   json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal([]byte(got[1].data), &first); err != nil {
			t.Fatal(err)
		}
		if first.AgentID != "a" || first.Turn != 1 || first.Type != "turn_start" ||
			!first.Timestamp.Equal(ts) || string(first.Payload) != `{"input":"hi"}` {
			t.Errorf("turn_start = %+v", first)
		}
		if !strings.Contains(got[2].data, `"payload":{"bytes":`) || !strings.Contains(got[2].data, `"omitted":true`) {
			t.Errorf("a payload over the cap was sent: %.200s", got[2].data)
		}
		mu.Lock()
		defer mu.Unlock()
		if m := asked[len(asked)-1]; m.After != 0 || m.Limit != 20 {
			t.Errorf("daemon asked after=%d tail=%d, want 0 and 20", m.After, m.Limit)
		}
	})

	t.Run("filtered, resumed from Last-Event-ID", func(t *testing.T) {
		h := http.Header{"Last-Event-Id": []string{"5"}}
		got := readSSE(t, srv.URL+"/api/v1/events?type=goal_*&agent=g1&after=2", h, 3)
		if got[0].event != "connected" || got[0].id != "5" {
			t.Errorf("handshake = %+v", got[0])
		}
		if got[1].event != "journal" || got[1].id != "8" {
			t.Errorf("block 1 = %+v, want only goal_created", got[1])
		}
		// The filtered-out tool_end still moves the client's position.
		if got[2] != (sseBlock{id: "9"}) {
			t.Errorf("block 2 = %+v, want a bare id 9", got[2])
		}
		mu.Lock()
		defer mu.Unlock()
		if m := asked[len(asked)-1]; m.After != 5 {
			t.Errorf("daemon asked after=%d, want Last-Event-ID's 5 over after=2", m.After)
		}
	})

	t.Run("exact types", func(t *testing.T) {
		got := readSSE(t, srv.URL+"/api/v1/events?type=tool_end&type=turn_start", nil, 3)
		if got[1].id != "6" || got[2].id != "8" || got[2].event != "" {
			t.Errorf("blocks = %+v, want turn_start then a bare id 8", got)
		}
	})
}

func TestStreamEventsReset(t *testing.T) {
	sock := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		return []protocol.Msg{{Type: protocol.TypeJournalFollow, After: 40, Status: protocol.StatusJournalReset}}
	})
	srv := httptest.NewServer(apiServerAt(sock))
	defer srv.Close()

	got := readSSE(t, srv.URL+"/api/v1/events?after=3", nil, 2)
	if got[0].event != "connected" || got[1].event != "reset" || got[1].id != "40" || got[1].data != `{"seq":40}` {
		t.Errorf("blocks = %+v, want connected then reset at 40", got)
	}
}

func TestStreamEventsRejectsBadPositions(t *testing.T) {
	sock := scriptedDaemon(t, func(protocol.Msg) []protocol.Msg { return errReply("not reached") })
	h := apiServerAt(sock)
	for _, c := range []struct {
		name, query, lastEventID string
	}{
		{"Last-Event-ID not a seq", "", "abc"},
		{"negative Last-Event-ID", "", "-1"},
		{"tail too large", "?tail=5000", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/v1/events"+c.query, nil)
			if c.lastEventID != "" {
				r.Header.Set("Last-Event-ID", c.lastEventID)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status %d, want 400 (body %s)", w.Code, w.Body.String())
			}
		})
	}
}

func TestListQuestions(t *testing.T) {
	exp := time.Date(2026, 10, 13, 13, 0, 0, 0, time.UTC)
	sock := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		if m.Type != protocol.TypeListHumanRequests {
			return errReply("unexpected")
		}
		b, _ := json.Marshal([]protocol.PendingQuestion{
			{ID: "q1", AgentID: "conv1", Question: "Ship it?", Options: []string{"yes", "no"}, CreatedAt: "2026-10-13T12:00:00Z", ExpiresAt: exp},
			{ID: "q2", AgentID: "conv2", Question: "Why?", CreatedAt: "2026-10-13T12:01:00Z", ExpiresAt: exp},
		})
		return []protocol.Msg{protocol.NewTextMsg(protocol.TypeListHumanRequests, string(b))}
	})
	w := httptest.NewRecorder()
	apiServerAt(sock).ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/questions", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Data []struct {
			ID       string    `json:"id"`
			AgentID  string    `json:"agent_id"`
			Question string    `json:"question"`
			Options  []string  `json:"options"`
			Expires  time.Time `json:"expires_at"`
		} `json:"data"`
	}
	decodeJSON(t, w, &got)
	if len(got.Data) != 2 || got.Data[0].AgentID != "conv1" || len(got.Data[0].Options) != 2 ||
		got.Data[1].Options != nil || !got.Data[0].Expires.Equal(exp) {
		t.Errorf("questions = %+v", got)
	}
}
