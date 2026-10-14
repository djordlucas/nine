package runtime

import (
	"strings"

	ninectx "nine/internal/context"
)

// defaultResidentTools are the tools a deferred loop always advertises with
// their schemas (adr/deferred-tool-schemas.md §3.1, "Phase 1 as built"): the
// ten built-in tools called in more than 5% of the eval corpus's runs, plus
// the structural ones — tool_search is the load path, skill_read follows the
// self-model's skill list, ask_human and run_agent are the interactive and
// delegating roles' own. A tool the loop does not have is simply absent;
// [context] resident_tools adds to the list.
var defaultResidentTools = []string{
	"process_list", "read_file", "write_file", "memory_get", "memory_set",
	"list_files", "file_search_text", "goal_list", "process_start", "delete_file",
	"tool_search", "skill_read", "ask_human", "run_agent",
}

// toolkits groups Nine's own tools for the deferred index and for a toolkit
// load through tool_search. Their names describe themselves, so the index
// lists them by name alone.
var toolkits = map[string][]string{
	"workspace": {"read_file", "write_file", "edit_file", "list_files", "file_search_text",
		"diff_file", "move_file", "copy_file", "delete_file", "trash_list", "restore_file"},
	"memory":     {"memory_get", "memory_set", "memory_list", "memory_delete"},
	"skills":     {"skill_list", "skill_read", "skill_search", "skill_write", "skill_modify"},
	"delegation": {"run_agent", "run_agents", "workflow_create", "workflow_get", "workflow_update", "workflow_list", "workflow_retry_step"},
	"goals":      {"goal_create", "goal_get", "goal_list", "goal_update_status"},
	"processes":  {"process_list", "process_show", "process_start", "process_stop", "process_send"},
	"jobs":       {"job_check", "job_wait", "job_cancel", "job_list"},
	"web":        {"http_get", "http_post", "web_page_read", "web_search"},
	"messages": {"ask_human", "notify_user", "queued_messages_get", "queued_message_mark_consumed",
		"queued_messages_mark_all_consumed", "queued_messages_count", "queued_messages_unconsumed_count"},
	"authoring": {"tool_read", "tool_write", "tool_delete", "js_eval"},
	"catalog":   {"tool_list", "tool_search", "doc_read", "doc_search", "capability_request"},
	"data":      {"sql_query"},
	"time":      {"time"},
}

// toolkitOf maps a built-in tool name to its toolkit.
var toolkitOf = func() map[string]string {
	m := map[string]string{}
	for kit, names := range toolkits {
		for _, n := range names {
			m[n] = kit
		}
	}
	return m
}()

// builtinGlosses annotate the built-in tools whose names mislead on their own.
var builtinGlosses = map[string]string{"delete_file": "to trash"}

// annotateBuiltin sets the toolkit and gloss of a tool Nine names.
func annotateBuiltin(tw *ninectx.ToolWithVector) {
	tw.Toolkit = toolkitOf[tw.Tool.Name]
	tw.Gloss = builtinGlosses[tw.Tool.Name]
}

// annotateExternal sets the toolkit of a tool Nine did not name — a plugin's,
// an MCP server's, an operator's or a generated one — and glosses it with the
// first clause of its description, since its name may say nothing.
func annotateExternal(tw *ninectx.ToolWithVector, kit string) {
	tw.Toolkit = kit
	tw.Gloss = firstClause(tw.Tool.Description, 10)
}

// pluginToolkit is the toolkit of a plugin's tools: "mcp/<server>" for an MCP
// server, the plugin's name otherwise.
func pluginToolkit(pluginName string) string {
	if server, ok := strings.CutPrefix(pluginName, "mcp:"); ok {
		return "mcp/" + server
	}
	return pluginName
}

// firstClause returns s up to its first sentence or clause break, cut to at
// most maxWords words, so it reads as a gloss.
func firstClause(s string, maxWords int) string {
	if i := strings.IndexAny(s, ".;:\n—("); i >= 0 {
		s = s[:i]
	}
	words := strings.Fields(s)
	if len(words) > maxWords {
		words = words[:maxWords]
	}
	return strings.TrimRight(strings.Join(words, " "), ",")
}
