package toolvm

import (
	"encoding/json"
	"testing"
)

// The workspace has two names — the host directory `shell` prints and the /work
// mount a shipped tool sees — and a model that copies a path from one into the
// other must not get "cannot read" for a file that exists.
func TestRewriteWorkspacePath(t *testing.T) {
	const root = "/data/workspace"

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"host path becomes guest path", `{"path":"/data/workspace/notes.txt"}`, `{"path":"/work/notes.txt"}`},
		{"nested host path", `{"path":"/data/workspace/a/b/c.md"}`, `{"path":"/work/a/b/c.md"}`},
		{"the root itself", `{"path":"/data/workspace"}`, `{"path":"/work"}`},
		{"trailing slash", `{"path":"/data/workspace/"}`, `{"path":"/work"}`},
		{"guest path untouched", `{"path":"/work/notes.txt"}`, `{"path":"/work/notes.txt"}`},
		{"relative path untouched", `{"path":"notes.txt"}`, `{"path":"notes.txt"}`},
		// A directory whose name merely starts with the root's is a different
		// directory; rewriting it would point the tool at the wrong file.
		{"sibling sharing a prefix untouched", `{"path":"/data/workspace-backup/notes.txt"}`, `{"path":"/data/workspace-backup/notes.txt"}`},
		{"unrelated absolute path untouched", `{"path":"/etc/passwd"}`, `{"path":"/etc/passwd"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteWorkspacePath(json.RawMessage(tc.in), root)
			if gotPath, wantPath := pathOf(t, got), pathOf(t, json.RawMessage(tc.want)); gotPath != wantPath {
				t.Fatalf("path = %q, want %q", gotPath, wantPath)
			}
		})
	}
}

// Other arguments survive the rewrite: only `path` is a workspace path.
func TestRewriteWorkspacePath_PreservesOtherArgs(t *testing.T) {
	in := json.RawMessage(`{"path":"/data/workspace/a.txt","content":"/data/workspace/a.txt is literal text"}`)
	out := rewriteWorkspacePath(in, "/data/workspace")

	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := obj["path"]; got != "/work/a.txt" {
		t.Errorf("path = %v, want /work/a.txt", got)
	}
	if got := obj["content"]; got != "/data/workspace/a.txt is literal text" {
		t.Errorf("content was rewritten: %v", got)
	}
}

// With no workspace configured there is nothing to map onto, and malformed or
// path-less arguments must reach the tool unchanged rather than being dropped.
func TestRewriteWorkspacePath_PassThrough(t *testing.T) {
	cases := []struct {
		name string
		args string
		root string
	}{
		{"no workspace root", `{"path":"/data/workspace/a.txt"}`, ""},
		{"no path property", `{"query":"pangolin"}`, "/data/workspace"},
		{"not an object", `["/data/workspace/a.txt"]`, "/data/workspace"},
		{"empty path", `{"path":""}`, "/data/workspace"},
		{"path is not a string", `{"path":42}`, "/data/workspace"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(rewriteWorkspacePath(json.RawMessage(tc.args), tc.root)); got != tc.args {
				t.Fatalf("args = %s, want %s unchanged", got, tc.args)
			}
		})
	}
}

func pathOf(t *testing.T, args json.RawMessage) string {
	t.Helper()
	var obj struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &obj); err != nil {
		t.Fatalf("unmarshal %s: %v", args, err)
	}
	return obj.Path
}
