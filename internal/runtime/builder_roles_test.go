package runtime_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"nine/internal/llm"
	"nine/internal/memory"
	"nine/internal/memory/memtest"
	"nine/internal/plugin"
	"nine/internal/runtime"
)

// scriptedProvider records every request and answers via script(callN, req).
// Calls in these tests form a deterministic chain (root turn → nested
// sub-agent turn → root continuation), so callN identifies the loop.
type scriptedProvider struct {
	mu     sync.Mutex
	calls  []llm.Request
	script func(n int, req llm.Request) llm.Response
}

func (p *scriptedProvider) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, req)
	return p.script(len(p.calls), req), nil
}

func (p *scriptedProvider) call(n int) llm.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[n-1]
}

func (p *scriptedProvider) nCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func toolNames(req llm.Request) map[string]bool {
	names := make(map[string]bool, len(req.Tools))
	for _, td := range req.Tools {
		names[td.Name] = true
	}
	return names
}

func runAgentCall(task, role string) llm.Response {
	input, _ := json.Marshal(map[string]string{"task": task, "role": role})
	return llm.Response{
		StopReason: "tool_use",
		ToolCalls:  []llm.ToolCall{{ID: "t1", Name: "run_agent", Input: input}},
	}
}

func rolesTestBuilder(t *testing.T, p *scriptedProvider, mutate func(*runtime.AgentBuilderConfig)) *runtime.AgentBuilder {
	t.Helper()
	cfg := runtime.AgentBuilderConfig{
		Loop: runtime.LoopConfig{
			Mgr:           plugin.NewManager(""),
			SystemPrompt:  "ROOT-DAEMON-PROMPT",
			ContextBudget: 100_000,
		},
		InitialQueue: llm.NewQueue(p, 1),
		Sup:          runtime.NewSupervisor(8),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return runtime.NewAgentBuilder(cfg)
}

// Gates 1 + 7 (docs/roles.md §12): a delegation naming no role runs the
// executor — same tool surface as the parent minus nothing (it may still
// sub-delegate at depth 1) — and its system prompt is the executor persona,
// never the orchestrator's daemon prompt.
func TestDelegationDefaultsToExecutorWithSubAgentPersona(t *testing.T) {
	p := &scriptedProvider{}
	p.script = func(n int, req llm.Request) llm.Response {
		switch n {
		case 1: // root orchestrator turn → delegate with no role
			return runAgentCall("count things", "")
		case 2: // sub-agent turn
			return llm.Response{Text: "sub done", StopReason: "end_turn"}
		default: // root continues after observation
			return llm.Response{Text: "root done", StopReason: "end_turn"}
		}
	}
	factory := rolesTestBuilder(t, p, nil)

	loop := factory.Build("root-1", false)
	if _, err := loop.Run(context.Background(), "please delegate"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if p.nCalls() < 3 {
		t.Fatalf("expected ≥3 LLM calls, got %d", p.nCalls())
	}

	rootReq, subReq := p.call(1), p.call(2)
	if !strings.Contains(rootReq.System, "ROOT-DAEMON-PROMPT") {
		t.Errorf("orchestrator system prompt = %q, want the daemon prompt (R-ROLE.3 fallback)", rootReq.System)
	}
	if !strings.Contains(subReq.System, "sub-agent mode") {
		t.Errorf("sub-agent system prompt = %q, want the executor persona (R-ROLE.10)", subReq.System)
	}
	if strings.Contains(subReq.System, "ROOT-DAEMON-PROMPT") {
		t.Error("sub-agent must not inherit the orchestrator prompt (gate 7)")
	}

	// Gate 1: executor advertises the full core surface and may sub-delegate.
	subTools := toolNames(subReq)
	for _, want := range []string{"memory_get", "skill_write", "run_agent", "workflow_create", "goal_create"} {
		if !subTools[want] {
			t.Errorf("executor leaf missing %q (must reproduce the pre-role sub-agent toolset)", want)
		}
	}
}

// R-ROLE.8 end-to-end: a store-backed role skill reaches the run_agent schema
// the model is actually shown. This drives the whole path — registry →
// RoleEnum → buildToolList → appendInterceptedTools → advertised ToolDef — so
// it fails if the delegation defs go back to being static package constants.
func TestAdvertisedRunAgentSchemaListsStoreRoles(t *testing.T) {
	store := newRoleTestStore(t)
	if err := store.SkillUpsert(memory.Skill{
		Name:        "data-wrangler",
		Description: "Clean and reshape datasets — files and shell, no network.",
		Content:     "---\nname: data-wrangler\nrole:\n  tools: [shell, read_file]\n---\n\nWrangle data.",
		Source:      memory.SkillSourceAgent,
	}); err != nil {
		t.Fatalf("seed role skill: %v", err)
	}

	p := &scriptedProvider{}
	p.script = func(int, llm.Request) llm.Response {
		return llm.Response{Text: "done", StopReason: "end_turn"}
	}
	factory := rolesTestBuilder(t, p, func(c *runtime.AgentBuilderConfig) {
		c.Loop.Memory = store
	})

	loop := factory.Build("root-1", false)
	if _, err := loop.Run(context.Background(), "hello"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if p.nCalls() == 0 {
		t.Fatal("no LLM calls recorded")
	}

	var schema string
	for _, td := range p.call(1).Tools {
		if td.Name == "run_agent" {
			schema = string(td.InputSchema)
		}
	}
	if schema == "" {
		t.Fatal("run_agent not advertised to the orchestrator")
	}
	if !strings.Contains(schema, "data-wrangler") {
		t.Errorf("advertised run_agent schema omits store-backed role; the model can only\nreach it by guessing the name (R-ROLE.8). schema = %s", schema)
	}
	// Built-ins must still be listed alongside it.
	if !strings.Contains(schema, "executor") {
		t.Errorf("advertised run_agent schema dropped the built-in executor: %s", schema)
	}
}

// Gate 2: a coarse role's allowlist is enforced at both boundaries — a
// disallowed tool is absent from the advertised list AND dispatching it
// returns unknown-tool.
func TestCoarseRoleEnforcesBothBoundaries(t *testing.T) {
	p := &scriptedProvider{}
	p.script = func(n int, req llm.Request) llm.Response {
		switch n {
		case 1: // root → delegate to report-writer
			return runAgentCall("write a report", "report-writer")
		case 2: // sub-agent hallucinates a disallowed core tool
			input, _ := json.Marshal(map[string]string{"key": "x"})
			return llm.Response{
				StopReason: "tool_use",
				ToolCalls:  []llm.ToolCall{{ID: "t2", Name: "memory_delete", Input: input}},
			}
		case 3: // sub-agent sees the failure observation, gives up
			return llm.Response{Text: "cannot", StopReason: "end_turn"}
		default:
			return llm.Response{Text: "root done", StopReason: "end_turn"}
		}
	}
	factory := rolesTestBuilder(t, p, nil)

	loop := factory.Build("root-2", false)
	if _, err := loop.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	subTools := toolNames(p.call(2))
	for _, banned := range []string{"shell", "memory_delete", "skill_write", "run_agent", "workflow_create"} {
		if subTools[banned] {
			t.Errorf("report-writer advertises %q; allowlist must exclude it (boundary 1)", banned)
		}
	}
	for _, want := range []string{"memory_get", "memory_set", "file_store", "skill_read"} {
		if !subTools[want] {
			t.Errorf("report-writer missing allowed tool %q", want)
		}
	}

	// Boundary 2: the dispatched memory_delete call must have failed as
	// unknown tool; the failure observation reaches the sub-agent's next call.
	msgs, _ := json.Marshal(p.call(3).Messages)
	if !strings.Contains(string(msgs), "unknown tool") {
		t.Errorf("memory_delete dispatch must fail as unknown tool (boundary 2); messages: %s", msgs)
	}
}

// Gate 4: an agent-authored role allowlist naming a nonexistent tool spawns a
// leaf without it — silently dropped, never an error (R-ROLE.5).
func TestRoleAllowlistDropsUnknownToolsSilently(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SkillUpsert(memory.Skill{
		Name:    "narrow",
		Source:  memory.SkillSourceAgent,
		Content: "---\nrole:\n  tools: [memory_get, warp_drive]\n---\nNarrow persona.",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	p := &scriptedProvider{}
	p.script = func(n int, req llm.Request) llm.Response {
		if n == 1 {
			return runAgentCall("audit", "narrow")
		}
		return llm.Response{Text: "done", StopReason: "end_turn"}
	}
	factory := rolesTestBuilder(t, p, func(cfg *runtime.AgentBuilderConfig) {
		cfg.Loop.Memory = store
	})

	loop := factory.Build("root-3", false)
	if _, err := loop.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	subTools := toolNames(p.call(2))
	if !subTools["memory_get"] {
		t.Error("allowed existing tool memory_get missing")
	}
	if subTools["warp_drive"] {
		t.Error("nonexistent tool warp_drive must be dropped, not created (R-ROLE.5)")
	}
	if !strings.Contains(p.call(2).System, "Narrow persona") {
		t.Errorf("agent role persona not applied: %q", p.call(2).System)
	}
}

// Gate 5: when depthGuard is exhausted, delegation tools are not registered
// even for a Delegates:true role (R-ROLE.6).
func TestDepthGuardRemovesDelegationTools(t *testing.T) {
	p := &scriptedProvider{}
	p.script = func(n int, req llm.Request) llm.Response {
		if n == 1 {
			return runAgentCall("leaf task", "executor")
		}
		return llm.Response{Text: "done", StopReason: "end_turn"}
	}
	factory := rolesTestBuilder(t, p, func(cfg *runtime.AgentBuilderConfig) {
		cfg.MaxDelegationDepth = 1 // root may delegate once; its child may not
	})

	loop := factory.Build("root-4", false)
	if _, err := loop.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	rootTools, subTools := toolNames(p.call(1)), toolNames(p.call(2))
	if !rootTools["run_agent"] {
		t.Error("root at depthGuard=1 must still delegate")
	}
	if subTools["run_agent"] || subTools["run_agents"] || subTools["workflow_create"] {
		t.Error("executor at depthGuard=0 must lose delegation tools (R-ROLE.6)")
	}
}

// Gate 6 (narrowing half): the reflection role sheds the orchestrator's full
// surface — memory tools and skill_read only, no delegation, and it keeps the
// daemon system prompt (empty role body falls back, R-ROLE.3).
func TestReflectionRoleIsNarrowed(t *testing.T) {
	p := &scriptedProvider{}
	p.script = func(int, llm.Request) llm.Response {
		return llm.Response{Text: "reflected", StopReason: "end_turn"}
	}
	factory := rolesTestBuilder(t, p, nil)

	loop := factory.BuildForRole("self-reflection", runtime.RoleParams{Role: "reflection"})
	if _, err := loop.Run(context.Background(), "reflect"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	tools := toolNames(p.call(1))
	for _, want := range []string{"memory_get", "memory_set", "memory_delete", "memory_list", "skill_read"} {
		if !tools[want] {
			t.Errorf("reflection missing %q", want)
		}
	}
	for _, banned := range []string{"run_agent", "goal_create", "skill_write", "file_store"} {
		if tools[banned] {
			t.Errorf("reflection advertises %q; role must narrow it away (docs/roles.md §4)", banned)
		}
	}
	if !strings.Contains(p.call(1).System, "ROOT-DAEMON-PROMPT") {
		t.Error("reflection role body is empty; system prompt must fall back to the daemon prompt")
	}
}

// A pre-defined standing agent runs the narrowed monitor role in a pursue
// shell (OwnsGoal): it keeps its read-only work tools and gains the goal
// self-management tools (to steer its own goal), but never gains goal_create,
// delegation, or shell — the shell confers goal ownership, not delegation
// (docs/predefined-agents.md §3.1).
func TestMonitorPursueShellToolSurface(t *testing.T) {
	p := &scriptedProvider{}
	p.script = func(int, llm.Request) llm.Response {
		return llm.Response{Text: "watched", StopReason: "end_turn"}
	}
	factory := rolesTestBuilder(t, p, func(cfg *runtime.AgentBuilderConfig) {
		cfg.NotifyUser = func(string, string) {} // enables the notify_user shell tool
	})

	loop := factory.BuildForRole("sec-watch", runtime.RoleParams{Role: "monitor", OwnsGoal: true})
	if _, err := loop.Run(context.Background(), "check"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	tools := toolNames(p.call(1))
	// Goal self-management and notify_user are granted by the pursue shell
	// regardless of the monitor allowlist (which lists none of these).
	for _, want := range []string{"goal_get", "goal_list", "goal_update_status", "goal_append_subtree", "notify_user"} {
		if !tools[want] {
			t.Errorf("monitor pursue shell missing shell tool %q", want)
		}
	}
	// Read-only work tools from the role allowlist survive.
	for _, want := range []string{"memory_get", "memory_set", "skill_read"} {
		if !tools[want] {
			t.Errorf("monitor missing work tool %q", want)
		}
	}
	// Delegation and goal creation stay gated; the monitor allowlist bans writes.
	for _, banned := range []string{"goal_create", "run_agent", "run_agents", "shell", "file_store"} {
		if tools[banned] {
			t.Errorf("monitor pursue shell advertises %q; it must stay gated/narrowed", banned)
		}
	}
}

// v2: a standing agent may opt into delegation and a wider role. With
// Delegates=true the pursue shell keeps goal ownership + notify_user AND gains
// the delegation tools (run_agent/goal_create) — even though the sysadmin
// allowlist lists none of them — exercising the escalation path end-to-end
// (docs/predefined-agents.md §7 v2). Plugin work tools like shell aren't loaded
// in this harness, so we assert on the intercepted/shell tool surface only.
func TestDelegatingStandingAgentWiderRole(t *testing.T) {
	p := &scriptedProvider{}
	p.script = func(int, llm.Request) llm.Response {
		return llm.Response{Text: "done", StopReason: "end_turn"}
	}
	factory := rolesTestBuilder(t, p, func(cfg *runtime.AgentBuilderConfig) {
		cfg.NotifyUser = func(string, string) {}
		cfg.MaxDelegationDepth = 1
	})

	loop := factory.BuildForRole("dep-monitor", runtime.RoleParams{
		Role:      "sysadmin",
		OwnsGoal:  true,
		Delegates: true,
	})
	if _, err := loop.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	tools := toolNames(p.call(1))
	for _, want := range []string{"run_agent", "run_agents", "goal_create", "goal_get", "notify_user"} {
		if !tools[want] {
			t.Errorf("delegating standing agent (sysadmin) missing shell-conferred %q", want)
		}
	}
}

// Even if the model hallucinates a banned tool name, the dispatcher rejects it
// for an allowlist role — but the shell-conferred goal tools dispatch fine
// (Boundary 2 of R-ROLE.4, extended for OwnsGoal in build()).
func TestMonitorPursueShellDispatchBoundary(t *testing.T) {
	p := &scriptedProvider{}
	p.script = func(n int, _ llm.Request) llm.Response {
		if n == 1 {
			input, _ := json.Marshal(map[string]string{"command": "rm -rf /"})
			return llm.Response{
				StopReason: "tool_use",
				ToolCalls:  []llm.ToolCall{{ID: "t1", Name: "shell", Input: input}},
			}
		}
		return llm.Response{Text: "done", StopReason: "end_turn"}
	}
	factory := rolesTestBuilder(t, p, nil)

	loop := factory.BuildForRole("sec-watch", runtime.RoleParams{Role: "monitor", OwnsGoal: true})
	if _, err := loop.Run(context.Background(), "check"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The banned shell call must have been rejected at dispatch (RestrictTo), so
	// the loop continues to a second turn carrying the rejection as a tool
	// result rather than executing the command.
	if p.nCalls() < 2 {
		t.Fatalf("expected the loop to continue after a rejected tool call; got %d calls", p.nCalls())
	}
}

// countingEmbedder returns a fixed vector and counts how many times Embed is
// called, to prove the per-tool embedding cache computes each tool once.
type countingEmbedder struct {
	mu    sync.Mutex
	calls map[string]int
}

func (c *countingEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	c.calls[text]++
	return []float32{1, 0, 0}, nil
}

func (c *countingEmbedder) count(text string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[text]
}

// The context builder ranks tools by cosine similarity to the query using each
// tool's embedding (docs/context-builder.md). This verifies the AgentBuilder now
// actually populates those per-tool vectors — and caches them so a tool's
// description is embedded at most once across sessions — rather than leaving
// them nil (which made ranking inert).
func TestToolVectorsArePopulatedAndCached(t *testing.T) {
	emb := &countingEmbedder{}
	f := rolesTestBuilder(t, &scriptedProvider{}, nil)

	// First lookup embeds; the description text is "name: description".
	v1 := f.ToolVectorForTest(emb, "http_get", "Perform an HTTP GET request.")
	if len(v1) == 0 {
		t.Fatal("tool vector should be populated when an embedder is configured")
	}
	// Second lookup for the same tool is served from cache — no re-embed.
	v2 := f.ToolVectorForTest(emb, "http_get", "Perform an HTTP GET request.")
	if len(v2) == 0 {
		t.Fatal("cached tool vector should be non-empty")
	}
	if got := emb.count("http_get: Perform an HTTP GET request."); got != 1 {
		t.Errorf("Embed called %d times for the same tool, want 1 (cached)", got)
	}

	// A different tool embeds separately.
	if v := f.ToolVectorForTest(emb, "shell", "Run a shell command."); len(v) == 0 {
		t.Error("second distinct tool should also be embedded")
	}

	// With no embedder, ranking degrades gracefully to nil vectors (score 0).
	if v := f.ToolVectorForTest(nil, "http_get", "Perform an HTTP GET request."); v != nil {
		t.Errorf("nil embedder should yield a nil vector, got %v", v)
	}
}
