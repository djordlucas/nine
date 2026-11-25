package ninectx

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"nine/internal/llm"
)

func msg(role, text string) llm.Message { return llm.Message{Role: role, Text: text} }

func TestCutIndexPrefersUserAndNeverToolResult(t *testing.T) {
	messages := []llm.Message{
		msg("user", "first task"),
		msg("assistant", "answer one"),
		{Role: "user", ToolResults: []llm.ToolResult{{ToolCallID: "call_1", Content: "big output"}}},
		msg("assistant", "answer two"),
		msg("user", "second task"),
		msg("assistant", "final answer"),
	}
	// A generous keep budget keeps from the last user message.
	if got := CutIndex(messages, 1_000_000); got != 4 {
		t.Fatalf("generous budget: cut = %d, want 4 (last user message)", got)
	}
	// A single-message history has nothing to fold.
	if got := CutIndex(messages[:1], 1_000_000); got != 0 {
		t.Fatalf("single message: cut = %d, want 0", got)
	}
	msize := func(i int) int {
		b, _ := json.Marshal(messages[i])
		return len(b)
	}
	// A tight budget keeps only the tail; the cut must not land on the
	// tool-result message (index 2).
	keep := msize(5) + 10
	got := CutIndex(messages, keep)
	if got == 2 {
		t.Fatalf("cut landed on a tool-result message: %d", got)
	}
	if got != 5 {
		t.Fatalf("tight budget: cut = %d, want 5 (final assistant message)", got)
	}
	// A budget that fits the last user message cuts there, keeping whole turns.
	keep = msize(4) + msize(5) + 10
	if got := CutIndex(messages, keep); got != 4 {
		t.Fatalf("medium budget: cut = %d, want 4 (last user message)", got)
	}
}

func TestIsOverflow(t *testing.T) {
	cases := map[string]bool{
		`llm: 400 Bad Request: context length exceeded`:        true,
		"422: prompt is too long for the model":                 true,
		"request 413: total tokens exceed the context window":   true,
		"500 internal error":                                    false,
		"llm: connection refused":                               false,
	}
	for text, want := range cases {
		if got := IsOverflow(errors.New(text)); got != want {
			t.Errorf("IsOverflow(%q) = %v, want %v", text, got, want)
		}
	}
	if IsOverflow(nil) {
		t.Error("IsOverflow(nil) = true, want false")
	}
}

func TestRequestCharsAndEstimate(t *testing.T) {
	chars := RequestChars("system", []llm.Message{msg("user", "hi")}, nil)
	if chars <= len("system") {
		t.Fatalf("RequestChars too small: %d", chars)
	}
	if EstimateTokensFromChars(3450) != 1000 {
		t.Fatalf("EstimateTokensFromChars(3450) = %d, want 1000", EstimateTokensFromChars(3450))
	}
	if EstimateTokensToChars(1000) != 3450 {
		t.Fatalf("EstimateTokensToChars(1000) = %d, want 3450", EstimateTokensToChars(1000))
	}
}

func TestSummarizeMergesChunks(t *testing.T) {
	// A window small enough that two messages do not fit one chunk.
	window := 100
	_ = EstimateTokensToChars(int(float64(window) * ChunkShare)) // 50 tokens → ~172 chars
	messages := []llm.Message{
		msg("user", strings.Repeat("a", 200)),
		msg("assistant", strings.Repeat("b", 200)),
	}
	var prompts []string
	submit := func(_ context.Context, _ int, req llm.Request) (llm.Response, error) {
		prompts = append(prompts, req.Messages[0].Text)
		if req.System != SummarizerPrompt {
			t.Fatalf("summarizer system prompt mismatch")
		}
		n := len(prompts)
		return llm.Response{Text: strings.Repeat("s", 10) + string(rune('0'+n))}, nil
	}
	out, err := Summarize(context.Background(), submit, llm.PriorityBackground, "", messages, window)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if len(prompts) != 2 {
		t.Fatalf("expected 2 chunk calls, got %d", len(prompts))
	}
	if !strings.Contains(prompts[1], "The summary so far") {
		t.Error("second chunk did not carry the previous summary")
	}
	if out != strings.Repeat("s", 10)+"2" {
		t.Errorf("summary = %q, want the last chunk's summary", out)
	}
}

func TestSummarizeEmpty(t *testing.T) {
	submit := func(context.Context, int, llm.Request) (llm.Response, error) {
		t.Fatal("submit called for empty conversation")
		return llm.Response{}, nil
	}
	if _, err := Summarize(context.Background(), submit, 1, "", nil, 1000); err == nil {
		t.Error("empty conversation without previous summary should error")
	}
	out, err := Summarize(context.Background(), submit, 1, "previous", nil, 1000)
	if err != nil || out != "previous" {
		t.Errorf("previous-only: out=%q err=%v", out, err)
	}
}

func TestRenderAndClip(t *testing.T) {
	m := llm.Message{Role: "assistant", Text: "hello", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "shell", Input: json.RawMessage(`{"cmd":"ls"}`)}}}
	r := Render(m)
	if !strings.Contains(r, "[assistant]") || !strings.Contains(r, "hello") || !strings.Contains(r, "→ shell(") {
		t.Errorf("Render assistant = %q", r)
	}
	tr := llm.Message{Role: "user", ToolResults: []llm.ToolResult{{ToolCallID: "call_1", Content: "out"}}}
	if r := Render(tr); !strings.Contains(r, "[result of tool call call_1]") {
		t.Errorf("Render tool result = %q", r)
	}
	long := strings.Repeat("x", MaxMessageChars+100)
	c := clip(long)
	if !strings.Contains(c, "characters cut") || len(c) < MaxMessageChars {
		t.Errorf("clip did not cut: len=%d", len(c))
	}
}
