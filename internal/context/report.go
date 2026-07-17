package ninectx

import "nine/internal/llm"

// Report is a breakdown of an assembled context: how the token budget was spent
// per priority section, plus the fully-assembled system prompt and messages for
// verbose inspection. It is produced by Builder.BuildReport and involves no LLM
// call — only the same deterministic assembly the loop runs before each turn.
type Report struct {
	Budget   int           `json:"budget"`
	Used     int           `json:"used"`
	Sections []Section     `json:"sections"`
	System   string        `json:"system"`   // fully-assembled system prompt
	Messages []llm.Message `json:"messages"` // fully-assembled message list
}

// Section is one priority band's contribution to the assembled context.
type Section struct {
	Name     string `json:"name"`     // "system-core", "tools", "self-model", ...
	Priority string `json:"priority"` // priority band label, e.g. "1", "2", "2.5"
	Tokens   int    `json:"tokens"`   // tokens this section consumed (0 when dropped)
	Included bool   `json:"included"` // false when the section was dropped for budget
	Detail   string `json:"detail"`   // human-readable note, e.g. "12 of 45 tools"
}
