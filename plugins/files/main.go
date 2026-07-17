package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"nine/internal/plugin"
)

var workspaceRoot string

func init() {
	workspaceRoot = os.Getenv("NINE_WORKSPACE")
}

func main() {
	writeDesc := "Write content to a file, creating parent directories as needed."
	if workspaceRoot != "" {
		writeDesc = fmt.Sprintf("Write content to a file within the workspace root (%s). Relative paths resolve against the workspace root; absolute paths outside it are rejected.", workspaceRoot)
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
	b, err := os.ReadFile(p.Path)
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
	if !filepath.IsAbs(path) {
		path = filepath.Join(rootAbs, path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if abs != rootAbs && !strings.HasPrefix(abs, rootAbs+string(filepath.Separator)) {
		return "", fmt.Errorf("write path %q is outside workspace root %q", path, rootAbs)
	}
	return abs, nil
}
