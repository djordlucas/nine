package runtime

import (
	"encoding/json"
	"testing"
	"time"

	"nine/internal/memory"
	"nine/internal/toolvm"
)

// The hook was declared, called, and tested inside toolvm for a long time while
// nothing in production ever set it — so "what did this tool reach, and which
// turn asked for it" was answerable from the log but not from the journal. This
// asserts the wiring itself, which is the part that was missing.
func TestToolHTTPReachesTheJournal(t *testing.T) {
	sink := &captureSink{}
	w := &AgentWorker{id: "agent-1", sink: sink}
	w.toolN = 2 // a tool call is in flight

	audit := w.httpAuditor(7)
	audit(toolvm.HTTPCall{
		Tool:     "weather",
		Method:   "GET",
		URL:      "https://api.example/v1?q=x",
		Host:     "api.example",
		Status:   200,
		Bytes:    1234,
		Duration: 42 * time.Millisecond,
	})

	if len(sink.events) != 1 {
		t.Fatalf("journaled %d events, want 1", len(sink.events))
	}
	ev := sink.events[0]
	if ev.Type != "tool_http" {
		t.Errorf("Type = %q", ev.Type)
	}
	if ev.AgentID != "agent-1" || ev.Turn != 7 {
		t.Errorf("AgentID/Turn = %q/%d", ev.AgentID, ev.Turn)
	}
	// Nested under the tool call that made it, not beside the turn — otherwise a
	// trace shows requests with no indication of which call caused them.
	if ev.ParentSpanID != "t7.tool2" {
		t.Errorf("ParentSpanID = %q, want t7.tool2", ev.ParentSpanID)
	}
	if ev.SpanID != "t7.tool2.http" {
		t.Errorf("SpanID = %q", ev.SpanID)
	}

	var p toolHTTPPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Tool != "weather" || p.Method != "GET" || p.Host != "api.example" {
		t.Errorf("payload = %+v", p)
	}
	if p.Status != 200 || p.Bytes != 1234 || p.DurationMs != 42 {
		t.Errorf("payload = %+v", p)
	}
}

// A blocked request is the one an operator most wants to find later, so it must
// journal too — with no status, which is how a refusal is distinguishable from
// a response.
func TestBlockedToolHTTPIsJournaled(t *testing.T) {
	sink := &captureSink{}
	w := &AgentWorker{id: "a", sink: sink}
	w.httpAuditor(1)(toolvm.HTTPCall{Tool: "scraper", Method: "GET", URL: "http://169.254.169.254/", Host: "169.254.169.254"})

	if len(sink.events) != 1 {
		t.Fatalf("journaled %d events, want 1", len(sink.events))
	}
	var p toolHTTPPayload
	if err := json.Unmarshal(sink.events[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Status != 0 {
		t.Errorf("Status = %d, want 0 for a call that never got a response", p.Status)
	}
	if p.Host != "169.254.169.254" {
		t.Errorf("Host = %q", p.Host)
	}
}

type captureSink struct{ events []memory.SessionEvent }

func (s *captureSink) Append(ev memory.SessionEvent) { s.events = append(s.events, ev) }
func (s *captureSink) Close() error                  { return nil }
