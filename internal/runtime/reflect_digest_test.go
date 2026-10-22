package runtime

import (
	"encoding/json"
	"strings"
	"testing"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

func journal(t *testing.T, store *memory.Store, agentID, typ string, payload map[string]any) {
	t.Helper()
	b, _ := json.Marshal(payload)
	if err := store.SessionEventsAppend([]memory.SessionEvent{{AgentID: agentID, Turn: 1, SpanID: "s", Type: typ, Payload: b}}); err != nil {
		t.Fatal(err)
	}
}

// The digest names what other sessions did since the cursor — their turns and
// tokens, failures, repeated-call stops and process changes — leaves out the
// reflecting session's own events, and is empty when nothing else happened.
func TestActivityDigest(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	r := &StandingRunner{store: store, log: newStandingLog()}
	if err := store.GoalCreate("g1", "Keep up with Canadian news", "", "operator"); err != nil {
		t.Fatal(err)
	}
	start, _ := store.LastEventSeq("g1")

	journal(t, store, "self-reflection", "turn_end", map[string]any{"result": "reflected"})
	if d, _, err := r.activityDigest("self-reflection", start); err != nil || d != "" {
		t.Fatalf("digest of only its own turn = %q, %v; want empty", d, err)
	}

	journal(t, store, "g1", "llm_response", map[string]any{"input_tokens": 40000, "output_tokens": 100})
	journal(t, store, "g1", "turn_end", map[string]any{"error": "llm: mistral: HTTP 429"})
	journal(t, store, "g1", "turn_end", map[string]any{"result": "Stopped this turn: goal_get was called 5 times with the same arguments and kept returning the same result."})
	journal(t, store, "g1", "tool_end", map[string]any{"name": "web_page_read", "err": "timeout"})
	journal(t, store, "goal:g1", "process_paused", map[string]any{"by": "budget"})

	d, last, err := r.activityDigest("self-reflection", start)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`goal "Keep up with Canadian news" (g1): 2 turns, 40100 tokens`,
		"1x llm: mistral: HTTP 429",
		"web_page_read: 1",
		"goal_get was called 5 times",
		"process_paused goal:g1",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("digest lacks %q:\n%s", want, d)
		}
	}
	if d2, _, _ := r.activityDigest("self-reflection", last); d2 != "" {
		t.Errorf("digest after the last seq read = %q, want empty", d2)
	}
}

// A reflect process's tick with nothing new is skipped and logged; the next
// one, after another session's turn, carries the digest.
func TestReflectionDigestSkipsAQuietTick(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	r := &StandingRunner{store: store, log: newStandingLog()}
	journal(t, store, "self-reflection", "turn_end", map[string]any{"result": "reflected"})
	lp := &liveProc{row: memory.Process{ID: "self-reflection", Tool: "reflect", SessionID: "self-reflection"}}

	if _, ok := r.reflectionDigest(lp); ok {
		t.Error("a tick with nothing new was not skipped")
	}
	if e := r.log.recent("self-reflection", 1); len(e) != 1 || e[0].Outcome != "skipped" {
		t.Errorf("log = %+v, want the skip", e)
	}
	journal(t, store, "c1", "turn_end", map[string]any{"result": "hi"})
	if d, ok := r.reflectionDigest(lp); !ok || !strings.Contains(d, "c1: 1 turns") {
		t.Errorf("digest after a conversation's turn = %q, %v", d, ok)
	}
}
