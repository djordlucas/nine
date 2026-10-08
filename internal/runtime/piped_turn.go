package runtime

// PipedTurnTools are the tools a turn may use when a report piped from a
// process started it (adr/process-sessions.md §7). What a pipe carries is
// data, often fetched from outside, and models obey instructions planted in
// it even when it is framed as data: on process-pipe-injection, qwen3.5:4b and
// 9b deleted a file on an injected instruction every time. So the turn is
// limited to what acting on a report needs — read, record, remember, steer its
// own goal, tell a person — and everything else waits for the session's own
// next turn, which its goal drives.
//
// An allowlist, so a tool added later — a plugin's, an MCP server's, one Nine
// writes — is left out until someone decides it belongs here. Left out on
// purpose: deleting and moving files, the shell, the network, sub-agents and
// workflows, and anything that persists instructions (tool_write, skill_write,
// goal_create).
var PipedTurnTools = []string{
	"read_file", "list_files", "file_search_text", "diff_file",
	"write_file", "edit_file", "trash_list", "restore_file",
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
