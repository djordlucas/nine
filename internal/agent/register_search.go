package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"

	ninectx "nine/internal/context"
	"nine/internal/docindex"
	"nine/internal/embed"
	"nine/internal/llm"
)

// searchToolDefs are the definitions for the active, model-driven catalog
// meta-tools. They complement the passive, once-per-turn relevance ranking the
// context builder applies to tools (and the self-model assembler applies to
// skills): when the task drifts mid-turn or the user asks what tools/skills
// exist, the model can query the full catalog on demand rather than being
// limited to the top-K selected against the opening query
// (adr/tool-exposition.md). Each catalog has a query-driven search and a
// query-free enumeration: tool_search/tool_list here, skill_search/skill_list
// with the other skill tools in register_skills.go.
var searchToolDefs = []llm.ToolDef{toolSearchDef, toolListDef}

var toolSearchDef = llm.ToolDef{
	Name:        "tool_search",
	DisplayName: "Search Tools",
	Description: "Semantically search the tools available to you by a natural-language query, returning the most relevant tool names, descriptions, and input schemas. Use this when the tools you can currently see do not fit the task — a relevant tool may exist that was not surfaced up front. Call the returned tool directly by its name.",
	InputSchema: json.RawMessage(`{"type":"object","required":["query"],"properties":{"query":{"type":"string","description":"What capability you are looking for"},"top_k":{"type":"integer","description":"Number of results (default 5)"}}}`),
}

// ToolSearchDeferredDef is tool_search as a deferred loop advertises it: the
// load path for the tools its index lists (adr/deferred-tool-schemas.md §3.3).
// A ranked loop keeps toolSearchDef, so the ranked arm's prompt is unchanged.
var ToolSearchDeferredDef = llm.ToolDef{
	Name:        "tool_search",
	DisplayName: "Search Tools",
	Description: "Load the full schema of tools listed under More tools, by name or toolkit, or find tools by what they do. Loaded tools stay available for the rest of the session.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"names":{"type":"array","items":{"type":"string"},"description":"Tool or toolkit names to load"},"query":{"type":"string","description":"What capability you are looking for"},"top_k":{"type":"integer","description":"Number of query results (default 3)"}}}`),
}

var toolListDef = llm.ToolDef{
	Name:        "tool_list",
	DisplayName: "List Tools",
	Description: "List every tool available to you, with names and descriptions. Use this when you need the whole catalog rather than the best matches for a keyword — for example when the user asks what you can do or which tools you have. Prefer tool_search when you are looking for a specific capability.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"include_schemas":{"type":"boolean","description":"Also return each tool's input schema (verbose; default false)"}}}`),
}

// toolListEntry is one entry returned by tool_list. InputSchema is omitted
// unless the caller asked for schemas, so the default listing stays compact
// enough to survive the dispatcher's output cap on a large catalog.
type toolListEntry struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// RegisterToolList registers the tool_list handler: a query-free enumeration of
// the loop's advertised (and therefore callable) tool set, the counterpart to
// skill_list for the tool catalog. getTools is the same closure tool_search
// ranks over, so the two agree on what "available" means — the advertised set,
// which excludes registered-but-unadvertised tools such as gap_report. Unlike
// tool_search it needs no embedder: enumeration does not rank.
func RegisterToolList(d *Dispatcher, getTools func() []ninectx.ToolWithVector) {
	if getTools == nil {
		return
	}
	d.handlers["tool_list"] = func(_ context.Context, args json.RawMessage) (string, error) {
		var req struct {
			IncludeSchemas bool `json:"include_schemas"`
		}
		// An absent or empty argument object is a valid "list everything".
		if len(args) > 0 {
			if err := json.Unmarshal(args, &req); err != nil {
				return "", fmt.Errorf("tool_list: %w", err)
			}
		}
		tools := getTools()
		out := make([]toolListEntry, 0, len(tools))
		for _, tw := range tools {
			e := toolListEntry{Name: tw.Tool.Name, Description: tw.Tool.Description}
			if req.IncludeSchemas {
				e.InputSchema = tw.Tool.InputSchema
			}
			out = append(out, e)
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		data, err := json.Marshal(map[string]any{"count": len(out), "tools": out})
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
}

// toolSearchResult is one entry returned by tool_search.
type toolSearchResult struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
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

// RegisterDeferredToolSearch registers tool_search for a deferred loop. names
// loads tools, or every tool of a toolkit, by exact name; query ranks the
// catalog lexically (BM25), fused with the embedder's ranking when there is one,
// so the load path never depends on an embedder. Every tool returned is
// promoted, so from the loop's next request on the model calls it as a declared
// tool. Results, like the ranked handler's, come only from getTools: the tools
// this loop may call.
func RegisterDeferredToolSearch(d *Dispatcher, embedder embed.Embedder, getTools func() []ninectx.ToolWithVector, promotions *Promotions) {
	if getTools == nil || promotions == nil {
		return
	}
	d.handlers["tool_search"] = func(ctx context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Names []string `json:"names"`
			Query string   `json:"query"`
			TopK  int      `json:"top_k"`
		}
		if len(args) > 0 {
			if err := json.Unmarshal(args, &req); err != nil {
				return "", fmt.Errorf("tool_search: %w", err)
			}
		}
		if len(req.Names) == 0 && req.Query == "" {
			return "", fmt.Errorf("tool_search: give names to load or a query")
		}
		tools := getTools()
		var (
			found    []ninectx.ToolWithVector
			notFound []string
		)
		if len(req.Names) > 0 {
			found, notFound = toolsByName(tools, req.Names)
		} else {
			// Fewer than the ranked handler's 5: every result is promoted, and
			// each costs its schema on every request until demoted.
			if req.TopK <= 0 {
				req.TopK = 3
			}
			found = rankTools(ctx, embedder, tools, req.Query, req.TopK)
		}
		out := make([]toolSearchResult, 0, len(found))
		names := make([]string, 0, len(found))
		for _, tw := range found {
			out = append(out, toolSearchResult{Name: tw.Tool.Name, Description: tw.Tool.Description, InputSchema: tw.Tool.InputSchema})
			names = append(names, tw.Tool.Name)
		}
		promotions.Promote(names, PromoteSearch)
		res := map[string]any{"results": out}
		if len(notFound) > 0 {
			res["not_found"] = notFound
		}
		data, err := json.Marshal(res)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
}

// toolsByName resolves names against tools: a tool's own name, or a toolkit's,
// which stands for each of its tools. The catalog meta-tools are never results.
func toolsByName(tools []ninectx.ToolWithVector, names []string) (found []ninectx.ToolWithVector, notFound []string) {
	seen := map[string]bool{}
	for _, n := range names {
		matched := false
		for _, tw := range tools {
			if tw.Tool.Name != n && tw.Toolkit != n {
				continue
			}
			matched = true
			if !seen[tw.Tool.Name] && !isSearchMeta(tw.Tool.Name) {
				seen[tw.Tool.Name] = true
				found = append(found, tw)
			}
		}
		if !matched {
			notFound = append(notFound, n)
		}
	}
	return found, notFound
}

// rankTools returns the topK tools best matching query: BM25 over each tool's
// name, toolkit and description, fused by reciprocal rank with cosine
// similarity when an embedder is configured.
func rankTools(ctx context.Context, embedder embed.Embedder, tools []ninectx.ToolWithVector, query string, topK int) []ninectx.ToolWithVector {
	byName := make(map[string]ninectx.ToolWithVector, len(tools))
	secs := make([]docindex.Section, 0, len(tools))
	for _, tw := range tools {
		if isSearchMeta(tw.Tool.Name) {
			continue
		}
		byName[tw.Tool.Name] = tw
		secs = append(secs, docindex.Section{Addr: tw.Tool.Name, Title: tw.Toolkit, Body: tw.Tool.Description})
	}
	lists := [][]string{docindex.NewLexical(secs).Rank(query)}
	if embedder != nil {
		if qvec, err := embedder.Embed(ctx, query); err == nil && len(qvec) > 0 {
			type scored struct {
				name  string
				score float32
			}
			var vs []scored
			for name, tw := range byName {
				if len(tw.Vector) > 0 {
					vs = append(vs, scored{name, cosineSim(qvec, tw.Vector)})
				}
			}
			sort.Slice(vs, func(i, j int) bool {
				if vs[i].score != vs[j].score {
					return vs[i].score > vs[j].score
				}
				return vs[i].name < vs[j].name
			})
			vec := make([]string, len(vs))
			for i, v := range vs {
				vec[i] = v.name
			}
			lists = append(lists, vec)
		}
	}
	ranked := docindex.FuseRRF(docindex.RRFK, lists...)
	if len(ranked) > topK {
		ranked = ranked[:topK]
	}
	out := make([]ninectx.ToolWithVector, 0, len(ranked))
	for _, n := range ranked {
		out = append(out, byName[n])
	}
	return out
}

// isSearchMeta reports the catalog search tools, which the model already has.
func isSearchMeta(name string) bool {
	return slices.Contains([]string{"tool_search", "skill_search"}, name)
}
