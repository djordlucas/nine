package agent

import (
	"nine/internal/llm"
)

// InterceptedDefs are the tool definitions for core-intercepted tools.
// The context builder includes these in every turn, exempt from relevance
// filtering. Grouped by domain across register_memory.go, register_skills.go,
// register_subagents.go, register_workflow.go, and register_goals.go.
var InterceptedDefs = concatToolDefs(
	memoryToolDefs,
	skillToolDefs,
	subAgentToolDefs,
	workflowToolDefs,
	goalToolDefs,
	notifyToolDefs,
)

func concatToolDefs(groups ...[]llm.ToolDef) []llm.ToolDef {
	var all []llm.ToolDef
	for _, g := range groups {
		all = append(all, g...)
	}
	return all
}

