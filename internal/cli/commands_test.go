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
		if !strings.Contains(out.String(), "# CLI usage") {
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

// parseTraceFlags consumes args off the front rather than walking them by
// index, so "--turn" eating the argument after it is the case most worth
// pinning: the flags must still work in any order, and "--turn" at the very end
// must be an error rather than an out-of-range read.
func TestParseTraceFlags(t *testing.T) {
	cases := []struct {
		name          string
		args          []string
		wantTurn      int
		wantSubAgents bool
		wantErr       string
	}{
		{name: "no flags", args: nil},
		{name: "sub-agents alone", args: []string{"--sub-agents"}, wantSubAgents: true},
		{name: "turn with separate value", args: []string{"--turn", "3"}, wantTurn: 3},
		{name: "turn with equals", args: []string{"--turn=3"}, wantTurn: 3},
		{
			name: "both, turn first", args: []string{"--turn", "2", "--sub-agents"},
			wantTurn: 2, wantSubAgents: true,
		},
		{
			name: "both, sub-agents first", args: []string{"--sub-agents", "--turn", "2"},
			wantTurn: 2, wantSubAgents: true,
		},
		{
			name: "both, equals form", args: []string{"--sub-agents", "--turn=7"},
			wantTurn: 7, wantSubAgents: true,
		},
		{name: "turn without a value", args: []string{"--turn"}, wantErr: "--turn requires a value"},
		{
			name: "turn without a value, after another flag",
			args: []string{"--sub-agents", "--turn"}, wantErr: "--turn requires a value",
		},
		{name: "turn value is not a number", args: []string{"--turn", "x"}, wantErr: `invalid --turn value "x"`},
		{name: "equals value is not a number", args: []string{"--turn=x"}, wantErr: `invalid --turn value "x"`},
		{name: "unknown flag", args: []string{"--nope"}, wantErr: "unexpected argument: --nope"},
		{
			name: "the value of --turn is not treated as a flag",
			args: []string{"--turn", "--sub-agents"}, wantErr: `invalid --turn value "--sub-agents"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			turn, subAgents, err := parseTraceFlags(tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parseTraceFlags(%q) error = %v, want it to contain %q", tc.args, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTraceFlags(%q) returned an unexpected error: %v", tc.args, err)
			}
			if turn != tc.wantTurn {
				t.Errorf("parseTraceFlags(%q) turn = %d, want %d", tc.args, turn, tc.wantTurn)
			}
			if subAgents != tc.wantSubAgents {
				t.Errorf("parseTraceFlags(%q) subAgents = %v, want %v", tc.args, subAgents, tc.wantSubAgents)
			}
		})
	}
}
