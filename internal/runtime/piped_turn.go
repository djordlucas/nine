package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"nine/internal/agent"
)

// PipedTurnTools are the tools a turn may use when a report piped from a
// process started it (adr/process-sessions.md §7). What a pipe carries is
// data, often fetched from outside, and models obey instructions planted in
// it even when it is framed as data: on process-pipe-injection, qwen3.5:4b and
// 9b deleted a file on an injected instruction every time. So the turn is
// limited to what acting on a report needs — read, record, remember, steer its
// own goal, tell a person — and everything else waits for the session's own
// next turn, which its goal drives.
//
// Writes are new files only (pipedWriteGuard): an injected instruction can make
// the turn record, not rewrite what is there.
//
// An allowlist, so a tool added later — a plugin's, an MCP server's, one Nine
// writes — is left out until someone decides it belongs here. Left out on
// purpose: deleting and moving files, the shell, the network, sub-agents and
// workflows, and anything that persists instructions (tool_write, skill_write,
// goal_create), and restore_file, which can put an old version over current
// content.
var PipedTurnTools = []string{
	"read_file", "list_files", "file_search_text", "diff_file",
	"write_file", "edit_file", "trash_list",
	"memory_get", "memory_set", "memory_list", "memory_query",
	"skill_list", "skill_read", "skill_search",
	"doc_read", "doc_search", "tool_list", "tool_search",
	"goal_get", "goal_list", "goal_update_status",
	"notify_user", "time",
	"job_check", "job_list",
	"queued_messages_get", "queued_message_mark_consumed", "queued_messages_mark_all_consumed",
	"queued_messages_count", "queued_messages_unconsumed_count",
	"gap_report",
}

// pipedTurn is the restriction of a turn a piped report started: tools, and
// writes to new files only.
func pipedTurn(tools []string, workspaceRoot string) agent.TurnRestriction {
	return agent.TurnRestriction{Tools: tools, Guard: pipedWriteGuard(workspaceRoot)}
}

// pipedWriteGuard lets a piped turn's write_file create a file, and rewrite or
// append to one it created itself, but not touch a file that existed before
// the turn; edit_file may change only a file the turn created. 4b, refused
// delete_file on an injected instruction, emptied the file with edit_file
// instead. A path the guard cannot place in the workspace passes to the tool,
// which refuses it.
func pipedWriteGuard(workspaceRoot string) func(name string, args json.RawMessage) error {
	created := map[string]bool{}
	return func(name string, args json.RawMessage) error {
		if name != "write_file" && name != "edit_file" {
			return nil
		}
		var a struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(args, &a) != nil {
			return nil
		}
		rel, ok := workspaceRel(a.Path)
		if !ok || created[rel] {
			return nil
		}
		if workspaceRoot == "" {
			return fmt.Errorf("%s %s refused: in a turn a piped report started, only new files may be written, "+
				"and the workspace is not known here to tell", name, a.Path)
		}
		if _, err := os.Lstat(filepath.Join(workspaceRoot, filepath.FromSlash(rel))); err == nil {
			return fmt.Errorf("%s %s refused: it already exists, and a turn a piped report started may only "+
				"create new files. Record what you found in a new file; change this one on your own next turn "+
				"if your goal calls for it", name, a.Path)
		}
		if name == "write_file" {
			created[rel] = true
		}
		return nil
	}
}

// workspaceRel turns a file tool's path — relative, or under /work — into a
// clean path relative to the workspace, or false when it is outside it.
func workspaceRel(p string) (string, bool) {
	p = strings.TrimSpace(p)
	if p == "/work" || strings.HasPrefix(p, "/work/") {
		p = strings.TrimPrefix(strings.TrimPrefix(p, "/work"), "/")
	} else if strings.HasPrefix(p, "/") {
		return "", false
	}
	p = path.Clean(p)
	if p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return "", false
	}
	return p, true
}
