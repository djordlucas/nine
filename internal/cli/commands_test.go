package cli

import (
	"bytes"
	"strings"
	"testing"

	"nine/internal/config"
)

// TestRunTypoSuggestsCommand verifies a single mistyped word close to a real
// command (e.g. `staus`) is reported as an error with a suggestion and the usage
// reference — not silently sent as a message (which would need a daemon and hang
// the test).
func TestRunTypoSuggestsCommand(t *testing.T) {
	cases := []struct {
		arg  string
		want string // expected suggestion in the stderr note
	}{
		{"staus", "status"},
		{"stauts", "status"},
		{"contex", "context"},
		{"workflow", ""}, // exact command, handled elsewhere — not reached here
	}
	for _, tc := range cases {
		if tc.want == "" {
			continue
		}
		var out, errBuf bytes.Buffer
		c := &CLI{Out: &out, Err: &errBuf}
		if err := c.Run([]string{tc.arg}, &config.Config{}); err != nil {
			t.Fatalf("Run(%q): %v", tc.arg, err)
		}
		note := errBuf.String()
		if !strings.Contains(note, "unknown command: "+tc.arg) {
			t.Errorf("Run(%q): stderr = %q, want an unknown-command note", tc.arg, note)
		}
		if !strings.Contains(note, "did you mean") || !strings.Contains(note, tc.want) {
			t.Errorf("Run(%q): stderr = %q, want a suggestion of %q", tc.arg, note, tc.want)
		}
		if !strings.Contains(out.String(), "# CLI Usage") {
			t.Errorf("Run(%q): expected usage reference on stdout", tc.arg)
		}
	}
}

// TestNearestCommand checks the typo/message boundary: real typos resolve to a
// command, while genuine one-word messages that don't resemble any command do
// not (so `nine <message>` still works).
func TestNearestCommand(t *testing.T) {
	typos := map[string]string{
		"staus":  "status",  // one insertion
		"stauts": "status",  // transposition (distance 2)
		"halp":   "help",    // one substitution
		"contex": "context", // one deletion
		"hi":     "",        // too short
		"banana": "",        // a real message, not a command
		"deploy": "",        // 2 edits from "replay" but different first letter
		"reset":  "",        // unrelated word, not close to any command
	}
	for arg, want := range typos {
		got, ok := nearestCommand(arg)
		if want == "" {
			if ok {
				t.Errorf("nearestCommand(%q) = (%q, true), want no suggestion", arg, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("nearestCommand(%q) = (%q, %v), want (%q, true)", arg, got, ok, want)
		}
	}
}

// TestRunStopUsage verifies `nine stop` with no id returns a usage error rather
// than acting on anything.
func TestRunStopUsage(t *testing.T) {
	c := &CLI{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}
	err := c.Run([]string{"stop"}, &config.Config{})
	if err == nil || !strings.Contains(err.Error(), "usage: nine stop") {
		t.Errorf("Run(stop) error = %v, want a usage error", err)
	}
}
