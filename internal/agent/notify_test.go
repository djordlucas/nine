package agent_test

import (
	"context"
	"encoding/json"
	"testing"

	"nine/internal/agent"
)

func TestNotifyUser(t *testing.T) {
	d := agent.New()
	var gotAgent, gotText string
	calls := 0
	agent.RegisterNotifyUser(d, "sec-watch", func(agentID, text string) {
		calls++
		gotAgent, gotText = agentID, text
	})

	res, err := d.Dispatch(context.Background(), "notify_user",
		json.RawMessage(`{"text":"CVE-2026-1 affects our deps"}`))
	if err != nil {
		t.Fatalf("notify_user: %v", err)
	}
	if calls != 1 {
		t.Fatalf("add called %d times, want 1", calls)
	}
	if gotAgent != "sec-watch" || gotText != "CVE-2026-1 affects our deps" {
		t.Errorf("add(%q, %q), want (sec-watch, CVE...)", gotAgent, gotText)
	}
	if res.Output == "" {
		t.Error("notify_user returned empty confirmation")
	}

	// Empty text is rejected and does not post.
	if _, err := d.Dispatch(context.Background(), "notify_user", json.RawMessage(`{"text":"  "}`)); err == nil {
		t.Error("empty notify_user text should error")
	}
	if calls != 1 {
		t.Errorf("add called %d times after empty text, want still 1", calls)
	}
}
