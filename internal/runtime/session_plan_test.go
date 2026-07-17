package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nine/internal/llm"
	"nine/internal/memory"
	"nine/internal/memory/memtest"
	"nine/internal/runtime"
)

// ---- loadOrCreatePlan ----

func TestLoadOrCreatePlanNoStore(t *testing.T) {
	persisted, plan, err := runtime.LoadOrCreatePlanForTest(nil, "agent-1", []string{"active"}, false)
	if err != nil {
		t.Fatalf("LoadOrCreatePlanForTest: %v", err)
	}
	if persisted {
		t.Error("persisted = true, want false (no store)")
	}
	if plan.Status != "active" {
		t.Errorf("plan.Status = %q, want active", plan.Status)
	}
	if len(plan.Stages) != 1 || plan.Stages[0].Kind != "active" || plan.Stages[0].Status != "active" {
		t.Errorf("plan.Stages = %+v", plan.Stages)
	}
}

func TestLoadOrCreatePlanLazyWithStore(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck

	persisted, _, err := runtime.LoadOrCreatePlanForTest(store, "agent-2", []string{"active"}, false)
	if err != nil {
		t.Fatalf("LoadOrCreatePlanForTest: %v", err)
	}
	if persisted {
		t.Error("persisted = true, want false for a lazy profile")
	}

	got, err := store.SessionPlanGet("agent-2")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("SessionPlanGet = %+v, want nil (lazy plan not yet written)", got)
	}
}

func TestLoadOrCreatePlanEagerPersists(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck

	persisted, _, err := runtime.LoadOrCreatePlanForTest(store, "agent-3", []string{"active"}, true)
	if err != nil {
		t.Fatalf("LoadOrCreatePlanForTest: %v", err)
	}
	if !persisted {
		t.Error("persisted = false, want true for an eager profile")
	}

	got, err := store.SessionPlanGet("agent-3")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("SessionPlanGet = nil, want a row written eagerly")
	}
}

func TestLoadOrCreatePlanLoadsExisting(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck

	existing := &memory.SessionPlan{
		ID:     "agent-4",
		Status: "active",
		Stages: []memory.SessionStage{{Name: "active", Kind: "active", Status: "done", Result: "all done"}},
	}
	if err := store.SessionPlanSave(existing); err != nil {
		t.Fatal(err)
	}

	persisted, plan, err := runtime.LoadOrCreatePlanForTest(store, "agent-4", []string{"active"}, false)
	if err != nil {
		t.Fatalf("LoadOrCreatePlanForTest: %v", err)
	}
	if !persisted {
		t.Error("persisted = false, want true for an existing row")
	}
	if len(plan.Stages) != 1 || plan.Stages[0].Status != "done" || plan.Stages[0].Result != "all done" {
		t.Errorf("plan.Stages = %+v, want existing row preserved", plan.Stages)
	}
}

func TestLoadOrCreatePlanUnknownStageKind(t *testing.T) {
	_, _, err := runtime.LoadOrCreatePlanForTest(nil, "agent-5", []string{"no-such-kind"}, false)
	if err == nil {
		t.Fatal("expected error for unregistered stage kind")
	}
}

// ---- the trivial [active] stage ----

func TestActiveStageIsTrivial(t *testing.T) {
	h := runtime.StageRegistry["active"]()
	ctx := context.Background()

	if err := h.Init(ctx, "agent-1", nil); err != nil {
		t.Errorf("Init: %v", err)
	}
	if err := h.OnTurnEnd(ctx, "agent-1", "result", nil); err != nil {
		t.Errorf("OnTurnEnd: %v", err)
	}
	if err := h.OnTurnEnd(ctx, "agent-1", "", runtime.ErrStall); err != nil {
		t.Errorf("OnTurnEnd(ErrStall): %v", err)
	}
	if text, ok := h.OnIdle(ctx, "agent-1"); ok || text != "" {
		t.Errorf("OnIdle = (%q, %v), want (\"\", false)", text, ok)
	}
}

// ---- planNeedsResume ----

func TestPlanNeedsResume(t *testing.T) {
	idleCfg, _ := json.Marshal(map[string]int{"idle_interval_seconds": 60})

	cases := []struct {
		name string
		plan memory.SessionPlan
		want bool
	}{
		{
			name: "plain active conversation",
			plan: memory.SessionPlan{Status: "active", Stages: []memory.SessionStage{{Status: "active"}}},
			want: false,
		},
		{
			name: "active idle-capable stage",
			plan: memory.SessionPlan{Status: "active", Stages: []memory.SessionStage{{Status: "active", Config: idleCfg}}},
			want: true,
		},
		{
			name: "idle-capable but stage not active",
			plan: memory.SessionPlan{Status: "active", Stages: []memory.SessionStage{{Status: "done", Config: idleCfg}}},
			want: false,
		},
		{
			name: "idle-capable but plan paused",
			plan: memory.SessionPlan{Status: "paused", Stages: []memory.SessionStage{{Status: "active", Config: idleCfg}}},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := runtime.PlanNeedsResumeForTest(tc.plan); got != tc.want {
				t.Errorf("planNeedsResume = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---- OnTurnEnd / ErrStall fan-out ----

// recordingStage records every (result, err) it's notified of via OnTurnEnd.
type recordingStage struct {
	mu    *sync.Mutex
	calls *[]error
}

func (s recordingStage) Init(context.Context, string, json.RawMessage) error { return nil }
func (s recordingStage) OnTurnEnd(_ context.Context, _ string, _ string, err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	*s.calls = append(*s.calls, err)
	return nil
}
func (s recordingStage) OnIdle(context.Context, string) (string, bool) { return "", false }

func TestOnTurnEndFanOutAndStall(t *testing.T) {
	var mu sync.Mutex
	var calls []error
	runtime.StageRegistry["recording-test"] = func() runtime.StageHandler {
		return recordingStage{mu: &mu, calls: &calls}
	}
	t.Cleanup(func() { delete(runtime.StageRegistry, "recording-test") })

	loop := workerLoop(constProvider("no tools"))
	stall := runtime.StallConfig{Limit: 2}
	w, err := runtime.NewAgentWorkerWithPlanForTest(
		"agent-turnend", loop, nil, nil, stall, nil, []string{"recording-test"}, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.StopAgentWorker() })

	for i := range 2 {
		if _, err := w.TurnAgentWorker(context.Background(), "hi"); err != nil {
			t.Fatalf("turn %d: %v", i+1, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	// Two ordinary end-of-turn notifications, plus a stall fan-out after the
	// second (no-tool) turn hits the stall limit.
	if len(calls) != 3 {
		t.Fatalf("OnTurnEnd called %d times, want 3: %v", len(calls), calls)
	}
	if calls[0] != nil || calls[1] != nil {
		t.Errorf("calls[0:2] = %v, want nil errors", calls[:2])
	}
	if !errors.Is(calls[2], runtime.ErrStall) {
		t.Errorf("calls[2] = %v, want ErrStall", calls[2])
	}
}

// ---- idle scheduler ----

// idleStage always has work to do, recording how many times OnIdle fires.
type idleStage struct {
	calls *atomic.Int32
}

func (s idleStage) Init(context.Context, string, json.RawMessage) error    { return nil }
func (s idleStage) OnTurnEnd(context.Context, string, string, error) error { return nil }
func (s idleStage) OnIdle(context.Context, string) (string, bool) {
	s.calls.Add(1)
	return "idle turn", true
}

func TestIdleSchedulerRunsOnIdleTurn(t *testing.T) {
	var calls atomic.Int32
	runtime.StageRegistry["idle-test"] = func() runtime.StageHandler {
		return idleStage{calls: &calls}
	}
	t.Cleanup(func() { delete(runtime.StageRegistry, "idle-test") })

	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck

	cfg, _ := json.Marshal(map[string]int{"idle_interval_seconds": 1})
	plan := &memory.SessionPlan{
		ID:     "agent-idle",
		Status: "active",
		Stages: []memory.SessionStage{{Name: "idle-test", Kind: "idle-test", Status: "active", Config: cfg}},
	}
	if err := store.SessionPlanSave(plan); err != nil {
		t.Fatal(err)
	}

	var turns atomic.Int32
	provider := llm.ProviderFunc(func(_ context.Context, _ llm.Request) (llm.Response, error) {
		turns.Add(1)
		return llm.Response{Text: "ok", StopReason: "end_turn"}, nil
	})
	loop := workerLoop(provider)

	w, err := runtime.NewAgentWorkerWithPlanForTest(
		"agent-idle", loop, nil, nil, runtime.StallConfig{}, store, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.StopAgentWorker() })

	deadline := time.After(3 * time.Second)
	for turns.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("idle-triggered turn did not run within 3s")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if calls.Load() == 0 {
		t.Error("OnIdle was never called")
	}
}

// ---- daemon-startup resume pass ----

// quietIdleStage is idle-capable (so it qualifies for resume) but never has
// work to do, so it doesn't run a turn during the test.
type quietIdleStage struct{}

func (quietIdleStage) Init(context.Context, string, json.RawMessage) error    { return nil }
func (quietIdleStage) OnTurnEnd(context.Context, string, string, error) error { return nil }
func (quietIdleStage) OnIdle(context.Context, string) (string, bool)          { return "", false }

func TestResumeSessionsStartsIdleCapableSessions(t *testing.T) {
	runtime.StageRegistry["quiet-idle-test"] = func() runtime.StageHandler { return quietIdleStage{} }
	t.Cleanup(func() { delete(runtime.StageRegistry, "quiet-idle-test") })

	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck

	cfg, _ := json.Marshal(map[string]int{"idle_interval_seconds": 60})
	resumable := &memory.SessionPlan{
		ID:     "resume-agent",
		Status: "active",
		Stages: []memory.SessionStage{{Name: "quiet-idle-test", Kind: "quiet-idle-test", Status: "active", Config: cfg}},
	}
	plain := &memory.SessionPlan{
		ID:     "plain-agent",
		Status: "active",
		Stages: []memory.SessionStage{{Name: "active", Kind: "active", Status: "active"}},
	}
	if err := store.SessionPlanSave(resumable); err != nil {
		t.Fatal(err)
	}
	if err := store.SessionPlanSave(plain); err != nil {
		t.Fatal(err)
	}

	provider := seqProvider(nil)
	d, sock := startDaemon(t, makeFactory(provider), nil, nil)
	d.ConfigurePlanStore(store)

	if err := d.ResumeSessions(context.Background()); err != nil {
		t.Fatalf("ResumeSessions: %v", err)
	}

	c := dial(t, sock)
	info, err := c.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	var ids []string
	for _, a := range info.Agents {
		ids = append(ids, a.ID)
	}
	found := false
	for _, id := range ids {
		if id == "resume-agent" {
			found = true
		}
		if id == "plain-agent" {
			t.Error("plain [active] conversation was resumed, want it to stay attach-on-demand")
		}
	}
	if !found {
		t.Errorf("resume-agent not in active sessions: %v", ids)
	}
}
