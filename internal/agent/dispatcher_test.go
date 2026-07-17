package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"nine/internal/agent"
	"nine/internal/embed"
	"nine/internal/memory"
	"nine/internal/memory/memtest"
	"nine/internal/plugin"
)

// newWithHandler returns a Dispatcher with a custom handler injected for testing.
func newWithHandler(toolName string, fn func(context.Context, json.RawMessage) (string, error)) *agent.Dispatcher {
	d := agent.New()
	d.InjectHandler(toolName, fn)
	return d
}

func TestDispatchValidTool(t *testing.T) {
	d := newWithHandler("echo", func(_ context.Context, args json.RawMessage) (string, error) {
		return "echoed: " + string(args), nil
	})

	args := json.RawMessage(`{"msg":"hello"}`)
	res, err := d.Dispatch(context.Background(), "echo", args)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.Output != `echoed: {"msg":"hello"}` {
		t.Errorf("output = %q", res.Output)
	}
	if res.Truncated {
		t.Error("should not be truncated")
	}
}

func TestDispatchUnknownTool(t *testing.T) {
	d := agent.New()
	_, err := d.Dispatch(context.Background(), "no_such_tool", json.RawMessage(`{}`))
	if err == nil {
		t.Error("expected error for unknown tool, got nil")
	}
	if !strings.Contains(err.Error(), "no_such_tool") {
		t.Errorf("error = %v, want it to mention the tool name", err)
	}
}


func TestDispatchOutputTruncation(t *testing.T) {
	// Output larger than maxOutputTokens (2048 tokens ≈ 8192 chars).
	bigOutput := strings.Repeat("a", 10000)
	d := newWithHandler("big", func(_ context.Context, _ json.RawMessage) (string, error) {
		return bigOutput, nil
	})

	res, err := d.Dispatch(context.Background(), "big", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !res.Truncated {
		t.Error("expected Truncated=true")
	}
	if len(res.Output) >= len(bigOutput) {
		t.Errorf("output not shortened: len=%d", len(res.Output))
	}
	if !strings.Contains(res.Output, "[output truncated]") {
		t.Error("truncation marker missing")
	}
}

func TestDispatchOutputNotTruncated(t *testing.T) {
	// Exactly at the limit should not be truncated.
	atLimit := strings.Repeat("a", 2048*4)
	d := newWithHandler("tool", func(_ context.Context, _ json.RawMessage) (string, error) {
		return atLimit, nil
	})
	res, err := d.Dispatch(context.Background(), "tool", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Truncated {
		t.Error("output exactly at limit should not be truncated")
	}
}

func TestDispatchHookFires(t *testing.T) {
	d := newWithHandler("tool", func(_ context.Context, _ json.RawMessage) (string, error) {
		return "result", nil
	})

	var gotName, gotOutput string
	var gotArgs json.RawMessage
	d.AddHook("tool", func(name string, args json.RawMessage, output string) {
		gotName = name
		gotArgs = args
		gotOutput = output
	})

	args := json.RawMessage(`{"x":1}`)
	d.Dispatch(context.Background(), "tool", args) //nolint:errcheck

	if gotName != "tool" {
		t.Errorf("hook name = %q, want tool", gotName)
	}
	if string(gotArgs) != `{"x":1}` {
		t.Errorf("hook args = %q", gotArgs)
	}
	if gotOutput != "result" {
		t.Errorf("hook output = %q", gotOutput)
	}
}

func TestDispatchHookNotFiredOnError(t *testing.T) {
	d := newWithHandler("fail", func(_ context.Context, _ json.RawMessage) (string, error) {
		return "", context.DeadlineExceeded
	})

	called := false
	d.AddHook("fail", func(string, json.RawMessage, string) { called = true })
	d.Dispatch(context.Background(), "fail", json.RawMessage(`{}`)) //nolint:errcheck

	if called {
		t.Error("hook should not fire when tool returns an error")
	}
}

// dispatcherModuleRoot walks up from cwd to find go.mod.
func dispatcherModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

func TestRegisterPlugin(t *testing.T) {
	root := dispatcherModuleRoot(t)
	binPath := filepath.Join(t.TempDir(), "testplugin")
	cmd := exec.Command("go", "build", "-mod=vendor", "-o", binPath, "./internal/plugin/testplugin")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build testplugin: %v\n%s", err, out)
	}

	mgr := plugin.NewManager("")
	p, err := mgr.Start(binPath)
	if err != nil {
		t.Fatalf("start testplugin: %v", err)
	}
	t.Cleanup(func() { mgr.Stop(p) })

	d := agent.New()
	d.RegisterPlugin(mgr, p)

	args := json.RawMessage(`{"message":"hello"}`)
	res, err := d.Dispatch(context.Background(), "echo", args)
	if err != nil {
		t.Fatalf("Dispatch echo: %v", err)
	}
	// testplugin echoes args back as output
	if res.Output != `{"message":"hello"}` {
		t.Errorf("output = %q, want %q", res.Output, `{"message":"hello"}`)
	}
}

func newDispatcherTestStore(t *testing.T) *memory.Store {
	t.Helper()
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestWireFileSearchSemantic(t *testing.T) {
	store := newDispatcherTestStore(t)
	// Pre-seed a vector so the query can return a result.
	if err := store.VectorStore("doc:doc.md", "files", "doc.md", []float32{1, 0}); err != nil {
		t.Fatalf("seed vector: %v", err)
	}

	mockEmbedder := embed.EmbedderFunc(func(_ context.Context, _ string) ([]float32, error) {
		return []float32{1, 0}, nil
	})

	d := agent.New()
	agent.RegisterMemoryTools(d, store, mockEmbedder, nil)

	res, err := d.Dispatch(context.Background(), "file_search_semantic",
		json.RawMessage(`{"query":"test","top_k":1}`))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !strings.Contains(res.Output, "doc.md") {
		t.Errorf("output = %q, want it to contain 'doc.md'", res.Output)
	}
}

func TestWireFileSearchSemanticDefaultTopK(t *testing.T) {
	store := newDispatcherTestStore(t)
	mockEmbedder := embed.EmbedderFunc(func(_ context.Context, _ string) ([]float32, error) {
		return []float32{1, 0}, nil
	})

	d := agent.New()
	agent.RegisterMemoryTools(d, store, mockEmbedder, nil)

	// Omit top_k — should default to 5. With empty store returns no error.
	_, err := d.Dispatch(context.Background(), "file_search_semantic",
		json.RawMessage(`{"query":"test"}`))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
}

// ---- run_agent / run_agents ----


func TestWireRunAgentSuccess(t *testing.T) {
	var gotTask, gotExtraCtx string
	d := agent.New()
	agent.RegisterRunAgent(d, func(_ context.Context, task, extraCtx, _ string) (string, error) {
		gotTask = task
		gotExtraCtx = extraCtx
		return "sub-agent result", nil
	})

	res, err := d.Dispatch(context.Background(), "run_agent",
		json.RawMessage(`{"task":"do the thing","context":"some context"}`))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if res.Output != "sub-agent result" {
		t.Errorf("output = %q, want 'sub-agent result'", res.Output)
	}
	if gotTask != "do the thing" {
		t.Errorf("task = %q, want 'do the thing'", gotTask)
	}
	if gotExtraCtx != "some context" {
		t.Errorf("extraCtx = %q, want 'some context'", gotExtraCtx)
	}
}

func TestWireRunAgentNoContext(t *testing.T) {
	d := agent.New()
	var gotExtraCtx string
	agent.RegisterRunAgent(d, func(_ context.Context, _, extraCtx, _ string) (string, error) {
		gotExtraCtx = extraCtx
		return "done", nil
	})

	_, err := d.Dispatch(context.Background(), "run_agent",
		json.RawMessage(`{"task":"simple task"}`))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if gotExtraCtx != "" {
		t.Errorf("extraCtx = %q, want empty when context field is absent", gotExtraCtx)
	}
}

func TestWireRunAgentError(t *testing.T) {
	d := agent.New()
	agent.RegisterRunAgent(d, func(_ context.Context, _, _, _ string) (string, error) {
		return "", fmt.Errorf("sub-agent failed")
	})

	_, err := d.Dispatch(context.Background(), "run_agent",
		json.RawMessage(`{"task":"failing task"}`))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "sub-agent failed") {
		t.Errorf("error = %v, want to contain 'sub-agent failed'", err)
	}
}

func TestWireRunAgentInvalidJSON(t *testing.T) {
	d := agent.New()
	agent.RegisterRunAgent(d, func(_ context.Context, _, _, _ string) (string, error) {
		return "should not reach", nil
	})

	_, err := d.Dispatch(context.Background(), "run_agent", json.RawMessage(`{bad json}`))
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestWireRunAgentContextPropagated(t *testing.T) {
	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "marker")

	d := agent.New()
	var gotCtx context.Context
	agent.RegisterRunAgent(d, func(c context.Context, _, _, _ string) (string, error) {
		gotCtx = c
		return "ok", nil
	})

	d.Dispatch(ctx, "run_agent", json.RawMessage(`{"task":"check ctx"}`)) //nolint:errcheck
	if gotCtx == nil || gotCtx.Value(ctxKey{}) != "marker" {
		t.Error("context not propagated to run_agent handler")
	}
}

func TestWireRunAgentsSuccess(t *testing.T) {
	d := agent.New()
	agent.RegisterRunAgents(d, func(_ context.Context, tasks []agent.SubAgentTask, _ int) []agent.SubAgentResult {
		results := make([]agent.SubAgentResult, len(tasks))
		for i, tk := range tasks {
			results[i] = agent.SubAgentResult{Task: tk.Task, Result: "result for " + tk.Task}
		}
		return results
	})

	res, err := d.Dispatch(context.Background(), "run_agents",
		json.RawMessage(`{"tasks":[{"task":"task A"},{"task":"task B"}]}`))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	for _, want := range []string{`"done"`, "task A", "task B", "result for task A"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("output missing %q: %s", want, res.Output)
		}
	}
}

func TestWireRunAgentsTimeout(t *testing.T) {
	d := agent.New()
	agent.RegisterRunAgents(d, func(_ context.Context, tasks []agent.SubAgentTask, _ int) []agent.SubAgentResult {
		return []agent.SubAgentResult{
			{Task: tasks[0].Task, Err: context.DeadlineExceeded},
		}
	})

	res, err := d.Dispatch(context.Background(), "run_agents",
		json.RawMessage(`{"tasks":[{"task":"slow task"}],"timeout_seconds":1}`))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !strings.Contains(res.Output, `"timed_out"`) {
		t.Errorf("output missing 'timed_out': %s", res.Output)
	}
	// timed_out entries must not have an "error" field
	if strings.Contains(res.Output, `"error"`) {
		t.Errorf("timed_out result should not have 'error' field: %s", res.Output)
	}
}

func TestWireRunAgentsMixedResults(t *testing.T) {
	d := agent.New()
	agent.RegisterRunAgents(d, func(_ context.Context, tasks []agent.SubAgentTask, _ int) []agent.SubAgentResult {
		return []agent.SubAgentResult{
			{Task: tasks[0].Task, Result: "ok"},
			{Task: tasks[1].Task, Err: fmt.Errorf("boom")},
			{Task: tasks[2].Task, Err: context.DeadlineExceeded},
		}
	})

	res, err := d.Dispatch(context.Background(), "run_agents",
		json.RawMessage(`{"tasks":[{"task":"A"},{"task":"B"},{"task":"C"}]}`))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	for _, want := range []string{`"done"`, `"failed"`, `"timed_out"`, "boom"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("output missing %q: %s", want, res.Output)
		}
	}
}

func TestWireRunAgentsInvalidJSON(t *testing.T) {
	d := agent.New()
	agent.RegisterRunAgents(d, func(_ context.Context, _ []agent.SubAgentTask, _ int) []agent.SubAgentResult {
		return nil
	})

	_, err := d.Dispatch(context.Background(), "run_agents", json.RawMessage(`{bad json}`))
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestWireRunAgentsContextPropagated(t *testing.T) {
	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "marker")

	d := agent.New()
	var gotCtx context.Context
	agent.RegisterRunAgents(d, func(c context.Context, _ []agent.SubAgentTask, _ int) []agent.SubAgentResult {
		gotCtx = c
		return nil
	})

	d.Dispatch(ctx, "run_agents", json.RawMessage(`{"tasks":[]}`)) //nolint:errcheck
	if gotCtx == nil || gotCtx.Value(ctxKey{}) != "marker" {
		t.Error("context not propagated to run_agents handler")
	}
}
