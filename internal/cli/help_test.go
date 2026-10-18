package cli

import (
	"bytes"
	"strings"
	"testing"

	"nine/docs"
	"nine/internal/config"
)

// TestHelpRawMarkdown verifies that when Out is not a terminal (a bytes.Buffer),
// Help writes the embedded usage.md verbatim so piped output stays clean.
func TestHelpRawMarkdown(t *testing.T) {
	var out bytes.Buffer
	c := &CLI{Out: &out, Err: &bytes.Buffer{}}

	if err := c.Help(); err != nil {
		t.Fatalf("Help: %v", err)
	}
	if out.String() != docs.Usage {
		t.Errorf("Help output does not match embedded usage.md verbatim")
	}
	if !strings.Contains(out.String(), "nine help") {
		t.Errorf("usage reference is missing the `nine help` entry")
	}
}

// TestRunHelpAliases checks that help, --help and -h all render the usage
// reference rather than opening a conversation.
func TestRunHelpAliases(t *testing.T) {
	for _, arg := range []string{"help", "--help", "-h"} {
		var out bytes.Buffer
		c := &CLI{Out: &out, Err: &bytes.Buffer{}}
		if err := c.Run([]string{arg}, &config.Config{}); err != nil {
			t.Fatalf("Run(%q): %v", arg, err)
		}
		if !strings.Contains(out.String(), "# CLI usage") {
			t.Errorf("Run(%q) did not print usage reference:\n%s", arg, out.String())
		}
	}
}

// With no arguments, nine prints the usage, which points to the web UI; and
// attach, removed with the terminal UI, is refused rather than sent as a
// message.
func TestRunBareAndAttach(t *testing.T) {
	var out bytes.Buffer
	c := &CLI{Out: &out, Err: &bytes.Buffer{}}
	if err := c.Run(nil, &config.Config{}); err != nil {
		t.Fatalf("Run(): %v", err)
	}
	if !strings.Contains(out.String(), "# CLI usage") || !strings.Contains(out.String(), "web UI") {
		t.Errorf("Run() did not print the usage with the web UI pointer:\n%s", out.String())
	}
	err := c.Run([]string{"attach", "abc"}, &config.Config{})
	if err == nil || !strings.Contains(err.Error(), "web UI") {
		t.Errorf("Run(attach) = %v, want an error pointing to the web UI", err)
	}
}

// TestRunUnknownFlagShowsHelp verifies a flag-shaped first argument is treated
// as a mistyped command — help on stdout, an error note on stderr — instead of
// being sent as a message (which would require a daemon and hang the test).
func TestRunUnknownFlagShowsHelp(t *testing.T) {
	var out, errBuf bytes.Buffer
	c := &CLI{Out: &out, Err: &errBuf}

	if err := c.Run([]string{"--nope"}, &config.Config{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(errBuf.String(), "unknown command: --nope") {
		t.Errorf("expected unknown-command note on stderr, got: %q", errBuf.String())
	}
	if !strings.Contains(out.String(), "# CLI usage") {
		t.Errorf("expected usage reference on stdout, got: %q", out.String())
	}
}
