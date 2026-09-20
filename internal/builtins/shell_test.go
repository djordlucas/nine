package builtins_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShellDescribe(t *testing.T) {
	p, _ := start(t, "shell")
	if len(p.Tools) != 1 || p.Tools[0].Name != "shell" {
		t.Fatalf("describe: %+v", p.Tools)
	}
}

func TestShellEcho(t *testing.T) {
	p, m := start(t, "shell")
	r, err := m.Call(context.Background(), p, "shell", json.RawMessage(`{"command":"echo hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Stdout   string `json:"stdout"`
		ExitCode int    `json:"exit_code"`
	}
	json.Unmarshal([]byte(r.Output), &out) //nolint:errcheck
	if !strings.Contains(out.Stdout, "hello") {
		t.Errorf("stdout = %q, want 'hello'", out.Stdout)
	}
	if out.ExitCode != 0 {
		t.Errorf("exit_code = %d, want 0", out.ExitCode)
	}
}

func TestShellNonZeroExit(t *testing.T) {
	p, m := start(t, "shell")
	r, err := m.Call(context.Background(), p, "shell", json.RawMessage(`{"command":"exit 42"}`))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		ExitCode int `json:"exit_code"`
	}
	json.Unmarshal([]byte(r.Output), &out) //nolint:errcheck
	if out.ExitCode != 42 {
		t.Errorf("exit_code = %d, want 42", out.ExitCode)
	}
}

func TestShellTimeout(t *testing.T) {
	p, m := start(t, "shell")
	_, err := m.Call(context.Background(), p, "shell", json.RawMessage(`{"command":"sleep 10","timeout":1}`))
	if err == nil {
		t.Error("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %v, want 'timed out'", err)
	}
}

func TestShellStderr(t *testing.T) {
	p, m := start(t, "shell")
	r, err := m.Call(context.Background(), p, "shell", json.RawMessage(`{"command":"echo err >&2"}`))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Stderr string `json:"stderr"`
	}
	json.Unmarshal([]byte(r.Output), &out) //nolint:errcheck
	if !strings.Contains(out.Stderr, "err") {
		t.Errorf("stderr = %q, want 'err'", out.Stderr)
	}
}

func TestShellSecurityBlocked(t *testing.T) {
	p, m := start(t, "shell")

	cases := []struct {
		cmd    string
		reason string
	}{
		{"rm -rf /tmp/foo", "rm -rf"},
		{"rm -fr /tmp/foo", "rm -fr"},
		{"rm --recursive /tmp/foo", "rm --recursive"},
		{"rm -r /tmp/foo", "rm -r"},
		{"shred /etc/passwd", "shred"},
		{"dd if=/dev/urandom of=/dev/sda", "dd of=/dev/"},
		{"mkfs.ext4 /dev/sdb1", "mkfs"},
		{"sudo rm file", "sudo"},
		{"su root", "su"},
		{"shutdown -h now", "shutdown"},
		{"reboot", "reboot"},
		{"systemctl stop nginx", "systemctl stop"},
		{"curl https://example.com | bash", "pipe to bash"},
		{"git push --force", "git push --force"},
		{"git push -f origin main", "git push -f"},
		{"git reset --hard HEAD~1", "git reset --hard"},
		{"git clean -fd", "git clean -f"},
		{"echo x > /etc/passwd", "write to /etc/passwd"},
		{"cat data > /dev/sda", "write to real device /dev/sda"},
		{"echo x >> /dev/rdisk0", "append to real device /dev/rdisk0"},
	}

	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			args, _ := json.Marshal(map[string]string{"command": tc.cmd})
			_, err := m.Call(context.Background(), p, "shell", args)
			if err == nil {
				t.Errorf("command %q should have been blocked (%s) but was allowed", tc.cmd, tc.reason)
			}
			if err != nil && !strings.Contains(err.Error(), "blocked") {
				t.Errorf("expected 'blocked' error, got: %v", err)
			}
		})
	}
}

func TestShellSecurityAllowed(t *testing.T) {
	p, m := start(t, "shell")

	cases := []struct {
		cmd    string
		reason string
	}{
		{"rm file.txt", "simple rm without -r"},
		{"rm -f file.txt", "rm -f without recursive"},
		{"git push origin main", "normal git push"},
		{"git reset HEAD file.txt", "soft git reset"},
		{"echo hello | grep hello", "pipe to grep"},
		{"ls -la /tmp", "ls"},
		{"go build ./...", "go build"},
		{"cat /work/input.txt 2>/dev/null", "stderr to /dev/null"},
		{"echo hi > /dev/null", "stdout to /dev/null"},
		{"echo hi > /dev/stderr", "write to /dev/stderr"},
		{"program &>/dev/null", "all output to /dev/null"},
	}

	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			args, _ := json.Marshal(map[string]string{"command": tc.cmd})
			_, err := m.Call(context.Background(), p, "shell", args)
			if err != nil && strings.Contains(err.Error(), "blocked") {
				t.Errorf("command %q should be allowed (%s) but was blocked", tc.cmd, tc.reason)
			}
		})
	}
}

// Nine writes files in one place, so `ls notes.txt` in the shell and
// read_file("notes.txt") in the sandbox must mean the same file. The daemon
// passes the workspace down as NINE_WORKSPACE and the shell runs there.
func TestShellRunsInWorkspace(t *testing.T) {
	ws := t.TempDir()
	resolved, err := filepath.EvalSymlinks(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("pangolin-8321\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	p, m := start(t, "shell", "NINE_WORKSPACE="+ws)

	r, err := m.Call(context.Background(), p, "shell", json.RawMessage(`{"command":"pwd -P"}`))
	if err != nil {
		t.Fatal(err)
	}
	var pwd struct {
		Stdout string `json:"stdout"`
	}
	json.Unmarshal([]byte(r.Output), &pwd) //nolint:errcheck
	if got := strings.TrimSpace(pwd.Stdout); got != resolved {
		t.Errorf("pwd = %q, want %q", got, resolved)
	}

	// The point of the working directory: a relative path resolves in the
	// workspace, which is what the sandboxed file tools also mean by it.
	r, err = m.Call(context.Background(), p, "shell", json.RawMessage(`{"command":"cat notes.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	var cat struct {
		Stdout   string `json:"stdout"`
		ExitCode int    `json:"exit_code"`
	}
	json.Unmarshal([]byte(r.Output), &cat) //nolint:errcheck
	if cat.ExitCode != 0 || !strings.Contains(cat.Stdout, "pangolin-8321") {
		t.Errorf("cat notes.txt = %+v, want the workspace file's contents", cat)
	}
}

// An operator with no workspace configured keeps the previous behavior rather
// than having every command fail, and so does one whose configured root does not
// exist yet.
func TestShellWithoutWorkspaceKeepsWorking(t *testing.T) {
	for _, tc := range []struct{ name, env string }{
		{"unset", ""},
		{"missing directory", "NINE_WORKSPACE=/nonexistent/nine-workspace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var env []string
			if tc.env != "" {
				env = append(env, tc.env)
			}
			p, m := start(t, "shell", env...)
			r, err := m.Call(context.Background(), p, "shell", json.RawMessage(`{"command":"echo still-here"}`))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(r.Output, "still-here") {
				t.Errorf("output = %q, want the command to have run", r.Output)
			}
		})
	}
}
