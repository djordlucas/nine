package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"nine/internal/memory"
)

// sampleTurn returns a one-turn journal matching the shape produced by
// internal/runtime/journal.go: turn_start → llm_request/response → tool
// start/end → turn_end.
func sampleTurn() []memory.SessionEvent {
	mustJSON := func(v any) json.RawMessage {
		b, err := json.Marshal(v)
		if err != nil {
			panic(err)
		}
		return b
	}
	return []memory.SessionEvent{
		{Seq: 1, Turn: 1, SpanID: "t1", Type: "turn_start",
			Payload: mustJSON(map[string]any{"input": "what is it?", "trigger": "user"})},
		{Seq: 2, Turn: 1, SpanID: "t1.llm1", ParentSpanID: "t1", Type: "llm_request",
			Payload: mustJSON(map[string]any{
				"system": "you are nine", "tool_names": []string{"echo", "memory_get"},
				"tokens_used": 948, "budget": 100000, "llm_call_n": 1,
				"messages": []map[string]any{{"Role": "user", "Text": "what is it?"}},
			})},
		{Seq: 3, Turn: 1, SpanID: "t1.llm1", ParentSpanID: "t1", Type: "llm_response",
			Payload: mustJSON(map[string]any{
				"text": "let me check", "stop_reason": "tool_use", "llm_call_n": 1,
				"tool_calls": []map[string]any{{"Name": "echo", "Input": json.RawMessage(`{"v":"hi"}`)}},
			})},
		{Seq: 4, Turn: 1, SpanID: "t1.tool1", ParentSpanID: "t1.llm1", Type: "tool_start",
			Payload: mustJSON(map[string]any{"name": "echo", "input": json.RawMessage(`{"v":"hi"}`)})},
		{Seq: 5, Turn: 1, SpanID: "t1.tool1", ParentSpanID: "t1.llm1", Type: "tool_end",
			Payload: mustJSON(map[string]any{
				"name": "echo", "output": "echoed:hi", "duration_ms": 4, "attempts": 1,
			})},
		{Seq: 6, Turn: 1, SpanID: "t1", Type: "turn_end",
			Payload: mustJSON(map[string]any{"result": "the answer is hi", "tool_count": 1, "duration_ms": 42})},
	}
}

func TestFormatTrace(t *testing.T) {
	var b strings.Builder
	formatTrace(&b, "agent-x", sampleTurn(), 0)
	out := b.String()

	for _, want := range []string{
		"session agent-x — 6 events across 1 turn(s)",
		`user: "what is it?"`,
		"call 1 · 948/100000 tok · tools: echo,memory_get",
		"tool_use · 1 tool call(s)",
		"echo · 4ms · ok",
		"ok · 1 tools · 42ms",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("trace output missing %q:\n%s", want, out)
		}
	}
}

func TestFormatTraceEmpty(t *testing.T) {
	var b strings.Builder
	formatTrace(&b, "ghost", nil, 0)
	if got := strings.TrimSpace(b.String()); got != "no events recorded for ghost" {
		t.Errorf("got %q", got)
	}

	// Turn filter with no matching events.
	b.Reset()
	formatTrace(&b, "agent-x", sampleTurn(), 9)
	if got := strings.TrimSpace(b.String()); got != "no events for turn 9 of agent-x" {
		t.Errorf("got %q", got)
	}
}

func TestFormatTraceTurnFilter(t *testing.T) {
	events := sampleTurn()
	// Add a second turn; the filter must exclude it.
	events = append(events, memory.SessionEvent{Seq: 7, Turn: 2, Type: "turn_start",
		Payload: json.RawMessage(`{"input":"again","trigger":"idle"}`)})

	var b strings.Builder
	formatTrace(&b, "agent-x", events, 1)
	out := b.String()
	if strings.Contains(out, "again") || strings.Contains(out, "idle") {
		t.Errorf("turn filter leaked turn 2:\n%s", out)
	}
	if !strings.Contains(out, "what is it?") {
		t.Errorf("turn filter dropped turn 1:\n%s", out)
	}
}

func TestFormatReplay(t *testing.T) {
	var b strings.Builder
	formatReplay(&b, "agent-x", sampleTurn(), 1)
	out := b.String()

	for _, want := range []string{
		"session agent-x · turn 1",
		"trigger: user",
		"input:   what is it?",
		"── LLM call 1 ",
		"request:  948/100000 tokens · 1 msg window · tools: echo, memory_get",
		"system:   you are nine",
		"response: stop=tool_use",
		"text: let me check",
		`→ call echo {"v":"hi"}`,
		"tool echo  (4ms, 1 attempt(s), ok)",
		"output: echoed:hi",
		"result (ok · 1 tools · 42ms): the answer is hi",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("replay output missing %q:\n%s", want, out)
		}
	}
}

func TestFormatReplayMissingTurn(t *testing.T) {
	var b strings.Builder
	formatReplay(&b, "agent-x", sampleTurn(), 5)
	if got := strings.TrimSpace(b.String()); got != "no events for turn 5 of agent-x" {
		t.Errorf("got %q", got)
	}
}

func TestParseTurnFlag(t *testing.T) {
	cases := []struct {
		args []string
		want int
		err  bool
	}{
		{nil, 0, false},
		{[]string{"--turn", "3"}, 3, false},
		{[]string{"--turn=7"}, 7, false},
		{[]string{"--turn"}, 0, true},
		{[]string{"--turn", "x"}, 0, true},
		{[]string{"--bogus"}, 0, true},
	}
	for _, tc := range cases {
		got, err := parseTurnFlag(tc.args)
		if (err != nil) != tc.err {
			t.Errorf("parseTurnFlag(%v) err=%v, wantErr=%v", tc.args, err, tc.err)
		}
		if got != tc.want {
			t.Errorf("parseTurnFlag(%v) = %d, want %d", tc.args, got, tc.want)
		}
	}
}
