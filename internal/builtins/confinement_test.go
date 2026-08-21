package builtins_test

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The built-ins share the nine binary, so a plugin child could in principle run
// any of nine's startup. These tests pin that it runs none of it — the child is
// a plugin and nothing else (spec/contracts/plugin.md R-PLUG.13).
//
// They spawn the binary directly rather than through the Manager, because the
// point is what the process does before plugin.Serve is reached, and because the
// cwd and env a plugin inherits are exactly what is under test.

// linkNine puts a nine binary in its own directory, so a nine.log written beside
// it is attributable to this test alone. A hard link avoids copying ~40MB;
// os.Executable reports the link path, which is what setupLogger would use.
func linkNine(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	dst := filepath.Join(dir, "nine")
	if err := os.Link(nineBin, dst); err != nil {
		// Different filesystem: fall back to a copy.
		src, err := os.Open(nineBin)
		if err != nil {
			t.Fatalf("open nine: %v", err)
		}
		defer src.Close() //nolint:errcheck // test cleanup
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0o755)
		if err != nil {
			t.Fatalf("create nine copy: %v", err)
		}
		if _, err := io.Copy(out, src); err != nil {
			out.Close() //nolint:errcheck // test cleanup
			t.Fatalf("copy nine: %v", err)
		}
		if err := out.Close(); err != nil {
			t.Fatalf("close nine copy: %v", err)
		}
	}
	return dst
}

// serveChild starts `nine plugin serve <name>` with a deliberately hostile
// environment — a cwd and $HOME that both contain a config file — and returns
// the binary path, the combined output, and the exit error (nil if it was still
// running and had to be killed).
func serveChild(t *testing.T, name string, budget time.Duration, env ...string) (bin, output string, waitErr error) {
	t.Helper()
	bin = linkNine(t)
	workdir := t.TempDir()

	// An unparseable config: if the child reads it, LoadDefault logs
	// "ignoring unusable config file", which is a positive signal that the read
	// happened. A valid file would leave no trace either way.
	if err := os.WriteFile(filepath.Join(workdir, "nine.toml"), []byte("not valid toml [[[\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cmd := exec.Command(bin, "plugin", "serve", name)
	cmd.Dir = workdir
	// HOME is on the plugin env allowlist, so it is a real config search path.
	cmd.Env = append([]string{"PATH=/usr/bin:/bin", "HOME=" + workdir}, env...)
	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case waitErr = <-done:
	case <-time.After(budget):
		// Still serving. For a valid invocation that is the healthy case; for one
		// that should have refused, it is the failure the caller is testing for,
		// and waitErr stays nil to say so.
		cmd.Process.Kill() //nolint:errcheck // best-effort
		<-done
	}
	return bin, buf.String(), waitErr
}

// waitBudget is how long serveChild gives the child before calling it "still
// serving". The two cases want opposite budgets, which is why the caller picks.
//
// A child that should *refuse* exits in milliseconds, so waiting longer costs
// nothing on the happy path and only buys tolerance for a slow start — and slow
// starts happen: the binary is freshly hardlinked, so its first execution pays
// the code-signing assessment macOS charges for a binary it has not seen, on top
// of spawn contention when the suite runs in parallel. The old fixed 2s was the
// flake in TestServeChildRequiresSocket: a child exiting at 2.001s was reported
// as "kept running", which is the same mistake R-PLUG.14 fixed in the daemon —
// a fixed wall-clock budget that cannot tell dead from slow.
//
// A child that should *keep serving* is confirmed alive by the budget elapsing,
// so there the budget is pure cost and stays short.
type waitBudget = time.Duration

const (
	// expectExit: the child should refuse and exit. Generous.
	expectExit = 30 * time.Second
	// expectServing: the child should still be running. Paid in full every time.
	expectServing = 2 * time.Second
)

// mustHaveRun fails when the child never got as far as doing the thing under
// test. Both config tests assert an *absence*, so a child that exited instantly —
// because the name stopped being a built-in, say — would satisfy them without
// testing anything. Removing `time` from the roster did exactly that, silently.
func mustHaveRun(t *testing.T, name, out string) {
	t.Helper()
	if strings.Contains(out, "unknown built-in plugin") {
		t.Fatalf("child never served: %q is not a built-in any more, so this test proves nothing:\n%s", name, out)
	}
}

// TestServeChildReadsNoOperatorConfig is the one that matters. The operator's
// nine.toml carries [embeddings].api_key and every [plugin.<name>.settings]
// block — including other plugins' settings. internal/plugin.sanitizedHostEnv
// withholds exactly that class of data from a plugin's environment, and a plugin
// child that loaded the config itself would walk straight around it.
func TestServeChildReadsNoOperatorConfig(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "p.sock")
	// NINE_LOG_FILE=off forces slog to stderr. Without it a config-load warning
	// would land in nine.log instead, where this test cannot see it — and the
	// check would pass even for a child that did read the config.
	bin, out, _ := serveChild(t, "shell", expectServing, "NINE_PLUGIN_SOCKET="+sock, "NINE_LOG_FILE=off")
	mustHaveRun(t, "shell", out)

	if strings.Contains(out, "ignoring unusable config file") {
		t.Errorf("plugin child parsed a config file it should never look for; output:\n%s", out)
	}
	// Belt and braces: NINE_LOG_FILE should be moot because no log file is
	// opened at all, but read it if one appeared rather than trust the flag.
	if b, err := os.ReadFile(filepath.Join(filepath.Dir(bin), "nine.log")); err == nil {
		if strings.Contains(string(b), "ignoring unusable config file") {
			t.Errorf("plugin child parsed a config file (found in nine.log):\n%s", b)
		}
	}
}

// TestServeChildWritesNoLogFile pins that a plugin child does not open the
// daemon's log. Five processes sharing the nine binary would otherwise
// interleave in one nine.log; a plugin's output belongs on stderr, which the
// manager wires to the daemon's.
func TestServeChildWritesNoLogFile(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "p.sock")
	bin, out, _ := serveChild(t, "shell", expectServing, "NINE_PLUGIN_SOCKET="+sock)
	mustHaveRun(t, "shell", out)

	logPath := filepath.Join(filepath.Dir(bin), "nine.log")
	if _, err := os.Stat(logPath); err == nil {
		b, _ := os.ReadFile(logPath)
		t.Errorf("plugin child wrote %s:\n%s", logPath, b)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", logPath, err)
	}
}

// TestServeChildRequiresSocket checks the fail-closed guard: run by hand rather
// than spawned by the manager, a built-in must refuse rather than sit there.
func TestServeChildRequiresSocket(t *testing.T) {
	// The socket guard runs before the name is resolved, so any name reaches it —
	// including one that is no longer a built-in, which is the point being made.
	_, out, err := serveChild(t, "shell", expectExit) // no NINE_PLUGIN_SOCKET

	if err == nil {
		t.Error("plugin serve with no NINE_PLUGIN_SOCKET kept running; want a non-zero exit")
	}
	if !strings.Contains(out, "NINE_PLUGIN_SOCKET") {
		t.Errorf("error does not name the missing variable; output:\n%s", out)
	}
}

// TestServeChildRejectsUnknownName covers the typo path, which must not be
// reported as the CLI's `plugin validate` usage — the two commands share a verb.
func TestServeChildRejectsUnknownName(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "p.sock")
	_, out, err := serveChild(t, "nope", expectExit, "NINE_PLUGIN_SOCKET="+sock)

	if err == nil {
		t.Error("plugin serve with an unknown name kept running; want a non-zero exit")
	}
	if !strings.Contains(out, "nope") || strings.Contains(out, "plugin validate") {
		t.Errorf("unhelpful error for an unknown built-in; output:\n%s", out)
	}
}
