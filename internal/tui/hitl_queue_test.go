package tui

import (
	"strings"
	"testing"

	"nine/internal/protocol"
)

func req(id, question, origin string) *protocol.HumanRequest {
	return &protocol.HumanRequest{RequestID: id, Question: question, Origin: origin}
}

// Parallel sub-agents can each hit an approval gate at once (R-HITL.5), so
// questions queue instead of the newest overwriting the one on screen. The
// head is what the user answers; answering uncovers the next.
func TestHumanQueueAnswersOldestFirst(t *testing.T) {
	var c chatState

	if c.pendingHuman() != nil {
		t.Fatal("a fresh chat has no pending question")
	}

	c.humanQueue = append(c.humanQueue, req("r1", "first?", ""))
	c.humanQueue = append(c.humanQueue, req("r2", "second?", `sub-agent "executor" · b-task`))
	c.humanQueue = append(c.humanQueue, req("r3", "third?", `sub-agent "monitor" · c-task`))

	if got := c.pendingHuman(); got.RequestID != "r1" {
		t.Errorf("on-screen question = %q, want the oldest (r1)", got.RequestID)
	}

	if next := c.popHuman(); next == nil || next.RequestID != "r2" {
		t.Fatalf("after answering r1, next = %v, want r2", next)
	}
	if got := c.pendingHuman(); got.RequestID != "r2" {
		t.Errorf("on-screen question = %q, want r2", got.RequestID)
	}
	if next := c.popHuman(); next == nil || next.RequestID != "r3" {
		t.Fatalf("after answering r2, next = %v, want r3", next)
	}
	// Answering the last one leaves nothing on screen.
	if next := c.popHuman(); next != nil {
		t.Errorf("after answering r3, next = %v, want nil", next)
	}
	if c.pendingHuman() != nil {
		t.Error("queue should be empty")
	}
	// Popping an empty queue is a no-op, not a panic.
	if next := c.popHuman(); next != nil {
		t.Errorf("popping an empty queue = %v, want nil", next)
	}
}

// A sub-agent's question is attributed, so the user can tell who wants
// permission for a tool they never saw requested. The session's own questions
// are rendered bare, exactly as before.
func TestFormatQuestionAttribution(t *testing.T) {
	own := formatQuestion(req("r1", `Run tool "shell"?`, ""))
	if strings.Contains(own, "[") {
		t.Errorf("own question = %q, want no attribution prefix", own)
	}
	if own != `Run tool "shell"?` {
		t.Errorf("own question = %q, want the bare question", own)
	}

	delegated := formatQuestion(req("r2", `Run tool "shell"?`, `sub-agent "executor" · audit the repo`))
	if !strings.HasPrefix(delegated, `[sub-agent "executor" · audit the repo]`) {
		t.Errorf("delegated question = %q, want the origin prefixed", delegated)
	}
	if !strings.Contains(delegated, `Run tool "shell"?`) {
		t.Errorf("delegated question = %q, want the question retained", delegated)
	}
}

// Options still render under the question, with or without attribution.
func TestFormatQuestionOptions(t *testing.T) {
	hr := req("r1", "Pick one", "sub-agent \"executor\" · x")
	hr.Options = []string{"alpha", "beta"}
	got := formatQuestion(hr)
	for _, want := range []string{"1) alpha", "2) beta", "sub-agent"} {
		if !strings.Contains(got, want) {
			t.Errorf("formatQuestion = %q, want it to contain %q", got, want)
		}
	}
}
