package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nine/internal/agent"
	ninectx "nine/internal/context"
	"nine/internal/embed"
	"nine/internal/llm"
)

// axisEmbedder maps a keyword to a one-hot axis so cosine similarity is
// deterministic: a query about "deploy" ranks the deploy item first.
func axisEmbedder() embed.Embedder {
	axis := func(s string) []float32 {
		v := []float32{0, 0, 0}
		switch {
		case strings.Contains(s, "deploy"):
			v[0] = 1
		case strings.Contains(s, "database"), strings.Contains(s, "sql"):
			v[1] = 1
		default:
			v[2] = 1
		}
		return v
	}
	return embed.EmbedderFunc(func(_ context.Context, text string) ([]float32, error) {
		return axis(text), nil
	})
}

// tool_search ranks the advertised tool set against the query and returns the
// most relevant tool with its schema, excluding the search meta-tools.
func TestToolSearch(t *testing.T) {
	d := agent.New()
	e := axisEmbedder()

	defs := []llm.ToolDef{
		{Name: "deploy_app", Description: "deploy the application", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "run_sql", Description: "run a database query", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "tool_search", Description: "search tools"},
	}
	getTools := func() []ninectx.ToolWithVector {
		out := make([]ninectx.ToolWithVector, len(defs))
		for i, def := range defs {
			vec, _ := e.Embed(context.Background(), def.Description)
			out[i] = ninectx.ToolWithVector{Tool: def, Vector: vec}
		}
		return out
	}
	agent.RegisterToolSearch(d, e, getTools)

	res, err := d.Dispatch(context.Background(), "tool_search", json.RawMessage(`{"query":"deploy the service","top_k":1}`))
	if err != nil {
		t.Fatalf("tool_search: %v", err)
	}
	var out struct {
		Results []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(res.Output), &out); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, res.Output)
	}
	if len(out.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(out.Results))
	}
	if out.Results[0].Name != "deploy_app" {
		t.Errorf("top result = %q, want deploy_app", out.Results[0].Name)
	}
	if len(out.Results[0].InputSchema) == 0 {
		t.Error("result should include the tool's input schema")
	}
	// The search meta-tool never appears in its own results.
	if strings.Contains(res.Output, "tool_search") {
		t.Errorf("results should exclude tool_search itself: %s", res.Output)
	}
}

// tool_search requires a non-empty query and a nil embedder registers nothing.
func TestToolSearchGuards(t *testing.T) {
	d := agent.New()
	agent.RegisterToolSearch(d, axisEmbedder(), func() []ninectx.ToolWithVector { return nil })
	if _, err := d.Dispatch(context.Background(), "tool_search", json.RawMessage(`{"query":""}`)); err == nil {
		t.Error("empty query should error")
	}

	dNil := agent.New()
	agent.RegisterToolSearch(dNil, nil, func() []ninectx.ToolWithVector { return nil })
	if _, err := dNil.Dispatch(context.Background(), "tool_search", json.RawMessage(`{"query":"x"}`)); err == nil {
		t.Error("nil embedder must not register tool_search (unknown tool expected)")
	}
}

// tool_list enumerates the whole advertised set — no query, nothing pruned —
// sorted by name, with schemas omitted unless asked for.
func TestToolList(t *testing.T) {
	d := agent.New()
	defs := []llm.ToolDef{
		{Name: "run_sql", Description: "run a database query", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "deploy_app", Description: "deploy the application", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "tool_search", Description: "search tools", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	getTools := func() []ninectx.ToolWithVector {
		out := make([]ninectx.ToolWithVector, len(defs))
		for i, def := range defs {
			out[i] = ninectx.ToolWithVector{Tool: def}
		}
		return out
	}
	agent.RegisterToolList(d, getTools)

	type listOut struct {
		Count int `json:"count"`
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
	}

	// No arguments at all is a valid "list everything".
	res, err := d.Dispatch(context.Background(), "tool_list", nil)
	if err != nil {
		t.Fatalf("tool_list: %v", err)
	}
	var out listOut
	if err := json.Unmarshal([]byte(res.Output), &out); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, res.Output)
	}
	if out.Count != len(defs) || len(out.Tools) != len(defs) {
		t.Fatalf("count = %d / %d tools, want %d of each", out.Count, len(out.Tools), len(defs))
	}
	// Sorted by name, and the meta-tools are listed too — unlike tool_search,
	// this is a complete enumeration of what the loop can call.
	want := []string{"deploy_app", "run_sql", "tool_search"}
	for i, w := range want {
		if out.Tools[i].Name != w {
			t.Errorf("tools[%d] = %q, want %q", i, out.Tools[i].Name, w)
		}
	}
	if out.Tools[0].Description != "deploy the application" {
		t.Errorf("description = %q, want the tool's description", out.Tools[0].Description)
	}
	if len(out.Tools[0].InputSchema) != 0 {
		t.Errorf("schemas should be omitted by default, got %s", out.Tools[0].InputSchema)
	}

	res, err = d.Dispatch(context.Background(), "tool_list", json.RawMessage(`{"include_schemas":true}`))
	if err != nil {
		t.Fatalf("tool_list with schemas: %v", err)
	}
	out = listOut{}
	if err := json.Unmarshal([]byte(res.Output), &out); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, res.Output)
	}
	if len(out.Tools) == 0 || len(out.Tools[0].InputSchema) == 0 {
		t.Errorf("include_schemas should return input schemas: %s", res.Output)
	}
}

// tool_list needs no embedder — enumeration does not rank — but a nil getTools
// registers nothing.
func TestToolListGuards(t *testing.T) {
	d := agent.New()
	agent.RegisterToolList(d, func() []ninectx.ToolWithVector { return nil })
	res, err := d.Dispatch(context.Background(), "tool_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("tool_list on an empty catalog should succeed: %v", err)
	}
	if !strings.Contains(res.Output, `"count":0`) {
		t.Errorf("empty catalog = %q, want count 0", res.Output)
	}

	dNil := agent.New()
	agent.RegisterToolList(dNil, nil)
	if _, err := dNil.Dispatch(context.Background(), "tool_list", json.RawMessage(`{}`)); err == nil {
		t.Error("nil getTools must not register tool_list (unknown tool expected)")
	}
}

// skill_search ranks the skills namespace against the query and returns names
// with descriptions; it is only registered when an embedder is present.
func TestSkillSearch(t *testing.T) {
	store := newTestStore(t)
	d := agent.New()
	e := axisEmbedder()
	agent.RegisterSkillTools(d, store, e)
	ctx := context.Background()

	if _, err := d.Dispatch(ctx, "skill_write",
		json.RawMessage(`{"name":"deploy","description":"how to deploy the app","content":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Dispatch(ctx, "skill_write",
		json.RawMessage(`{"name":"dbtips","description":"database and sql tips","content":"y"}`)); err != nil {
		t.Fatal(err)
	}

	res, err := d.Dispatch(ctx, "skill_search", json.RawMessage(`{"query":"deploy to prod","top_k":1}`))
	if err != nil {
		t.Fatalf("skill_search: %v", err)
	}
	if !strings.Contains(res.Output, "deploy") {
		t.Errorf("skill_search = %q, want the deploy skill ranked first", res.Output)
	}
	if !strings.Contains(res.Output, "how to deploy the app") {
		t.Errorf("skill_search = %q, want it to include the description", res.Output)
	}

	// Without an embedder, skill_search is not registered.
	dNil := agent.New()
	agent.RegisterSkillTools(dNil, newTestStore(t), nil)
	if _, err := dNil.Dispatch(ctx, "skill_search", json.RawMessage(`{"query":"x"}`)); err == nil {
		t.Error("skill_search must be absent without an embedder")
	}
}
