package cli

import (
	"strings"
	"testing"

	ninectx "nine/internal/context"
	"nine/internal/llm"
	"nine/internal/protocol"
)

func TestPrintContextReport(t *testing.T) {
	rep := &ninectx.Report{
		Budget: 1000,
		Used:   250,
		Sections: []ninectx.Section{
			{Name: "system-core", Priority: "1", Tokens: 200, Included: true, Detail: "always included"},
			{Name: "extras", Priority: "5", Tokens: 0, Included: false, Detail: "dropped: needs 200 free tokens"},
		},
		System:   "SYSTEM_PROMPT_MARKER",
		Messages: []llm.Message{{Role: "user", Text: "hi"}},
	}

	// Concise mode: totals + section table, no full prompt dump.
	var b strings.Builder
	printContextReport(&b, rep, false)
	out := b.String()
	if !strings.Contains(out, "250 / 1000") || !strings.Contains(out, "25%") {
		t.Errorf("concise output missing totals:\n%s", out)
	}
	if !strings.Contains(out, "system-core") || !strings.Contains(out, "dropped: needs 200") {
		t.Errorf("concise output missing section rows:\n%s", out)
	}
	if strings.Contains(out, "SYSTEM_PROMPT_MARKER") {
		t.Errorf("concise output should not dump the full system prompt:\n%s", out)
	}

	// Verbose mode: dumps the assembled system prompt and messages.
	var vb strings.Builder
	printContextReport(&vb, rep, true)
	vout := vb.String()
	if !strings.Contains(vout, "SYSTEM_PROMPT_MARKER") {
		t.Errorf("verbose output missing system prompt:\n%s", vout)
	}
	if !strings.Contains(vout, "user") {
		t.Errorf("verbose output missing message dump:\n%s", vout)
	}
}

func TestPrintNotifications(t *testing.T) {
	var b strings.Builder
	raw := `{"notifications":[
		{"agent_id":"sec-watch","message":"CVE affects deps","seen":false,"created_at":"2026-07-06T09:00:00Z"},
		{"agent_id":"","message":"system note","seen":true,"created_at":"2026-07-06T10:00:00Z"}
	]}`
	printNotifications(&b, raw)
	out := b.String()
	if !strings.Contains(out, "sec-watch") || !strings.Contains(out, "CVE affects deps") {
		t.Errorf("output missing agent-attributed notification:\n%s", out)
	}
	// An empty agent_id falls back to the "nine" label.
	if !strings.Contains(out, "nine") || !strings.Contains(out, "system note") {
		t.Errorf("output missing daemon-attributed notification:\n%s", out)
	}
}

func TestPrintNotificationsEmpty(t *testing.T) {
	var b strings.Builder
	printNotifications(&b, `{"notifications":[]}`)
	if got := strings.TrimSpace(b.String()); got != "no notifications" {
		t.Errorf("empty feed printed %q, want %q", got, "no notifications")
	}
}

func TestPrintStatusRolesAndQueue(t *testing.T) {
	var b strings.Builder
	printStatus(&b, &protocol.StatusInfo{
		Uptime: "3m0s",
		Agents: []protocol.AgentInfo{
			{ID: "abcdef012345", Name: "sec-watch", Role: "orchestrator"},
			{ID: "99887766aaaa", Role: "pursue"}, // unnamed session
		},
		SubAgents: []protocol.SubAgentInfo{
			{ID: "sub12345xyz", Description: "audit the parser", Role: "executor"},
		},
		LLMQueue: &protocol.QueueStat{Pending: 2, Inflight: 1, MaxConcurrent: 4},
		Plugins:  []string{"browser"},
	})
	out := b.String()

	// The session's role rides alongside its name/ID.
	if !strings.Contains(out, "sec-watch") || !strings.Contains(out, "orchestrator") {
		t.Errorf("named agent role missing:\n%s", out)
	}
	// An unnamed session still shows its role.
	if !strings.Contains(out, "pursue") {
		t.Errorf("unnamed agent role missing:\n%s", out)
	}
	// Sub-agents carry their leaf role next to the task.
	if !strings.Contains(out, "executor") || !strings.Contains(out, "audit the parser") {
		t.Errorf("sub-agent role/desc missing:\n%s", out)
	}
	// LLM queue depth is surfaced.
	if !strings.Contains(out, "1 inflight") || !strings.Contains(out, "2 waiting") || !strings.Contains(out, "max 4") {
		t.Errorf("queue depth missing:\n%s", out)
	}
}

func TestPrintStatusNoQueueStat(t *testing.T) {
	// A daemon with no queue-stat function wired (LLMQueue nil) omits the line
	// rather than printing zeroes.
	var b strings.Builder
	printStatus(&b, &protocol.StatusInfo{Uptime: "1s", Agents: nil})
	if strings.Contains(b.String(), "LLM queue") {
		t.Errorf("queue line should be omitted when LLMQueue is nil:\n%s", b.String())
	}
}
