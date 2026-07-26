package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"

	ninectx "nine/internal/context"
	"nine/internal/embed"
	"nine/internal/llm"
)

// searchToolDefs are the definitions for the active, model-driven catalog-search
// meta-tools. They complement the passive, once-per-turn relevance ranking the
// context builder applies to tools (and the self-model assembler applies to
// skills): when the task drifts mid-turn or the user asks what tools/skills
// exist, the model can query the full catalog on demand rather than being
// limited to the top-K selected against the opening query
// (docs/tool-exposition.md). skill_search's def lives with the other skill
// tools in register_skills.go; only tool_search is defined here.
var searchToolDefs = []llm.ToolDef{toolSearchDef}

var toolSearchDef = llm.ToolDef{
	Name:        "tool_search",
	DisplayName: "Search Tools",
	Description: "Semantically search the tools available to you by a natural-language query, returning the most relevant tool names, descriptions, and input schemas. Use this when the tools you can currently see do not fit the task — a relevant tool may exist that was not surfaced up front. Call the returned tool directly by its name.",
	InputSchema: json.RawMessage(`{"type":"object","required":["query"],"properties":{"query":{"type":"string","description":"What capability you are looking for"},"top_k":{"type":"integer","description":"Number of results (default 5)"}}}`),
}

// toolSearchResult is one entry returned by tool_search.
type toolSearchResult struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	Score       float32         `json:"score"`
}

// RegisterToolSearch registers the tool_search handler. getTools returns the
// current loop's advertised (and therefore callable) tool set with per-tool
// embedding vectors — the same set the context builder ranks — so search
// results are always tools this loop is actually allowed to call. The search
// meta-tools themselves are excluded from results. A nil embedder disables
// registration (there is nothing to rank against).
func RegisterToolSearch(d *Dispatcher, embedder embed.Embedder, getTools func() []ninectx.ToolWithVector) {
	if embedder == nil || getTools == nil {
		return
	}
	d.handlers["tool_search"] = func(ctx context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Query string `json:"query"`
			TopK  int    `json:"top_k"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("tool_search: %w", err)
		}
		if req.Query == "" {
			return "", fmt.Errorf("tool_search: query is required")
		}
		if req.TopK <= 0 {
			req.TopK = 5
		}
		qvec, err := embedder.Embed(ctx, req.Query)
		if err != nil {
			return "", fmt.Errorf("embed: %w", err)
		}

		var out []toolSearchResult
		for _, tw := range getTools() {
			// Skip the search meta-tools themselves — the model already has them.
			if tw.Tool.Name == "tool_search" || tw.Tool.Name == "skill_search" {
				continue
			}
			var score float32
			if len(qvec) > 0 && len(tw.Vector) > 0 {
				score = cosineSim(qvec, tw.Vector)
			}
			out = append(out, toolSearchResult{
				Name:        tw.Tool.Name,
				Description: tw.Tool.Description,
				InputSchema: tw.Tool.InputSchema,
				Score:       score,
			})
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
		if len(out) > req.TopK {
			out = out[:req.TopK]
		}
		data, err := json.Marshal(map[string]any{"results": out})
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
}

// cosineSim returns the cosine similarity of two equal-length vectors, or 0 if
// the lengths differ or either has zero magnitude. Mirrors the context
// builder's ranking metric so tool_search orders results the same way the
// passive tool selection does.
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
