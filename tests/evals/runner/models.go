package runner

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"nine/internal/embed"
	"nine/internal/llm"
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
// A model that is not listed falls back to ClassOf's tag heuristic.
var defaultClasses = map[string]ModelClass{
	"gemma4:e2b":  ClassNano,
	"gemma4:e4b":  ClassNano,
	"llama3.2:3b": ClassNano,
	"qwen3.5:9b":  ClassSmall,
	"llama3.1:8b": ClassSmall,
	"gemma4:12b":  ClassMedium,
}

// ClassOf returns a model's capability class: an explicit table entry, else the
// parameter count in the Ollama tag (`qwen3.5:32b` → 32B → large), else small.
//
// The tag heuristic exists because every model is now a local one, and its
// parameter count is the only capability signal an Ollama tag carries. It is
// deliberately coarse; a model whose tag lies about its tier (gemma's `e4b`
// effective-parameter tags punch below their number) gets a table entry, which
// always wins.
func ClassOf(model string) ModelClass {
	if c, ok := defaultClasses[model]; ok {
		return c
	}
	if b, ok := paramsB(model); ok {
		switch {
		case b < 4:
			return ClassNano
		case b < 10:
			return ClassSmall
		case b < 20:
			return ClassMedium
		default:
			return ClassLarge
		}
	}
	return ClassSmall
}

// paramsB extracts the parameter count in billions from an Ollama tag —
// "qwen3.5:32b" → 32, "gemma4:e2b" → 2 (the leading "e" marks effective
// parameters), "llama3.2:1.5b" → 1.5. It reports false for a tag that names no
// size, which is every model whose tier only a table entry can settle.
func paramsB(model string) (float64, bool) {
	_, tag, ok := strings.Cut(strings.ToLower(model), ":")
	if !ok {
		return 0, false
	}
	// A tag may carry a quantization suffix ("32b-instruct-q4_K_M"); the size is
	// the first hyphen-separated field.
	size, _, _ := strings.Cut(tag, "-")
	size = strings.TrimSuffix(size, "b")
	size = strings.TrimPrefix(size, "e")
	b, err := strconv.ParseFloat(size, 64)
	if err != nil || b <= 0 {
		return 0, false
	}
	return b, true
}

// ProviderFor constructs an llm.Provider for a model name. Every model is an
// Ollama model reached at NINE_LLM_ENDPOINT (default localhost:11434) — Ollama
// is Nine's only chat backend. Variance reduction (temperature 0, fixed seed) is
// left to the provider's defaults, which Ollama makes deterministic
// (docs/evals.md §5); N runs + a pass threshold absorb the residual variance.
func ProviderFor(model string) (llm.Provider, error) {
	if model == "" {
		return nil, fmt.Errorf("empty model name")
	}
	endpoint := os.Getenv("NINE_LLM_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://localhost:11434"
	}
	return llmollama.New(model, endpoint, EvalNumCtx(), false, 0), nil
}

// DefaultEvalNumCtx is the Ollama context window live evals run with. A nine
// turn's prompt alone is ~7,000–8,000 tokens (system prompt, self-model and ~50
// tool schemas), and the window holds the reply too, so 8192 left a model a few
// hundred tokens to think and answer in, and Ollama cut the start of longer
// prompts. 16384 leaves room for a reply on a machine that cannot hold
// production's 32768; NINE_EVAL_NUM_CTX overrides it.
const DefaultEvalNumCtx = 16384

// EvalNumCtx returns the live context window: NINE_EVAL_NUM_CTX, else the default.
func EvalNumCtx() int {
	if v, err := strconv.Atoi(os.Getenv("NINE_EVAL_NUM_CTX")); err == nil && v > 0 {
		return v
	}
	return DefaultEvalNumCtx
}

// EvalContextBudget is the context budget nine assembles a live turn within:
// the window, less the default reply cap, so nine trims history itself instead
// of Ollama silently truncating the prompt.
func EvalContextBudget() int { return EvalNumCtx() - 2048 }

// EvalEmbedder builds the embedder the live harness uses so semantic-memory and
// related-session/tool-ranking features are exercised as in production (else those
// tools are never even registered — RegisterMemoryTools gates them on a non-nil
// embedder). It defaults to Ollama's nomic-embed-text at NINE_LLM_ENDPOINT;
// override the model via NINE_EVAL_EMBED_MODEL, or disable entirely with
// NINE_EVAL_EMBED_MODEL=none (semantic cases then can't pass — useful when no
// embedding backend is reachable).
func EvalEmbedder() embed.Embedder {
	model := os.Getenv("NINE_EVAL_EMBED_MODEL")
	if model == "none" {
		return nil
	}
	if model == "" {
		model = "nomic-embed-text"
	}
	endpoint := os.Getenv("NINE_LLM_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://localhost:11434"
	}
	return embed.Build("ollama", model, endpoint)
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
