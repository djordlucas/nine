package runner

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// RunMetrics measures one run's cost, so two arms can be compared on more than
// their pass rates (adr/deferred-tool-schemas.md §7.1, §7.4).
type RunMetrics struct {
	Turns        int `json:"turns"`
	LLMCalls     int `json:"llm_calls"`
	ToolSearches int `json:"tool_searches"`
	Promotions   int `json:"promotions"`
	// PromptTokens sums every request's input: the provider's count where it
	// reports one, else the builder's estimate.
	PromptTokens int `json:"prompt_tokens"`
	// ToolTokensMax is the largest estimated tool block any request carried.
	ToolTokensMax int `json:"tool_tokens_max"`
	// ToolCalls counts calls per tool; the report sums them into ToolUse.
	ToolCalls map[string]int `json:"tool_calls,omitempty"`
}

func metricsOf(t *trace) RunMetrics {
	return RunMetrics{
		Turns:         t.userTurns,
		LLMCalls:      t.llmCalls,
		ToolSearches:  t.toolCalls["tool_search"],
		Promotions:    t.promotions,
		PromptTokens:  t.promptTokens(),
		ToolTokensMax: t.toolTokensMax,
		ToolCalls:     t.toolCalls,
	}
}

// ToolUse is how often one tool was called across a report's live runs: the
// data the deferred mode's resident set is derived from (§3.1).
type ToolUse struct {
	Name string `json:"name"`
	// Runs is the number of graded runs that called the tool at least once.
	Runs  int `json:"runs"`
	Calls int `json:"calls"`
}

// toolUse sums the per-run tool calls of every graded run, most-used first.
func toolUse(results []CaseModelResult) (use []ToolUse, runs int) {
	byName := map[string]*ToolUse{}
	for _, r := range results {
		for _, o := range r.Runs {
			if o.Err != "" {
				continue
			}
			runs++
			for name, n := range o.Metrics.ToolCalls {
				u := byName[name]
				if u == nil {
					u = &ToolUse{Name: name}
					byName[name] = u
				}
				u.Runs++
				u.Calls += n
			}
		}
	}
	for _, u := range byName {
		use = append(use, *u)
	}
	slices.SortFunc(use, func(a, b ToolUse) int {
		return cmp.Or(b.Runs-a.Runs, b.Calls-a.Calls, strings.Compare(a.Name, b.Name))
	})
	return use, runs
}

// ArmSummary is one model and arm's cost over the runs that passed: the
// figures §7.5 compares arms by.
type ArmSummary struct {
	Model string `json:"model"`
	Arm   string `json:"arm,omitempty"`
	// Passed and Runs count graded runs.
	Passed int `json:"passed"`
	Runs   int `json:"runs"`
	// The medians are over passed runs only: a failed run's cost says nothing
	// about what the work costs.
	MedianPromptTokens int `json:"median_prompt_tokens"`
	MedianTurns        int `json:"median_turns"`
	MedianLLMCalls     int `json:"median_llm_calls"`
	ToolTokensMax      int `json:"tool_tokens_max"`
}

// summarize groups results by model and arm, in first-seen order.
func summarize(results []CaseModelResult) []ArmSummary {
	type acc struct {
		sum                    ArmSummary
		prompt, turns, llmCall []int
	}
	var order []string
	byKey := map[string]*acc{}
	for _, r := range results {
		key := r.Model + "\x00" + r.Arm
		a := byKey[key]
		if a == nil {
			a = &acc{sum: ArmSummary{Model: r.Model, Arm: r.Arm}}
			byKey[key] = a
			order = append(order, key)
		}
		for _, o := range r.Runs {
			if o.Err != "" {
				continue
			}
			a.sum.Runs++
			a.sum.ToolTokensMax = max(a.sum.ToolTokensMax, o.Metrics.ToolTokensMax)
			if !o.Pass {
				continue
			}
			a.sum.Passed++
			a.prompt = append(a.prompt, o.Metrics.PromptTokens)
			a.turns = append(a.turns, o.Metrics.Turns)
			a.llmCall = append(a.llmCall, o.Metrics.LLMCalls)
		}
	}
	out := make([]ArmSummary, 0, len(order))
	for _, key := range order {
		a := byKey[key]
		a.sum.MedianPromptTokens = median(a.prompt)
		a.sum.MedianTurns = median(a.turns)
		a.sum.MedianLLMCalls = median(a.llmCall)
		out = append(out, a.sum)
	}
	return out
}

// median returns the middle value, the lower of the two middles for an even
// count, or 0 for none.
func median(xs []int) int {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	return s[(len(s)-1)/2]
}

// RenderCost renders the per-model, per-arm summary and the tool-use table.
func (r *Report) RenderCost() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-16s %-9s %-9s %14s %12s %10s %14s\n",
		"model", "arm", "passed", "prompt tok/run", "turns/run", "llm/run", "tool tok max")
	for _, s := range r.Summary {
		arm := s.Arm
		if arm == "" {
			arm = "-"
		}
		fmt.Fprintf(&b, "%-16s %-9s %-9s %14d %12d %10d %14d\n",
			truncate(s.Model, 16), arm, fmt.Sprintf("%d/%d", s.Passed, s.Runs),
			s.MedianPromptTokens, s.MedianTurns, s.MedianLLMCalls, s.ToolTokensMax)
	}
	b.WriteString("\nmedians over passed runs\n")
	if len(r.ToolUse) > 0 {
		fmt.Fprintf(&b, "\n%-28s %12s %8s\n", "tool", "runs", "calls")
		for _, u := range r.ToolUse {
			fmt.Fprintf(&b, "%-28s %12s %8d\n", truncate(u.Name, 28),
				fmt.Sprintf("%d/%d", u.Runs, r.GradedRuns), u.Calls)
		}
	}
	return b.String()
}
