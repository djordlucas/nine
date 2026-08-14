package builtins

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"nine/internal/plugin"
)

// workspaceRoot confines writes when NINE_WORKSPACE is set. It is read in
// serveFiles rather than an init(), because this package is linked into the
// nine binary as a whole: an init() would also run in the daemon and CLI
// processes, which have no business inheriting a plugin's env-derived state.
// Only the `nine plugin serve files` child ever sets it.
var workspaceRoot string

// serveFiles runs the `files` built-in: read_file / write_file, confined to
// NINE_WORKSPACE when one is configured.
func serveFiles() {
	workspaceRoot = os.Getenv("NINE_WORKSPACE")

	writeDesc := "Write content to a file, creating parent directories as needed."
	if workspaceRoot != "" {
		writeDesc = fmt.Sprintf("Write content to a file within the workspace root (%s). Relative paths and paths under /work resolve against the workspace root; other absolute paths outside it are rejected.", workspaceRoot)
	}

	plugin.Serve(
		[]plugin.ToolDefinition{
			{
				Name:        "read_file",
				DisplayName: "Read File",
				Description: "Read the contents of a file from the filesystem.",
				InputSchema: plugin.Schema(`{"type":"object","required":["path"],"properties":{"path":{"type":"string","description":"Absolute path to the file"}}}`),
			},
			{
				Name:        "write_file",
				DisplayName: "Write File",
				Description: writeDesc,
				InputSchema: plugin.Schema(`{"type":"object","required":["path","content"],"properties":{"path":{"type":"string"},"content":{"type":"string"}}}`),
			},
		},
		map[string]plugin.ToolHandler{
			"read_file":  readFile,
			"write_file": writeFile,
		},
	)
}

func readFile(_ context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", plugin.InvalidArgs("%v", err)
	}
	b, err := os.ReadFile(resolveReadPath(p.Path))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func writeFile(_ context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", plugin.InvalidArgs("%v", err)
	}
	path, err := resolveWritePath(p.Path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(p.Content), 0o644); err != nil {
		return "", err
	}
	return "ok", nil
}

// workspaceAlias is the canonical path prefix under which the workspace root is
// presented to the agent. A leading "/work" resolves to the configured root, so
// the model can address workspace files by a stable absolute path regardless of
// where the run's actual root lives (e.g. the eval schema's /work/… convention,
// docs/evals.md §2). It never collides with the production root (workspace.root,
// e.g. ./workspace) and is a no-op when no workspace root is configured.
const workspaceAlias = "/work"

// rootRelative maps a caller-supplied path into the workspace root: it rewrites a
// leading /work alias to the root and joins relative paths onto it. Genuine
// absolute paths (outside the alias) are returned untouched. It returns the empty
// string when no workspace root is configured, signalling "use the path as-is".
func rootRelative(rootAbs, path string) string {
	if path == workspaceAlias || strings.HasPrefix(path, workspaceAlias+"/") {
		return filepath.Join(rootAbs, filepath.Clean("/"+strings.TrimPrefix(path, workspaceAlias)))
	}
	if !filepath.IsAbs(path) {
		return filepath.Join(rootAbs, path)
	}
	return path
}

// resolveReadPath maps path into the workspace root when one is configured (via
// the /work alias or a relative path), leaving other absolute paths as given so
// reads outside the workspace still work.
func resolveReadPath(path string) string {
	if workspaceRoot == "" {
		return path
	}
	rootAbs, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return path
	}
	return rootRelative(rootAbs, path)
}

// resolveWritePath resolves path against the workspace root (if configured) and
// rejects any write that escapes the root.
func resolveWritePath(path string) (string, error) {
	if workspaceRoot == "" {
		return path, nil
	}
	rootAbs, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(rootRelative(rootAbs, path))
	if err != nil {
		return "", err
	}
	if abs != rootAbs && !strings.HasPrefix(abs, rootAbs+string(filepath.Separator)) {
		return "", fmt.Errorf("write path %q is outside workspace root %q", path, rootAbs)
	}
	return abs, nil
}
