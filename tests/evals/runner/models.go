package runner

import (
	"fmt"
	"os"
	"strings"

	"nine/internal/llm"
	"nine/internal/llm/anthropic"
	llmollama "nine/internal/llm/ollama"
)

// ModelClass is the capability tier a model belongs to (docs/evals.md §6). It
// orders nano < small < medium < large, so a case's expected_pass_min_class can
// be compared against a model's class to decide whether a failure is fatal.
type ModelClass int

const (
	ClassNano ModelClass = iota
	ClassSmall
	ClassMedium
	ClassLarge
)

var classNames = map[ModelClass]string{
	ClassNano: "nano", ClassSmall: "small", ClassMedium: "medium", ClassLarge: "large",
}

func (c ModelClass) String() string { return classNames[c] }

// ParseClass parses a class name from a case's expected_pass_min_class.
func ParseClass(s string) (ModelClass, error) {
	switch s {
	case "nano":
		return ClassNano, nil
	case "small":
		return ClassSmall, nil
	case "medium":
		return ClassMedium, nil
	case "large":
		return ClassLarge, nil
	}
	return 0, fmt.Errorf("unknown model class %q", s)
}

// defaultClasses maps the example models from docs/evals.md §6 to their tier.
// Unknown models fall back to classOf's prefix heuristics.
var defaultClasses = map[string]ModelClass{
	"gemma4:e4b":   ClassNano,
	"llama3.2:3b":  ClassNano,
	"qwen3.5:9b":   ClassSmall,
	"llama3.1:8b":  ClassSmall,
	"gemma4:12b":   ClassMedium,
	"claude-haiku": ClassMedium,
}

// ClassOf returns a model's capability class: an explicit table entry, else a
// prefix heuristic (claude-opus/sonnet are large, other claude-* medium, small
// local models default to small).
func ClassOf(model string) ModelClass {
	if c, ok := defaultClasses[model]; ok {
		return c
	}
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "opus"), strings.Contains(m, "sonnet"), strings.Contains(m, "fable"):
		return ClassLarge
	case strings.HasPrefix(m, "claude"):
		return ClassMedium
	default:
		return ClassSmall
	}
}

// ProviderFor constructs an llm.Provider for a model name. claude-* models use
// the Anthropic provider (ANTHROPIC_API_KEY from the environment); everything
// else is treated as an Ollama model reached at NINE_LLM_ENDPOINT (default
// localhost:11434). Variance reduction (temperature 0, fixed seed) is left to the
// provider's defaults — Ollama is deterministic, Anthropic best-effort
// (docs/evals.md §5); N runs + a pass threshold absorb the residual variance.
func ProviderFor(model string) (llm.Provider, error) {
	if model == "" {
		return nil, fmt.Errorf("empty model name")
	}
	if strings.HasPrefix(strings.ToLower(model), "claude") {
		key := os.Getenv("ANTHROPIC_API_KEY")
		if key == "" {
			return nil, fmt.Errorf("model %q needs ANTHROPIC_API_KEY", model)
		}
		return anthropic.New(key, model, "", 120), nil
	}
	endpoint := os.Getenv("NINE_LLM_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://localhost:11434"
	}
	return llmollama.New(model, endpoint, 8192, false), nil
}

// ApplicableModels returns the models a case should run on: its models.include
// list intersected with the suite's requested models (or the full requested set
// when the case lists none). Order follows the requested set for stable grids.
func (c *Case) ApplicableModels(requested []string) []string {
	if len(c.Models.Include) == 0 {
		return requested
	}
	include := map[string]bool{}
	for _, m := range c.Models.Include {
		include[m] = true
	}
	var out []string
	for _, m := range requested {
		if include[m] {
			out = append(out, m)
		}
	}
	return out
}
