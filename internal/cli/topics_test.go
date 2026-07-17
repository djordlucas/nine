package cli

import (
	"bytes"
	"strings"
	"testing"

	"nine/internal/config"
)

// TestDocsList verifies `nine docs` (no topic) lists topics with their titles.
func TestDocsList(t *testing.T) {
	var out bytes.Buffer
	c := &CLI{Out: &out, Err: &bytes.Buffer{}}
	if err := c.Docs(""); err != nil {
		t.Fatalf("Docs: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "nine docs <topic>") {
		t.Errorf("listing missing usage header:\n%s", got)
	}
	// A known topic and its extracted title should both appear.
	if !strings.Contains(got, "workflows") || !strings.Contains(got, "Workflows") {
		t.Errorf("listing missing workflows topic/title:\n%s", got)
	}
}

// TestDocsTopic renders a single documentation topic verbatim (raw Markdown to a
// non-terminal buffer).
func TestDocsTopic(t *testing.T) {
	var out bytes.Buffer
	c := &CLI{Out: &out, Err: &bytes.Buffer{}}
	if err := c.Docs("hitl"); err != nil {
		t.Fatalf("Docs(hitl): %v", err)
	}
	if !strings.Contains(out.String(), "# Human-in-the-Loop") {
		t.Errorf("hitl doc content missing:\n%s", out.String())
	}
}

// TestSpecTopic covers the nested spec/contracts tree via its short name and its
// explicit relative path.
func TestSpecTopic(t *testing.T) {
	for _, name := range []string{"event-journal", "contracts/event-journal", "contracts/event-journal.md"} {
		var out bytes.Buffer
		c := &CLI{Out: &out, Err: &bytes.Buffer{}}
		if err := c.Spec(name); err != nil {
			t.Fatalf("Spec(%q): %v", name, err)
		}
		if !strings.Contains(out.String(), "Session Event Journal") {
			t.Errorf("Spec(%q) missing expected content:\n%s", name, out.String())
		}
	}
}

// TestHiddenTopic verifies ROADMAP is excluded from both the listing and
// resolution (by short name and explicit path).
func TestHiddenTopic(t *testing.T) {
	var list bytes.Buffer
	c := &CLI{Out: &list, Err: &bytes.Buffer{}}
	if err := c.Docs(""); err != nil {
		t.Fatalf("Docs: %v", err)
	}
	if strings.Contains(list.String(), "ROADMAP") {
		t.Errorf("ROADMAP should not appear in the docs listing:\n%s", list.String())
	}
	for _, name := range []string{"ROADMAP", "ROADMAP.md"} {
		err := (&CLI{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}).Docs(name)
		if err == nil || !strings.Contains(err.Error(), "unknown docs topic") {
			t.Errorf("Docs(%q) should be unknown, got: %v", name, err)
		}
	}
}

// TestUnknownTopic is an error that points back at the listing.
func TestUnknownTopic(t *testing.T) {
	c := &CLI{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}
	err := c.Docs("does-not-exist")
	if err == nil || !strings.Contains(err.Error(), "unknown docs topic") {
		t.Fatalf("expected unknown-topic error, got: %v", err)
	}
}

// TestRunDocsSpecDispatch checks the dispatcher wires `docs`/`spec` with and
// without a topic argument.
func TestRunDocsSpecDispatch(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"docs"}, "nine docs <topic>"},
		{[]string{"spec"}, "nine spec <topic>"},
		{[]string{"spec", "wire-protocol"}, "Wire Protocol"},
	}
	for _, tc := range cases {
		var out bytes.Buffer
		c := &CLI{Out: &out, Err: &bytes.Buffer{}}
		if err := c.Run(tc.args, &config.Config{}); err != nil {
			t.Fatalf("Run(%v): %v", tc.args, err)
		}
		if !strings.Contains(out.String(), tc.want) {
			t.Errorf("Run(%v) missing %q:\n%s", tc.args, tc.want, out.String())
		}
	}
}
