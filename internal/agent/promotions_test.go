package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"nine/internal/agent"
	ninectx "nine/internal/context"
	"nine/internal/llm"
)

func TestPromotions_FitDemotesLeastRecentlyUsed(t *testing.T) {
	p := agent.NewPromotions()
	p.Promote([]string{"a", "b"}, agent.PromoteSearch)
	p.Promote([]string{"c"}, agent.PromoteCall)
	p.Called("a") // b is now the least recently used
	cost := func(name string) (int, bool) { return 100, name != "gone" }

	p.Fit(200, cost)
	if got := p.Names(); !slices.Equal(got, []string{"a", "c"}) {
		t.Errorf("after Fit = %v, want [a c]", got)
	}
	want := []agent.PromotionEvent{
		{Name: "a", Reason: agent.PromoteSearch, Promoted: true},
		{Name: "b", Reason: agent.PromoteSearch, Promoted: true},
		{Name: "c", Reason: agent.PromoteCall, Promoted: true},
		{Name: "b"},
	}
	if got := p.Drain(); !slices.Equal(got, want) {
		t.Errorf("events = %+v, want %+v", got, want)
	}
	if got := p.Drain(); got != nil {
		t.Errorf("a second Drain returned %v", got)
	}

	// Promoting an already promoted tool records no event and keeps its place.
	p.Promote([]string{"c"}, agent.PromoteSearch)
	if got := p.Names(); !slices.Equal(got, []string{"a", "c"}) || p.Drain() != nil {
		t.Errorf("re-promotion changed the set: %v", got)
	}
}

func TestPromotions_FitDropsToolsTheCatalogLost(t *testing.T) {
	p := agent.NewPromotions()
	p.Promote([]string{"gone", "kept"}, agent.PromoteSearch)
	p.Drain()
	p.Fit(1000, func(name string) (int, bool) { return 10, name != "gone" })
	if got := p.Names(); !slices.Equal(got, []string{"kept"}) {
		t.Errorf("names = %v", got)
	}
	if got := p.Drain(); !slices.Equal(got, []agent.PromotionEvent{{Name: "gone"}}) {
		t.Errorf("events = %v", got)
	}
}

// catalogTools is a catalog with toolkits, for the deferred tool_search.
func catalogTools() []ninectx.ToolWithVector {
	tool := func(name, kit, desc string) ninectx.ToolWithVector {
		return ninectx.ToolWithVector{
			Tool:    llm.ToolDef{Name: name, Description: desc, InputSchema: json.RawMessage(`{"type":"object"}`)},
			Toolkit: kit,
		}
	}
	return []ninectx.ToolWithVector{
		tool("read_file", "workspace", "Read a file from the workspace."),
		tool("trash_list", "workspace", "List the files in the trash."),
		tool("restore_file", "workspace", "Restore a deleted file from the trash to where it was."),
		tool("job_wait", "jobs", "Wait for a background job to finish."),
		tool("tool_search", "catalog", "Load tools."),
	}
}

type searchOut struct {
	Results []struct {
		Name        string          `json:"name"`
		InputSchema json.RawMessage `json:"input_schema"`
	} `json:"results"`
	NotFound []string `json:"not_found"`
}

func deferredSearch(t *testing.T, args string) (searchOut, *agent.Promotions) {
	t.Helper()
	d := agent.New()
	p := agent.NewPromotions()
	// No embedder: the deferred tool_search is the load path and must not need one.
	agent.RegisterDeferredToolSearch(d, nil, catalogTools, p)
	res, err := d.Dispatch(context.Background(), "tool_search", json.RawMessage(args))
	if err != nil {
		t.Fatalf("tool_search %s: %v", args, err)
	}
	var out searchOut
	if err := json.Unmarshal([]byte(res.Output), &out); err != nil {
		t.Fatalf("decode %q: %v", res.Output, err)
	}
	return out, p
}

func resultNames(out searchOut) []string {
	var names []string
	for _, r := range out.Results {
		names = append(names, r.Name)
	}
	return names
}

func TestDeferredToolSearch_LoadsByNameAndToolkit(t *testing.T) {
	out, p := deferredSearch(t, `{"names":["restore_file","jobs","nope","tool_search"]}`)
	if got := resultNames(out); !slices.Equal(got, []string{"restore_file", "job_wait"}) {
		t.Errorf("results = %v", got)
	}
	if len(out.Results) > 0 && len(out.Results[0].InputSchema) == 0 {
		t.Error("a loaded tool must come with its schema")
	}
	if !slices.Equal(out.NotFound, []string{"nope"}) {
		t.Errorf("not_found = %v", out.NotFound)
	}
	if got := p.Names(); !slices.Equal(got, []string{"restore_file", "job_wait"}) {
		t.Errorf("promoted = %v", got)
	}
}

func TestDeferredToolSearch_QueryRanksWithoutEmbedder(t *testing.T) {
	out, p := deferredSearch(t, `{"query":"get back a deleted file","top_k":1}`)
	if got := resultNames(out); !slices.Equal(got, []string{"restore_file"}) {
		t.Errorf("results = %v, want [restore_file]", got)
	}
	if got := p.Names(); !slices.Equal(got, []string{"restore_file"}) {
		t.Errorf("promoted = %v", got)
	}
}

func TestDeferredToolSearch_NeedsNamesOrQuery(t *testing.T) {
	d := agent.New()
	agent.RegisterDeferredToolSearch(d, nil, catalogTools, agent.NewPromotions())
	if _, err := d.Dispatch(context.Background(), "tool_search", json.RawMessage(`{}`)); err == nil {
		t.Error("an empty tool_search must fail")
	}
}

// bigCatalog is larger than ninectx.SmallCatalogTokens, so a deferred loop
// indexes the tools it does not keep resident.
func bigCatalog() []ninectx.ToolWithVector {
	pad := strings.Repeat("x", 4000)
	tool := func(name, kit, schema string) ninectx.ToolWithVector {
		return ninectx.ToolWithVector{Tool: llm.ToolDef{Name: name, Description: name + " " + pad, InputSchema: json.RawMessage(schema)}, Toolkit: kit}
	}
	return []ninectx.ToolWithVector{
		tool("echo", "misc", `{"type":"object"}`),
		tool("restore_file", "workspace", `{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}`),
		tool("job_wait", "jobs", `{"type":"object"}`),
	}
}

// recordingProvider answers from responses in order and records the tool names
// each request advertised.
type recordingProvider struct {
	mu        sync.Mutex
	responses []llm.Response
	requests  [][]string
}

func (r *recordingProvider) provider() llm.Provider {
	return llm.ProviderFunc(func(_ context.Context, req llm.Request) (llm.Response, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		var names []string
		for _, t := range req.Tools {
			names = append(names, t.Name)
		}
		r.requests = append(r.requests, names)
		if len(r.responses) == 0 {
			return finalResp("done"), nil
		}
		resp := r.responses[0]
		r.responses = r.responses[1:]
		return resp, nil
	})
}

func deferredLoop(rec *recordingProvider, d *agent.Dispatcher, p *agent.Promotions, budget int) *agent.Loop {
	return agent.NewLoop(agent.Config{
		SystemCore:     "You are a test agent.",
		Priority:       llm.PriorityConversation,
		Tools:          bigCatalog(),
		Promotions:     p,
		PromotedBudget: budget,
	}, ninectx.New(ninectx.Config{Budget: 100_000, Exposition: ninectx.ExpositionDeferred, Resident: []string{"echo"}}),
		llm.NewQueue(rec.provider(), 1), d)
}

// A call to an indexed tool that fails returns the tool's schema and promotes
// it, so the next request advertises it (call-to-load).
func TestLoop_CallToLoad(t *testing.T) {
	rec := &recordingProvider{responses: []llm.Response{
		toolCallResp("c1", "restore_file", json.RawMessage(`{"file":"plan.md"}`)),
		toolCallResp("c2", "restore_file", json.RawMessage(`{"path":"plan.md"}`)),
	}}
	d := agent.New()
	d.InjectHandler("restore_file", func(_ context.Context, args json.RawMessage) (string, error) {
		if !strings.Contains(string(args), `"path"`) {
			return "", errors.New("path is required")
		}
		return "restored", nil
	})
	p := agent.NewPromotions()
	loop := deferredLoop(rec, d, p, 0)
	var promoted []string
	var observations []string
	loop.SetHooks(agent.Hooks{
		OnToolPromoted: func(name, reason string) { promoted = append(promoted, name+":"+reason) },
		OnToolEnd: func(_, _, _ string, _ json.RawMessage, out agent.ToolOutcome) {
			observations = append(observations, out.Output)
		},
	})
	if _, err := loop.Run(context.Background(), "restore plan.md"); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(rec.requests[0], "restore_file") {
		t.Errorf("first request advertised the indexed tool: %v", rec.requests[0])
	}
	if !slices.Contains(rec.requests[1], "restore_file") {
		t.Errorf("second request did not advertise the loaded tool: %v", rec.requests[1])
	}
	if !strings.Contains(observations[0], `"required":["path"]`) {
		t.Errorf("the failed call's observation lacks the schema: %q", observations[0])
	}
	if !slices.Equal(promoted, []string{"restore_file:call"}) {
		t.Errorf("promotions = %v", promoted)
	}
}

// A call to a toolkit's name, which the index prints as "jobs: job_wait",
// loads the toolkit's tools instead of only failing.
func TestLoop_ToolkitCallLoadsTheToolkit(t *testing.T) {
	rec := &recordingProvider{responses: []llm.Response{
		toolCallResp("c1", "jobs", json.RawMessage(`{}`)),
	}}
	p := agent.NewPromotions()
	loop := deferredLoop(rec, agent.New(), p, 0)
	var observations []string
	loop.SetHooks(agent.Hooks{OnToolEnd: func(_, _, _ string, _ json.RawMessage, out agent.ToolOutcome) {
		observations = append(observations, out.Output)
	}})
	if _, err := loop.Run(context.Background(), "wait for the job"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(rec.requests[1], "job_wait") {
		t.Errorf("the toolkit's tool was not loaded: %v", rec.requests[1])
	}
	if !strings.Contains(observations[0], "job_wait") {
		t.Errorf("observation does not name the loaded tools: %q", observations[0])
	}
}

// A call to "<toolkit>:<tool>" or "<toolkit>/<tool>", the index line read as
// one name, runs the tool.
func TestLoop_QualifiedNameRunsTheTool(t *testing.T) {
	for _, name := range []string{"workspace:restore_file", "mcp/workspace/restore_file"} {
		rec := &recordingProvider{responses: []llm.Response{
			toolCallResp("c1", name, json.RawMessage(`{"path":"plan.md"}`)),
		}}
		d := agent.New()
		ran := false
		d.InjectHandler("restore_file", func(context.Context, json.RawMessage) (string, error) {
			ran = true
			return "restored", nil
		})
		if _, err := deferredLoop(rec, d, agent.NewPromotions(), 0).Run(context.Background(), "restore plan.md"); err != nil {
			t.Fatal(err)
		}
		if !ran {
			t.Errorf("%s did not run restore_file", name)
		}
	}
}

// A promoted tool stays across turns; past the budget, the least recently
// used is demoted before the next turn's first request, never mid-turn.
func TestLoop_PromotionsStickAndDemoteBetweenTurns(t *testing.T) {
	rec := &recordingProvider{}
	p := agent.NewPromotions()
	loop := deferredLoop(rec, agent.New(), p, 1500)
	p.Promote([]string{"restore_file"}, agent.PromoteSearch)

	if _, err := loop.Run(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(rec.requests[0], "restore_file") {
		t.Fatalf("a promoted tool was not advertised: %v", rec.requests[0])
	}

	// Two promoted tools of ~1,170 tokens each exceed 1,500: the one used
	// least recently goes before the next turn.
	p.Promote([]string{"job_wait"}, agent.PromoteSearch)
	var demoted []string
	loop.SetHooks(agent.Hooks{OnToolDemoted: func(name string) { demoted = append(demoted, name) }})
	if _, err := loop.Run(context.Background(), "two"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(demoted, []string{"restore_file"}) {
		t.Errorf("demoted = %v, want [restore_file]", demoted)
	}
	last := rec.requests[len(rec.requests)-1]
	if slices.Contains(last, "restore_file") || !slices.Contains(last, "job_wait") {
		t.Errorf("second turn advertised %v", last)
	}
	if got := loop.LastToolIndex(); !slices.Contains(got, "restore_file") {
		t.Errorf("a demoted tool must return to the index: %v", got)
	}
}
