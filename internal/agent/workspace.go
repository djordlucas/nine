package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"nine/internal/llm"
	"nine/internal/memory"
)

// WorkspaceHit is one workspace search result, with the snippet read from the
// file rather than from an index — so it shows the file as it is now.
type WorkspaceHit struct {
	Path    string `json:"path"`
	Snippet string `json:"snippet"`
	Source  string `json:"source"` // "workspace" or "scan"
}

// WorkspaceBackend is the workspace index, as the dispatcher needs it. The
// implementation lives in the runtime, which owns the scanner and the root;
// this is the narrow view the tools use.
type WorkspaceBackend interface {
	// Search returns hits under pathPrefix, how many files under that prefix are
	// known but unindexed, and whether the first full scan has finished.
	Search(ctx context.Context, query, pathPrefix string, limit int) (hits []WorkspaceHit, skipped int, scanned bool, err error)
	// List returns a JSON listing for list_files.
	List(ctx context.Context, prefix, pattern, changedSince string, limit int) (string, error)
}

// SetWorkspace installs the workspace index and registers list_files.
//
// Registering the tool here rather than in RegisterMemoryTools keeps the two
// independent: a daemon with no workspace configured advertises no list_files,
// instead of advertising one that can only answer "nothing".
func (d *Dispatcher) SetWorkspace(ws WorkspaceBackend) {
	d.workspace = ws
	if ws == nil {
		// Advertised but unconfigured: the tool explains the setting rather than
		// failing as an unknown tool, which reads like a Nine bug.
		registerWorkspacePlaceholders(d)
		return
	}
	d.handlers["list_files"] = func(ctx context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Prefix       string `json:"prefix"`
			Pattern      string `json:"pattern"`
			ChangedSince string `json:"changed_since"`
			Limit        int    `json:"limit"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("list_files: %w", err)
		}
		return ws.List(ctx, req.Prefix, req.Pattern, req.ChangedSince, req.Limit)
	}
	d.backends["list_files"] = "builtin"
}

// workspaceToolDefs are advertised whenever the daemon runs, like read_file and
// write_file: a workspace that is not configured is an operator's setting, and
// the tool says so rather than being silently absent.
var workspaceToolDefs = []llm.ToolDef{
	{
		Name:        "list_files",
		DisplayName: "List Files",
		Description: "List files in the workspace. Filter by prefix (a directory), by pattern (a glob such as *_test.go, matched against the whole path), or by changed_since (an ISO-8601 timestamp) to see what appeared or changed — including files created outside Nine, by a git pull or dropped in by a person. Each entry reports its size and whether its text is searchable.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"prefix":{"type":"string","description":"Only paths under this directory."},"pattern":{"type":"string","description":"Glob over the whole path, e.g. *_test.go or src/*.md."},"changed_since":{"type":"string","description":"ISO-8601 timestamp; only files changed at or after it."},"limit":{"type":"integer","description":"Maximum entries (default 200)."}}}`),
	},
}

// registerWorkspacePlaceholders installs handlers that explain an unconfigured
// workspace, so an advertised tool never fails with "unknown tool".
func registerWorkspacePlaceholders(d *Dispatcher) {
	if _, ok := d.handlers["list_files"]; ok {
		return
	}
	d.handlers["list_files"] = func(context.Context, json.RawMessage) (string, error) {
		return "", fmt.Errorf("no workspace is configured; set [workspace].root")
	}
	d.backends["list_files"] = "builtin"
}

// workspaceSearchOutput renders one result set from two places: files the store
// holds (spilled tool output) and files on disk.
//
// Both are labelled. A model that cannot tell which namespace a hit came from
// cannot tell which tool will read it back, and the recovery messages in
// register_memory.go exist because it guessed wrong.
func workspaceSearchOutput(
	store *memory.Store,
	query, pathPrefix string,
	stored []memory.FileSearchResult,
	hits []WorkspaceHit,
	skipped int,
	scanned bool,
) (string, error) {
	type result struct {
		Path    string `json:"path"`
		Snippet string `json:"snippet"`
		Source  string `json:"source"`
	}
	out := make([]result, 0, len(stored)+len(hits))
	for _, h := range hits {
		src := h.Source
		if src == "" {
			src = "workspace"
		}
		out = append(out, result{Path: h.Path, Snippet: h.Snippet, Source: src})
	}
	for _, r := range stored {
		out = append(out, result{Path: r.Path, Snippet: r.Snippet, Source: "spill"})
	}

	payload := map[string]any{"results": out}

	// What the search could not look at is part of the answer. "No matches" is
	// read as "not there", and a live model answered an empty result by
	// inventing a value (adr/tool-output-spill.md).
	var notes []string
	if !scanned {
		notes = append(notes, "the first workspace scan has not finished, so some files are not searchable yet")
	}
	if skipped > 0 {
		notes = append(notes, fmt.Sprintf("%d file(s) here are not indexed (too large, or not text); name one in path to scan it directly", skipped))
	}
	if len(out) == 0 {
		notes = append(notes, noSearchHitsMessage(store, query, pathPrefix))
	}
	if len(notes) > 0 {
		payload["note"] = strings.Join(notes, "; ")
	}

	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
