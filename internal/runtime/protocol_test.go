package runtime_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nine/internal/agent"
	ninectx "nine/internal/context"
	"nine/internal/embed"
	"nine/internal/llm"
	"nine/internal/llm/ollama"
	"nine/internal/memory"
	"nine/internal/memory/memtest"
	"nine/internal/plugin"
	"nine/internal/protocol"
	"nine/internal/runtime"
)

func newWireTestStore(t *testing.T) *memory.Store {
	t.Helper()
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// ---- mock helpers ----

// subAgentTestPrompt is a minimal sub-agent persona for manually-built test
// loops (the shipped persona now lives in the executor role skill).
const subAgentTestPrompt = "You are Nine operating in sub-agent mode. Complete the finite task and confirm."

// mockEmbedder returns a fixed vector for any text.
func mockEmbedder(vec []float32) embed.Embedder {
	return embed.EmbedderFunc(func(_ context.Context, _ string) ([]float32, error) {
		cp := make([]float32, len(vec))
		copy(cp, vec)
		return cp, nil
	})
}

// ---- stall detection ----

func TestStallDetection(t *testing.T) {
	const stallLimit = 3

	sup := runtime.NewSupervisor(16)
	supStore := newFakeEventStore()
	sup.Attach(supStore)
	supCtx, supCancel := context.WithCancel(context.Background())
	defer supCancel()
	go sup.Run(supCtx)

	// LLM always answers without using any tools — triggers stall counter.
	provider := llm.ProviderFunc(func(_ context.Context, _ llm.Request) (llm.Response, error) {
		return llm.Response{Text: "just talking", StopReason: "end_turn"}, nil
	})

	stallCh := make(chan string, 4)

	factory := func(id string, _ runtime.RoleParams) *agent.Loop {
		queue := llm.NewQueue(provider, 1)
		builder := ninectx.New(ninectx.Config{Budget: 100_000})
		return agent.NewLoop(agent.Config{
			SystemCore: "test",
			Priority:   llm.PriorityConversation,
		}, builder, queue, agent.New())
	}

	sock := tmpSock(t)
	d := runtime.New(sock, runtime.InternalAgent{
		Build: factory,
		Stall: runtime.StallConfig{
			Limit: stallLimit,
			OnStall: func(agentID string) {
				sup.Post(runtime.Event{Kind: runtime.EventGoalStalls, AgentID: agentID})
				stallCh <- agentID
			},
		},
	}, nil)

	dctx, dcancel := context.WithCancel(context.Background())
	defer dcancel()
	go d.Start(dctx) //nolint:errcheck
	waitForSock(t, sock)
	t.Cleanup(d.Stop)

	c, err := protocol.Connect(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint:errcheck

	id, err := c.NewConversation()
	if err != nil {
		t.Fatal(err)
	}

	// Send stallLimit turns with no tool calls; the stall should fire after
	// the Nth turn.
	for i := 0; i < stallLimit; i++ {
		if _, err := c.Turn(id, "say something"); err != nil {
			t.Fatalf("turn %d: %v", i+1, err)
		}
	}

	select {
	case got := <-stallCh:
		if got != id {
			t.Errorf("stall agentID = %q, want %q", got, id)
		}
		evts := supStore.eventsOfKind(runtime.EventGoalStalls)
		if len(evts) == 0 {
			t.Error("supervisor received no EventGoalStalls")
		}
	case <-time.After(2 * time.Second):
		t.Error("timeout: stall event never fired")
	}
}

// ---- gap_report ----

func TestGapReport(t *testing.T) {
	sup := runtime.NewSupervisor(16)
	supStore := newFakeEventStore()
	sup.Attach(supStore)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)

	var postedDesc string
	provider := llm.ProviderFunc(func(_ context.Context, _ llm.Request) (llm.Response, error) {
		// LLM calls gap_report on turn 1.
		return llm.Response{
			ToolCalls: []llm.ToolCall{{
				ID:    "tc1",
				Name:  "gap_report",
				Input: json.RawMessage(`{"description":"no file rename tool exists"}`),
			}},
			StopReason: "tool_use",
		}, nil
	})
	// Second call: final answer.
	var called atomic.Int32
	realProvider := llm.ProviderFunc(func(_ context.Context, req llm.Request) (llm.Response, error) {
		n := called.Add(1)
		if n == 1 {
			return llm.Response{
				ToolCalls: []llm.ToolCall{{
					ID:    "tc1",
					Name:  "gap_report",
					Input: json.RawMessage(`{"description":"no file rename tool exists"}`),
				}},
				StopReason: "tool_use",
			}, nil
		}
		return llm.Response{Text: "reported", StopReason: "end_turn"}, nil
	})
	_ = provider

	factory := func(id string, _ runtime.RoleParams) *agent.Loop {
		queue := llm.NewQueue(realProvider, 1)
		builder := ninectx.New(ninectx.Config{Budget: 100_000})
		d := agent.New()
		agent.RegisterGapReport(d, func(desc string) {
			postedDesc = desc
			sup.Post(runtime.Event{
				Kind:    runtime.EventGapReported,
				AgentID: id,
				Payload: desc,
			})
		})
		return agent.NewLoop(agent.Config{
			SystemCore: "test",
			Priority:   llm.PriorityConversation,
		}, builder, queue, d)
	}

	sock := tmpSock(t)
	dm := runtime.New(sock, runtime.InternalAgent{Build: factory}, nil)
	dctx, dcancel := context.WithCancel(context.Background())
	defer dcancel()
	go dm.Start(dctx) //nolint:errcheck
	waitForSock(t, sock)
	t.Cleanup(dm.Stop)

	c, _ := protocol.Connect(sock)
	defer c.Close() //nolint:errcheck

	id, _ := c.NewConversation()
	c.Turn(id, "try to rename a file") //nolint:errcheck

	// Verify supervisor received the event.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		evts := supStore.eventsOfKind(runtime.EventGapReported)
		if len(evts) > 0 {
			if evts[0].Payload != "no file rename tool exists" {
				t.Errorf("payload = %q, want 'no file rename tool exists'", evts[0].Payload)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if postedDesc != "no file rename tool exists" {
		t.Errorf("gap description = %q", postedDesc)
	}
}

// ---- memory_embed / memory_query (unit) ----

func TestMemoryEmbedWire(t *testing.T) {
	store := newWireTestStore(t)
	fixedVec := []float32{0.1, 0.2, 0.3}
	embedder := mockEmbedder(fixedVec)

	d := agent.New()
	agent.RegisterMemoryTools(d, store, embedder, nil, false)

	args := json.RawMessage(`{"id":"test-1","namespace":"notes","key":"note1","text":"hello world"}`)
	res, err := d.Dispatch(context.Background(), "memory_embed", args)
	if err != nil {
		t.Fatalf("memory_embed: %v", err)
	}
	if !strings.Contains(res.Output, "3-dim") {
		t.Errorf("output = %q, want to mention vector dim", res.Output)
	}
	// Verify the vector was actually stored.
	results, err := store.VectorQuery("notes", fixedVec, 1)
	if err != nil {
		t.Fatalf("vector query: %v", err)
	}
	if len(results) == 0 || results[0].Key != "note1" {
		t.Errorf("stored key = %v, want 'note1'", results)
	}
}

func TestMemoryQueryWire(t *testing.T) {
	store := newWireTestStore(t)
	fixedVec := []float32{0.1, 0.2, 0.3}
	embedder := mockEmbedder(fixedVec)

	// Pre-seed a vector so the query returns a result.
	if err := store.VectorStore("n:note1", "notes", "note1", fixedVec); err != nil {
		t.Fatalf("seed: %v", err)
	}

	d := agent.New()
	agent.RegisterMemoryTools(d, store, embedder, nil, false)

	args := json.RawMessage(`{"namespace":"notes","query":"hello","top_k":3}`)
	res, err := d.Dispatch(context.Background(), "memory_query", args)
	if err != nil {
		t.Fatalf("memory_query: %v", err)
	}
	if !strings.Contains(res.Output, "note1") {
		t.Errorf("output = %q, want to mention note1", res.Output)
	}
}

// ---- plugin_start (unit) ----

// ---- skill tools ----

func TestSkillHook(t *testing.T) {
	store := newWireTestStore(t)
	vec := []float32{1, 0, 0}
	embedder := mockEmbedder(vec)

	d := agent.New()
	agent.RegisterSkillTools(d, store, embedder)

	args := json.RawMessage(`{"name":"my-skill","description":"does something useful","content":"steps"}`)
	if _, err := d.Dispatch(context.Background(), "skill_write", args); err != nil {
		t.Fatal(err)
	}
	results, err := store.VectorQuery("skills", vec, 1)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(results) == 0 || results[0].Key != "my-skill" {
		t.Errorf("stored key = %v, want 'my-skill'", results)
	}
}

// ---- sub-agent runner ----

func TestSpawnSubAgent(t *testing.T) {
	provider := llm.ProviderFunc(func(_ context.Context, _ llm.Request) (llm.Response, error) {
		return llm.Response{Text: "task done successfully", StopReason: "end_turn"}, nil
	})
	factory := func(_ string, _ string, _ bool) *agent.Loop {
		return agent.NewLoop(agent.Config{
			SystemCore: subAgentTestPrompt,
			Priority:   llm.PriorityBackground,
		}, ninectx.New(ninectx.Config{Budget: 100_000}), llm.NewQueue(provider, 1), agent.New())
	}

	notif := runtime.NewInMemoryNotifStore()
	const spawnerID = "conv-123"

	ctx := context.Background()
	runtime.SpawnSubAgent(ctx, "task-1", "count to 10", factory("task-1", "executor", false), notif, spawnerID)

	// Wait for the notification to arrive.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ns, _ := notif.Fetch(spawnerID)
		if len(ns) > 0 {
			if !strings.Contains(ns[0], "task done") {
				t.Errorf("notification = %q, want to contain 'task done'", ns[0])
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("timeout: no notification received from task")
}

// ---- integration tests ----

// TestMemoryEmbedQueryIntegration wires the real memory tools to a deterministic
// embedder, so it needs no model and runs everywhere.
func TestMemoryEmbedQueryIntegration(t *testing.T) {
	store := newWireTestStore(t)

	// Use a deterministic fake embedder (no Ollama needed).
	var embedCallCount int
	deterministicEmbed := embed.EmbedderFunc(func(_ context.Context, _ string) ([]float32, error) {
		embedCallCount++
		// Different texts get slightly different vectors so query works.
		base := []float32{float32(embedCallCount) * 0.1, 1.0 - float32(embedCallCount)*0.1, 0.5}
		return base, nil
	})

	d := agent.New()
	agent.RegisterMemoryTools(d, store, deterministicEmbed, nil, false)

	// Embed a sentence.
	embedArgs := json.RawMessage(`{"id":"integ-1","namespace":"test","key":"sentence1","text":"the quick brown fox"}`)
	res, err := d.Dispatch(context.Background(), "memory_embed", embedArgs)
	if err != nil {
		t.Fatalf("memory_embed: %v", err)
	}
	if !strings.Contains(res.Output, "dim") {
		t.Errorf("embed output = %q", res.Output)
	}

	// Query for it — deterministic embedder will return a new vector, but the
	// store has only one entry so it should be returned as the top result.
	queryArgs := json.RawMessage(`{"namespace":"test","query":"fox running fast","top_k":1}`)
	qres, err := d.Dispatch(context.Background(), "memory_query", queryArgs)
	if err != nil {
		t.Fatalf("memory_query: %v", err)
	}
	if !strings.Contains(qres.Output, "sentence1") {
		t.Errorf("query output = %q, want 'sentence1'", qres.Output)
	}
}

// TestBackgroundTaskIntegration runs a real sub-agent against a live local
// model. Set NINE_LIVE_MODEL to an Ollama tag (e.g. qwen3.5:4b) to run it, and
// NINE_LLM_ENDPOINT if Ollama is not at localhost:11434.
func TestBackgroundTaskIntegration(t *testing.T) {
	model := os.Getenv("NINE_LIVE_MODEL")
	if model == "" {
		t.Skip("NINE_LIVE_MODEL not set")
	}

	root := wireModuleRoot(t)
	tmpDir, err := os.MkdirTemp("", "nine-task-integ")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Build shell and files plugins.
	for _, name := range []string{"shell", "files"} {
		bin := filepath.Join(tmpDir, name)
		cmd := exec.Command("go", "build", "-mod=vendor", "-o", bin, "./plugins/"+name)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", name, err, out)
		}
	}

	mgr := plugin.NewManager("")
	shellPlug, err := mgr.Start(filepath.Join(tmpDir, "shell"))
	if err != nil {
		t.Fatalf("start shell: %v", err)
	}
	t.Cleanup(func() { mgr.Stop(shellPlug) })

	filesPlug, err := mgr.Start(filepath.Join(tmpDir, "files"))
	if err != nil {
		t.Fatalf("start files: %v", err)
	}
	t.Cleanup(func() { mgr.Stop(filesPlug) })

	var tools []ninectx.ToolWithVector
	for _, p := range []*plugin.Plugin{shellPlug, filesPlug} {
		for _, td := range p.Tools {
			tools = append(tools, ninectx.ToolWithVector{
				Tool: llm.ToolDef{Name: td.Name, Description: td.Description, InputSchema: td.InputSchema},
			})
		}
	}

	provider := ollama.New(model, os.Getenv("NINE_LLM_ENDPOINT"), 0, false, 0)
	queue := llm.NewQueue(provider, 1)
	builder := ninectx.New(ninectx.Config{Budget: 8000, ToolTopN: 10})

	taskFactory := func(_ string) *agent.Loop {
		d := agent.New()
		d.RegisterPlugin(mgr, shellPlug)
		d.RegisterPlugin(mgr, filesPlug)
		return agent.NewLoop(agent.Config{
			SystemCore: subAgentTestPrompt,
			Priority:   llm.PriorityBackground,
			Tools:      tools,
			MaxTokens:  1024,
		}, builder, queue, d)
	}

	notif := runtime.NewInMemoryNotifStore()
	outFile := filepath.Join(tmpDir, "wordcount.txt")
	description := "Write the text 'hello from nine task' to the file " + outFile

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	runtime.SpawnSubAgent(ctx, "task-integ-1", description, taskFactory("task-integ-1"), notif, "conv-spawner")

	// Wait for notification.
	deadline := time.Now().Add(55 * time.Second)
	for time.Now().Before(deadline) {
		ns, _ := notif.Fetch("conv-spawner")
		if len(ns) > 0 {
			t.Logf("notification: %s", ns[0])
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	content, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("output file not created: %v", err)
	}
	if !strings.Contains(string(content), "hello from nine task") {
		t.Errorf("file content = %q", content)
	}
}

// ---- helpers ----

func tmpSock(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "nine-s8")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir + "/s.sock"
}

func waitForSock(t *testing.T, sock string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if c, err := protocol.Connect(sock); err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("daemon socket never appeared: " + sock)
}

func wireModuleRoot(t *testing.T) string {
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

// ---- RunSubAgentSync ----

func newSubAgentLoop(provider llm.Provider) *agent.Loop {
	return agent.NewLoop(agent.Config{
		SystemCore: subAgentTestPrompt,
		Priority:   llm.PriorityBackground,
	}, ninectx.New(ninectx.Config{Budget: 100_000}), llm.NewQueue(provider, 1), agent.New())
}

func TestRunSubAgentSync(t *testing.T) {
	const want = "sub-agent answer"
	provider := llm.ProviderFunc(func(_ context.Context, _ llm.Request) (llm.Response, error) {
		return llm.Response{Text: want, StopReason: "end_turn"}, nil
	})

	result, err := runtime.RunSubAgentSync(context.Background(), "sub-1", "do something", newSubAgentLoop(provider), nil, nil)
	if err != nil {
		t.Fatalf("RunSubAgentSync: %v", err)
	}
	if result != want {
		t.Errorf("result = %q, want %q", result, want)
	}
}

func TestRunSubAgentSyncContextCancellation(t *testing.T) {
	// Provider blocks until context is cancelled.
	provider := llm.ProviderFunc(func(ctx context.Context, _ llm.Request) (llm.Response, error) {
		<-ctx.Done()
		return llm.Response{}, ctx.Err()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := runtime.RunSubAgentSync(ctx, "sub-cancel", "blocked task", newSubAgentLoop(provider), nil, nil)
	if err == nil {
		t.Fatal("expected error on context cancellation, got nil")
	}
}

func TestRunSubAgentSyncConcurrent(t *testing.T) {
	const n = 3
	var wg sync.WaitGroup
	results := make([]string, n)
	errs := make([]error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			answer := fmt.Sprintf("result-%d", i)
			provider := llm.ProviderFunc(func(_ context.Context, _ llm.Request) (llm.Response, error) {
				return llm.Response{Text: answer, StopReason: "end_turn"}, nil
			})
			results[i], errs[i] = runtime.RunSubAgentSync(
				context.Background(), fmt.Sprintf("sub-%d", i), "task", newSubAgentLoop(provider), nil, nil)
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Errorf("sub-agent %d error: %v", i, errs[i])
		}
		want := fmt.Sprintf("result-%d", i)
		if results[i] != want {
			t.Errorf("sub-agent %d result = %q, want %q", i, results[i], want)
		}
	}
}
