package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"nine/internal/protocol"
)

// Truncating free text by byte offset corrupts anything outside ASCII: the cut
// lands mid-rune and the terminal renders a replacement character. Goal
// descriptions and workflow names are written by a model or a user, so non-ASCII
// is ordinary input, not an edge case.
func TestClipNeverProducesInvalidUTF8(t *testing.T) {
	inputs := []string{
		"日本語のゴールの説明でありこれは非常に長いテキストです",
		"Überwachung — Sicherheitsprüfung für alle Abhängigkeiten im Repository",
		"🎯 monitor the repository for security issues and triage new CVEs daily",
		strings.Repeat("é", 100),
		"ascii only, comfortably longer than the forty character limit imposed",
	}
	for _, in := range inputs {
		for _, max := range []int{1, 2, 3, 4, 8, 12, 40, 52} {
			got := clip(in, max)
			if !utf8.ValidString(got) {
				t.Errorf("clip(%q, %d) = %q, which is not valid UTF-8", in, max, got)
			}
			if n := utf8.RuneCountInString(got); n > max {
				t.Errorf("clip(%q, %d) returned %d runes", in, max, n)
			}
		}
	}
}

func TestClipKeepsShortStringsWhole(t *testing.T) {
	for _, s := range []string{"", "short", "日本語", "exactly-ten"} {
		if got := clip(s, 40); got != s {
			t.Errorf("clip(%q, 40) = %q, want it unchanged", s, got)
		}
	}
}

func TestClipMarksTruncation(t *testing.T) {
	got := clip("a description that is definitely longer than the limit", 20)
	if !strings.HasSuffix(got, "...") {
		t.Errorf("clip = %q, want a trailing ellipsis to show it was cut", got)
	}
	if utf8.RuneCountInString(got) != 20 {
		t.Errorf("clip = %q (%d runes), want exactly 20", got, utf8.RuneCountInString(got))
	}
}

// shortID replaced `id[:8]`, which panicked on anything shorter. The id comes
// from the daemon over the wire, so a malformed or truncated one took the whole
// CLI down rather than printing a short id.
func TestShortIDDoesNotPanicOnShortInput(t *testing.T) {
	for _, s := range []string{"", "a", "abc", "1234567", "12345678", "123456789"} {
		got := shortID(s, 8)
		if utf8.RuneCountInString(got) > 8 {
			t.Errorf("shortID(%q, 8) = %q, too long", s, got)
		}
		if len(s) <= 8 && got != s {
			t.Errorf("shortID(%q, 8) = %q, want it unchanged", s, got)
		}
	}
}

// An ID column has never carried an ellipsis; keeping that exact is what makes
// this a bug fix rather than an output change.
func TestShortIDHasNoEllipsis(t *testing.T) {
	got := shortID("0f2a1b3c-4d5e-6f70-8901-234567890abc", 12)
	if strings.Contains(got, ".") {
		t.Errorf("shortID = %q, want a plain prefix with no ellipsis", got)
	}
	if got != "0f2a1b3c-4d5" {
		t.Errorf("shortID = %q, want the first 12 characters", got)
	}
}

func TestPrintGoals(t *testing.T) {
	t.Run("renders rows", func(t *testing.T) {
		var b bytes.Buffer
		printGoals(&b, `{"goals":[
			{"id":"goal-1","description":"monitor the repo","status":"active"},
			{"id":"goal-2","description":"keep deps fresh","status":"paused"}]}`)
		out := b.String()
		for _, want := range []string{"ID", "STATUS", "DESCRIPTION", "goal-1", "active", "monitor the repo", "paused"} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q:\n%s", want, out)
			}
		}
	})

	// An empty list and malformed JSON must both say so rather than print a
	// header over nothing, or panic.
	t.Run("empty and malformed", func(t *testing.T) {
		for _, raw := range []string{`{"goals":[]}`, `{}`, `not json`, ``} {
			var b bytes.Buffer
			printGoals(&b, raw)
			if !strings.Contains(b.String(), "no goals") {
				t.Errorf("printGoals(%q) = %q, want \"no goals\"", raw, b.String())
			}
		}
	})

	t.Run("non-ascii description survives", func(t *testing.T) {
		var b bytes.Buffer
		printGoals(&b, `{"goals":[{"id":"g1","description":"日本語のゴールの説明でありこれは非常に長いテキストです","status":"active"}]}`)
		if !utf8.ValidString(b.String()) {
			t.Errorf("output is not valid UTF-8: %q", b.String())
		}
	})
}

func TestPrintWorkflows(t *testing.T) {
	var b bytes.Buffer
	printWorkflows(&b, `{"workflows":[{"id":"wf-1","name":"migrate to gRPC","status":"active",
		"steps":[{"id":"s1","label":"audit endpoints","status":"done"},
		         {"id":"s2","label":"define protobufs","status":"pending"}]}]}`)
	out := b.String()
	for _, want := range []string{"wf-1", "migrate to gRPC", "audit endpoints", "done", "define protobufs", "pending"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	for _, raw := range []string{`{"workflows":[]}`, `nonsense`} {
		var e bytes.Buffer
		printWorkflows(&e, raw)
		if !strings.Contains(e.String(), "no workflows") {
			t.Errorf("printWorkflows(%q) = %q", raw, e.String())
		}
	}
}

func TestPrintNotifications(t *testing.T) {
	var b bytes.Buffer
	printNotifications(&b, `{"notifications":[
		{"agent_id":"sec-watch","message":"found a CVE","created_at":"2026-08-19T10:00:00Z"},
		{"agent_id":"","message":"from the daemon itself","created_at":"2026-08-19T11:00:00Z"}]}`)
	out := b.String()
	if !strings.Contains(out, "sec-watch") || !strings.Contains(out, "found a CVE") {
		t.Errorf("output missing the agent notification:\n%s", out)
	}
	// An empty agent id is the daemon speaking; it must be labelled, not blank.
	if !strings.Contains(out, "nine") {
		t.Errorf("an agent-less notification should be attributed to \"nine\":\n%s", out)
	}
}

// A short agent id must not take the CLI down. This is the case that used to
// panic on `sa.ID[:8]`.
func TestPrintStatusHandlesShortIDs(t *testing.T) {
	var b bytes.Buffer
	printStatus(&b, &protocol.StatusInfo{
		Uptime: "1h",
		Agents: []protocol.AgentInfo{
			{ID: "ab", Role: "orchestrator"},
			{ID: "0f2a1b3c-4d5e-6f70", Name: "atlas", Role: "pursue"},
		},
		SubAgents: []protocol.SubAgentInfo{
			{ID: "xy", Description: "a short-id sub-agent"},
			{ID: "", Description: "no id at all"},
		},
	})
	out := b.String()
	for _, want := range []string{"1h", "ab", "atlas", "a short-id sub-agent"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestParseVerboseFlag(t *testing.T) {
	cases := []struct {
		args    []string
		want    bool
		wantErr bool
	}{
		{nil, false, false},
		{[]string{}, false, false},
		{[]string{"--verbose"}, true, false},
		{[]string{"-v"}, true, false},
		{[]string{"--wrong"}, false, true},
		{[]string{"extra-arg"}, false, true},
	}
	for _, tc := range cases {
		got, err := parseVerboseFlag(tc.args)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseVerboseFlag(%v) err = %v, wantErr %v", tc.args, err, tc.wantErr)
		}
		if got != tc.want {
			t.Errorf("parseVerboseFlag(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// `nine send` without --id continues the last conversation, which only works if
// the id round-trips through disk. A silent failure here looks like Nine
// forgetting the conversation, with no error anywhere.
func TestConversationIDRoundTrips(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if got := loadConversationID(); got != "" {
		t.Errorf("loadConversationID on a fresh home = %q, want empty", got)
	}

	saveConversationID("agent-42")
	if got := loadConversationID(); got != "agent-42" {
		t.Errorf("loadConversationID = %q, want agent-42", got)
	}

	// Overwriting replaces rather than appends.
	saveConversationID("agent-99")
	if got := loadConversationID(); got != "agent-99" {
		t.Errorf("after overwrite = %q, want agent-99", got)
	}
}

// A trailing newline is what an operator gets from `echo id > ~/.nine/…`, and it
// must not become part of the id — the daemon would report no such conversation.
func TestLoadConversationIDTrimsWhitespace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := filepath.Join(home, ".nine")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "last-conversation"), []byte("  agent-7\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadConversationID(); got != "agent-7" {
		t.Errorf("loadConversationID = %q, want it trimmed to agent-7", got)
	}
}

// The state file holds a conversation id, which is not secret but is not other
// users' business either; the directory it lives in is the operator's.
func TestConversationStateIsNotWorldReadable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	saveConversationID("agent-1")

	fi, err := os.Stat(filepath.Join(home, ".nine", "last-conversation"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("file mode = %o, want no group/other access", perm)
	}
}
