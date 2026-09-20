package builtins

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"

	"nine/internal/plugin"
)

// serveShell runs the `shell` built-in: a single tool that executes a command
// through sh, with the destructive-command guard in shell_security.go.
func serveShell() {
	plugin.Serve(
		[]plugin.ToolDefinition{{
			Name:        "shell",
			DisplayName: "Shell",
			Description: "Execute a shell command and return its stdout, stderr, and exit code as JSON. Commands run in the workspace, so a relative path means the same file here as it does to read_file and write_file. Destructive commands (recursive deletion, disk formatting, sudo, force-push, etc.) are blocked unless NINE_SHELL_UNSAFE=1 is set.",
			InputSchema: plugin.Schema(`{
				"type":"object",
				"required":["command"],
				"properties":{
					"command":{"type":"string","description":"Shell command to execute"},
					"timeout":{"type":"integer","description":"Timeout in seconds (default 30)"}
				}
			}`),
		}},
		map[string]plugin.ToolHandler{"shell": runShell},
	)
}

// workspaceDir returns the directory shell commands run in: the workspace the
// daemon passed down as NINE_WORKSPACE. Nine has one place it writes files, and
// a shell whose relative paths resolved somewhere else — the daemon's own
// working directory, which is wherever it was started — made `ls notes.txt`
// and `read_file("notes.txt")` disagree about which file that is.
//
// An empty or missing directory yields "", which leaves the process where it
// was: an operator running without a workspace configured keeps the old
// behavior rather than failing every command.
func workspaceDir() string {
	root := os.Getenv("NINE_WORKSPACE")
	if root == "" {
		return ""
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return ""
	}
	return root
}

func runShell(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", plugin.InvalidArgs("%v", err)
	}
	if p.Timeout <= 0 {
		p.Timeout = 30
	}

	if err := checkCommand(p.Command); err != nil {
		return "", err
	}

	// Derive from the caller's ctx so a cancelled/timed-out task (task_timeout)
	// aborts the command, in addition to the per-call timeout.
	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.Timeout)*time.Second)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "sh", "-c", p.Command)
	cmd.Dir = workspaceDir()
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	exitCode := 0
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("command timed out after %ds", p.Timeout)
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return "", err
		}
	}

	out := map[string]any{
		"stdout":    stdout.String(),
		"stderr":    stderr.String(),
		"exit_code": exitCode,
	}
	b, _ := json.Marshal(out)
	return string(b), nil
}
