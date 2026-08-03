package protocol_test

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nine/internal/protocol"
)

// The daemon EnsureDaemon spawns must not write to the caller's terminal. The
// caller is usually the TUI, which is about to draw a full-screen UI there, so
// an inherited stderr means the daemon's boot log lands in the chat area — the
// symptom when file logging is off and slog goes to stderr.
//
// The fake daemon writes to both streams and only then becomes reachable, so
// the spawn path runs for real; sockets go in a short /tmp path because macOS
// caps Unix-socket paths at 104 bytes.
func TestEnsureDaemonKeepsSpawnedOutputOffTheTerminal(t *testing.T) {
	dir, err := os.MkdirTemp("", "nine-ensure")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	const marker = "DAEMON-BOOT-LOG"
	fake := filepath.Join(dir, "fake-nine")
	script := "#!/bin/sh\necho " + marker + "-stdout\necho " + marker + "-stderr >&2\nsleep 30\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	// Stand in for the terminal: whatever the child inherits ends up here.
	captured := filepath.Join(dir, "terminal.txt")
	f, err := os.Create(captured)
	if err != nil {
		t.Fatal(err)
	}
	realStderr, realStdout := os.Stderr, os.Stdout
	os.Stderr, os.Stdout = f, f
	t.Cleanup(func() {
		os.Stderr, os.Stdout = realStderr, realStdout
		f.Close()
	})

	// The fake never listens, so stand the socket up out of band once the
	// child has had time to write — otherwise EnsureDaemon polls for a full 5s.
	sock := filepath.Join(dir, "s.sock")
	go func() {
		time.Sleep(300 * time.Millisecond)
		if ln, err := net.Listen("unix", sock); err == nil {
			t.Cleanup(func() { ln.Close() })
		}
	}()

	proc, err := protocol.EnsureDaemon(sock, fake)
	if err != nil {
		t.Fatalf("EnsureDaemon: %v", err)
	}
	if proc == nil {
		t.Fatal("EnsureDaemon returned no process, want the one it started")
	}
	t.Cleanup(func() { proc.Kill() }) //nolint:errcheck

	os.Stderr, os.Stdout = realStderr, realStdout
	out, err := os.ReadFile(captured)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), marker) {
		t.Errorf("spawned daemon wrote to the caller's terminal: %q", string(out))
	}
}
