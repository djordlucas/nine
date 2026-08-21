// Package ninectx assembles LLM turns from conversation state within a token
// budget, using priority-based trimming and embedding-based tool selection.
package ninectx

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"nine/internal/llm"
)

// --------- public types ---------

// Config controls context assembly.
type Config struct {
	Budget       int      // total token budget for the assembled turn
	ToolTopN     int      // max non-always-include tools to add (0 → 20)
	ExtrasBudget int      // min remaining tokens required to include extras (0 → 200)
	AlwaysTools  []string // tool names always included regardless of relevance
}

// ToolWithVector is a tool definition paired with its embedding for ranking.
type ToolWithVector struct {
	Tool   llm.ToolDef
	Vector []float32 // nil → score 0 (no embedding available)
}

// ScratchpadEntry is one iteration of the ReAct loop.
type ScratchpadEntry struct {
	Thought     string
	ToolName    string // empty when no tool was called
	ToolCallID  string
	ToolArgs    json.RawMessage
	Observation string
}

// BuildInput is the full state the builder uses to assemble one LLM turn.
type BuildInput struct {
	SystemCore   string // priority 1: never trimmed
	SystemExtras string // priority 5: dropped when budget is tight
	SystemSelf   string // priority 2.5: self-model, capped at 600 tokens
	// SystemEnrichment is out-of-band pull-surfaced context (a related prior
	// session; adr/reactive-events.md §5). Priority 2.6: capped small and
	// dropped when budget is tight, so enrichment never crowds out the turn.
	SystemEnrichment string
	Tools            []ToolWithVector  // priority 2: relevance-filtered
	QueryVector      []float32         // embedding of the current query; nil → no ranking
	History          []llm.Message     // priority 3: rolling window, oldest dropped
	Scratchpad       []ScratchpadEntry // priority 4: oldest entries dropped first

	// SystemPlan is the advisory request-analysis produced by the no-tool plan pass.
	// Priority 4.5 - capped and dropped when budget is tight and it yields to scratchpad entries.
	// A weak model's plan must bever crowd out the actual tool observations.
	SystemPlan string
}

// Builder assembles llm.Request values within a token budget.
type Builder struct{ cfg Config }

// New returns a Builder with the given config.
func New(cfg Config) *Builder { return &Builder{cfg: cfg} }

// Build assembles an llm.Request, respecting the token budget.
func (b *Builder) Build(input BuildInput) llm.Request {
	req, _ := b.BuildWithUsage(input)
	return req
}

// BuildWithUsage is like Build but also returns the number of tokens consumed.
func (b *Builder) BuildWithUsage(input BuildInput) (llm.Request, int) {
	req, used, _ := b.assemble(input, false)
	return req, used
}

// BuildReport assembles the context exactly as BuildWithUsage does, but records a
// per-section token breakdown alongside the fully-assembled request. It performs
// no LLM call — it is the same deterministic assembly, instrumented for
// inspection.
func (b *Builder) BuildReport(input BuildInput) Report {
	_, _, rep := b.assemble(input, true)
	return rep
}

// assemble is the single source of truth for context assembly. When report is
// true it fills and returns a Report describing how the budget was spent; when
// false the returned Report is the zero value (the hot path skips the extra
// bookkeeping).
func (b *Builder) assemble(input BuildInput, report bool) (llm.Request, int, Report) {
	remaining := b.cfg.Budget
	var rep Report
	add := func(name, priority string, tokens int, included bool, detail string) {
		if report {
			rep.Sections = append(rep.Sections, Section{
				Name: name, Priority: priority, Tokens: tokens, Included: included, Detail: detail,
			})
		}
	}

	// Priority 1 — system core: always present.
	coreTokens := countTokens(input.SystemCore)
	remaining -= coreTokens
	add("system-core", "1", coreTokens, true, "always included")

	// Priority 2 — tool definitions: relevance-filtered.
	tools := b.selectTools(input.Tools, input.QueryVector, remaining)
	toolTok := 0
	for _, t := range tools {
		toolTok += toolTokens(t)
	}
	remaining -= toolTok
	add("tools", "2", toolTok, len(tools) > 0,
		fmt.Sprintf("%d of %d selected", len(tools), len(input.Tools)))

	// Priority 2.5 — self model: capped at 600 tokens, omitted if budget is tight.
	selfModel := ""
	const selfModelCap = 600
	if input.SystemSelf != "" {
		cost := min(countTokens(input.SystemSelf), selfModelCap)
		if remaining >= cost {
			selfModel = truncateTokens(input.SystemSelf, selfModelCap)
			remaining -= cost
			add("self-model", "2.5", cost, true, fmt.Sprintf("capped at %d tokens", selfModelCap))
		} else {
			add("self-model", "2.5", 0, false, "dropped: budget too tight")
		}
	} else {
		add("self-model", "2.5", 0, false, "empty")
	}

	// Priority 2.6 — enrichment: pull-surfaced related context, capped and
	// dropped when budget is tight (enrich, don't crowd out the turn).
	enrichment := ""
	const enrichmentCap = 300
	if input.SystemEnrichment != "" {
		cost := min(countTokens(input.SystemEnrichment), enrichmentCap)
		if remaining >= cost {
			enrichment = truncateTokens(input.SystemEnrichment, enrichmentCap)
			remaining -= cost
			add("enrichment", "2.6", cost, true, fmt.Sprintf("capped at %d tokens", enrichmentCap))
		} else {
			add("enrichment", "2.6", 0, false, "dropped: budget too tight")
		}
	} else {
		add("enrichment", "2.6", 0, false, "empty")
	}

	// Priority 3 — message history: keep most recent, drop oldest.
	history := trimFront(input.History, remaining, messageTokens)
	histTok := 0
	for _, m := range history {
		histTok += messageTokens(m)
	}
	remaining -= histTok
	add("history", "3", histTok, len(history) > 0,
		fmt.Sprintf("keeps %d of %d messages", len(history), len(input.History)))

	// Priority 4 — scratchpad: keep most recent entries, drop oldest.
	scratchpad := trimFront(input.Scratchpad, remaining, scratchpadTokens)
	scratchTok := 0
	for _, e := range scratchpad {
		scratchTok += scratchpadTokens(e)
	}
	remaining -= scratchTok
	add("scratchpad", "4", scratchTok, len(scratchpad) > 0,
		fmt.Sprintf("keeps %d of %d entries", len(scratchpad), len(input.Scratchpad)))

	// Priority 4.5 - analysis plan: advisory, capped, dropped when budget is tight and it yields to scratchpad entries.
	plan := ""
	const analysisPlanCap = 500
	if input.SystemPlan != "" {
		cost := min(countTokens(input.SystemPlan), analysisPlanCap)
		if remaining >= cost {
			plan = truncateTokens(input.SystemPlan, analysisPlanCap)
			remaining -= cost
			add("plan", "4.5", cost, true, fmt.Sprintf("capped at %d tokens", analysisPlanCap))
		} else {
			add("plan", "4.5", 0, false, "dropped: budget too tight")
		}
	} else {
		add("plan", "4.5", 0, false, "empty")
	}

	// Priority 5 — system extras: include only when budget allows.
	system := input.SystemCore
	if selfModel != "" {
		system += "\n\n" + selfModel
	}

	if enrichment != "" {
		system += "\n\n" + enrichment
	}

	if plan != "" {
		system += "\n\n" + plan
	}

	extrasIncluded := input.SystemExtras != "" && remaining >= b.extrasBudget()
	if extrasIncluded {
		system += "\n\n" + input.SystemExtras
		// Note: extras deliberately do not decrement `remaining` — matching the
		// long-standing budgeting behaviour, `used` does not count the extras band.
		add("extras", "5", countTokens(input.SystemExtras), true, "included (not counted in used)")
	} else if input.SystemExtras != "" {
		add("extras", "5", 0, false, fmt.Sprintf("dropped: needs %d free tokens", b.extrasBudget()))
	} else {
		add("extras", "5", 0, false, "empty")
	}

	// Assemble messages: history followed by scratchpad entries.
	messages := append([]llm.Message(nil), history...)
	for _, e := range scratchpad {
		messages = append(messages, entryToMessages(e)...)
	}

	used := b.cfg.Budget - remaining
	if report {
		rep.Budget = b.cfg.Budget
		rep.Used = used
		rep.System = system
		rep.Messages = messages
	}
	return llm.Request{System: system, Messages: messages, Tools: tools}, used, rep
}

// Budget returns the total token budget this builder was configured with.
func (b *Builder) Budget() int { return b.cfg.Budget }

// --------- tool selection ---------

func (b *Builder) selectTools(tools []ToolWithVector, queryVec []float32, budget int) []llm.ToolDef {
	alwaysSet := make(map[string]bool, len(b.cfg.AlwaysTools))
	for _, n := range b.cfg.AlwaysTools {
		alwaysSet[n] = true
	}

	type candidate struct {
		tool   llm.ToolDef
		score  float32
		always bool
	}
	cs := make([]candidate, len(tools))
	for i, tw := range tools {
		s := float32(0)
		if len(queryVec) > 0 && len(tw.Vector) > 0 {
			s = cosineSim(queryVec, tw.Vector)
		}
		cs[i] = candidate{tool: tw.Tool, score: s, always: alwaysSet[tw.Tool.Name]}
	}

	// Always-include tools first, then descending relevance.
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].always != cs[j].always {
			return cs[i].always
		}
		return cs[i].score > cs[j].score
	})

	topN := b.toolTopN()
	var selected []llm.ToolDef
	nonAlways := 0
	remaining := budget

	for _, c := range cs {
		cost := toolTokens(c.tool)
		if c.always {
			selected = append(selected, c.tool)
			remaining -= cost
		} else {
			if nonAlways >= topN {
				break
			}
			if remaining < cost {
				continue
			}
			selected = append(selected, c.tool)
			remaining -= cost
			nonAlways++
		}
	}
	return selected
}

// --------- helpers ---------

// trimFront keeps the tail of s whose total cost fits within budget,
// dropping the oldest (front) elements first.
func trimFront[T any](s []T, budget int, cost func(T) int) []T {
	if budget <= 0 {
		return nil
	}
	total := 0
	for _, item := range s {
		total += cost(item)
	}
	if total <= budget {
		return s
	}
	start := 0
	for total > budget && start < len(s) {
		total -= cost(s[start])
		start++
	}
	return s[start:]
}

func entryToMessages(e ScratchpadEntry) []llm.Message {
	aMsg := llm.Message{Role: "assistant", Text: e.Thought}
	if e.ToolName != "" {
		aMsg.ToolCalls = []llm.ToolCall{{
			ID:    e.ToolCallID,
			Name:  e.ToolName,
			Input: e.ToolArgs,
		}}
	}
	msgs := []llm.Message{aMsg}
	if e.ToolName != "" {
		msgs = append(msgs, llm.Message{
			Role: "user",
			ToolResults: []llm.ToolResult{{
				ToolCallID: e.ToolCallID,
				Content:    e.Observation,
			}},
		})
	}
	return msgs
}

// --------- token counting ---------

// countTokens estimates the token count of s using the 4-chars-per-token
// approximation common for Claude models.
func countTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + 3) / 4
}

func toolTokens(t llm.ToolDef) int {
	return countTokens(t.Name + " " + t.Description + " " + string(t.InputSchema))
}

func messageTokens(m llm.Message) int {
	n := countTokens(m.Text)
	for _, tc := range m.ToolCalls {
		n += countTokens(tc.Name + " " + string(tc.Input))
	}
	for _, tr := range m.ToolResults {
		n += countTokens(tr.Content)
	}
	return n
}

func scratchpadTokens(e ScratchpadEntry) int {
	n := countTokens(e.Thought + " " + e.Observation)
	if e.ToolName != "" {
		n += countTokens(e.ToolName + " " + string(e.ToolArgs))
	}
	return n
}

// --------- cosine similarity ---------

func cosineSim(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(na) * math.Sqrt(nb)))
}

// --------- config defaults ---------

func (b *Builder) toolTopN() int {
	if b.cfg.ToolTopN > 0 {
		return b.cfg.ToolTopN
	}
	return 20
}

func (b *Builder) extrasBudget() int {
	if b.cfg.ExtrasBudget > 0 {
		return b.cfg.ExtrasBudget
	}
	return 200
}

// truncateTokens truncates s to approximately maxTokens using the 4-chars-per-token estimate.
func truncateTokens(s string, maxTokens int) string {
	limit := maxTokens * 4
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}
