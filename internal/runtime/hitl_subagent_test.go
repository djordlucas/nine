package runtime_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"nine/internal/llm"
	"nine/internal/protocol"
	"nine/internal/runtime"
)

// gateRecorder captures the human_input_required messages a HITL coordinator
// emits, and optionally answers each one so the blocked asker proceeds.
type gateRecorder struct {
	mu   sync.Mutex
	msgs []protocol.Msg
	to   []string // owning session each message was emitted to, index-aligned
}

func (g *gateRecorder) record(agentID string, msg protocol.Msg) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.msgs = append(g.msgs, msg)
	g.to = append(g.to, agentID)
}

func (g *gateRecorder) snapshot() ([]protocol.Msg, []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]protocol.Msg(nil), g.msgs...), append([]string(nil), g.to...)
}

// answerWith wires a HITL emit hook that records every prompt and answers it
// with reply, so a gated tool call unblocks instead of waiting out the timeout.
func answerWith(hitl *runtime.HITL, g *gateRecorder, reply string) {
	hitl.SetEmit(func(agentID string, msg protocol.Msg) {
		if msg.Type != "human_input_required" {
			return
		}
		g.record(agentID, msg)
		go hitl.Answer(msg.RequestID, reply)
	})
}

// memorySetCall is a tool call the core dispatcher can always serve, used as
// the stand-in for a risky tool listed in [hitl].require_approval.
var memorySetCall = llm.Response{
	StopReason: "tool_use",
	ToolCalls:  []llm.ToolCall{{ID: "t1", Name: "memory_set", Input: json.RawMessage(`{"key":"k","value":"v"}`)}},
}

// delegateThenGate scripts: root delegates once, the sub-agent calls the gated
// tool, then everyone wraps up.
func delegateThenGate(n int, _ llm.Request) llm.Response {
	switch n {
	case 1:
		return runAgentCall("store something", "")
	case 2:
		return memorySetCall
	}
	return llm.Response{Text: "done", StopReason: "end_turn"}
}

// R-HITL.5: a tool in require_approval that a *sub-agent* reaches for is put to
// the human, on the owning session's stream and attributed to the sub-agent.
// Before this, delegation was a way around the gate entirely.
func TestSubAgentGatedToolPromptsOwningSession(t *testing.T) {
	store := newRoleTestStore(t)
	hitl := runtime.NewHITL(store, time.Minute)
	g := &gateRecorder{}
	answerWith(hitl, g, "yes")

	p := &scriptedProvider{script: delegateThenGate}
	factory := rolesTestBuilder(t, p, func(c *runtime.AgentBuilderConfig) {
		c.Loop.Memory = store
		c.HITL = hitl
		c.ApprovalTools = []string{"memory_set"}
		c.GateSubAgents = true
	})
	if _, err := factory.Build("root-1", true).Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	msgs, to := g.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("approval prompts = %d, want 1 (the sub-agent's gated call)", len(msgs))
	}
	// Routed to the session a human is watching, not the sub-agent's own ID —
	// which is not a session at all, so emitting there would drop the prompt.
	if to[0] != "root-1" || msgs[0].AgentID != "root-1" {
		t.Errorf("prompt routed to %q (AgentID %q), want the owning session root-1", to[0], msgs[0].AgentID)
	}
	// Attribution: without it the user sees a prompt for work they never asked for.
	if !strings.Contains(msgs[0].Origin, "sub-agent") || !strings.Contains(msgs[0].Origin, "store something") {
		t.Errorf("Origin = %q, want it to name the sub-agent and its task", msgs[0].Origin)
	}
	if !strings.Contains(msgs[0].Question, "memory_set") {
		t.Errorf("Question = %q, want it to name the gated tool", msgs[0].Question)
	}
}

// A root session's own gated call is unchanged by all this: still prompted,
// still unattributed (no sub-agent is involved).
func TestRootGatedToolPromptUnattributed(t *testing.T) {
	store := newRoleTestStore(t)
	hitl := runtime.NewHITL(store, time.Minute)
	g := &gateRecorder{}
	answerWith(hitl, g, "yes")

	p := &scriptedProvider{script: func(n int, _ llm.Request) llm.Response {
		if n == 1 {
			return memorySetCall
		}
		return llm.Response{Text: "done", StopReason: "end_turn"}
	}}
	factory := rolesTestBuilder(t, p, func(c *runtime.AgentBuilderConfig) {
		c.Loop.Memory = store
		c.HITL = hitl
		c.ApprovalTools = []string{"memory_set"}
		c.GateSubAgents = true
	})
	if _, err := factory.Build("root-1", true).Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	msgs, _ := g.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("approval prompts = %d, want 1", len(msgs))
	}
	if msgs[0].Origin != "" {
		t.Errorf("Origin = %q, want empty when the session itself asks", msgs[0].Origin)
	}
}

// [hitl].gate_sub_agents = false restores the old behavior: the sub-agent runs
// the gated tool unprompted. The root's own gate is unaffected.
func TestSubAgentGatingCanBeDisabled(t *testing.T) {
	store := newRoleTestStore(t)
	hitl := runtime.NewHITL(store, time.Minute)
	g := &gateRecorder{}
	answerWith(hitl, g, "yes")

	p := &scriptedProvider{script: delegateThenGate}
	factory := rolesTestBuilder(t, p, func(c *runtime.AgentBuilderConfig) {
		c.Loop.Memory = store
		c.HITL = hitl
		c.ApprovalTools = []string{"memory_set"}
		c.GateSubAgents = false
	})
	if _, err := factory.Build("root-1", true).Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if msgs, _ := g.snapshot(); len(msgs) != 0 {
		t.Errorf("approval prompts = %d, want 0 with gate_sub_agents off", len(msgs))
	}
}

// R-HITL.1's surviving half: a non-interactive root (goal/pursue, reflection,
// CLI one-shot) has no human attached, so neither it nor its sub-agents are
// gated — blocking there would hang on a question nobody can see.
func TestNonInteractiveRootNeverGatesSubAgents(t *testing.T) {
	store := newRoleTestStore(t)
	hitl := runtime.NewHITL(store, time.Minute)
	g := &gateRecorder{}
	answerWith(hitl, g, "yes")

	p := &scriptedProvider{script: delegateThenGate}
	factory := rolesTestBuilder(t, p, func(c *runtime.AgentBuilderConfig) {
		c.Loop.Memory = store
		c.HITL = hitl
		c.ApprovalTools = []string{"memory_set"}
		c.GateSubAgents = true
	})
	if _, err := factory.Build("root-1", false).Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if msgs, _ := g.snapshot(); len(msgs) != 0 {
		t.Errorf("approval prompts = %d, want 0 for a non-interactive root", len(msgs))
	}
}

// Gates propagate, ask_human does not (R-HITL.1). A sub-agent is a bounded
// worker: it may be stopped for permission, but never gets to interrogate the
// user itself.
func TestSubAgentNeverAdvertisesAskHuman(t *testing.T) {
	store := newRoleTestStore(t)
	hitl := runtime.NewHITL(store, time.Minute)
	g := &gateRecorder{}
	answerWith(hitl, g, "yes")

	p := &scriptedProvider{script: delegateThenGate}
	factory := rolesTestBuilder(t, p, func(c *runtime.AgentBuilderConfig) {
		c.Loop.Memory = store
		c.HITL = hitl
		c.ApprovalTools = []string{"memory_set"}
		c.GateSubAgents = true
	})
	if _, err := factory.Build("root-1", true).Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if p.nCalls() < 2 {
		t.Fatalf("expected a sub-agent turn, got %d calls", p.nCalls())
	}
	if root := toolNames(p.call(1)); !root["ask_human"] {
		t.Error("interactive root should still advertise ask_human")
	}
	if sub := toolNames(p.call(2)); sub["ask_human"] {
		t.Error("sub-agent must not advertise ask_human even when its gates are live (R-HITL.1)")
	}
}

// Rejecting a sub-agent's gate fails that tool call, and the failure is the
// model's to handle — the same contract as a rejected root-session gate.
func TestSubAgentGateRejectionFailsTheCall(t *testing.T) {
	store := newRoleTestStore(t)
	hitl := runtime.NewHITL(store, time.Minute)
	g := &gateRecorder{}
	answerWith(hitl, g, "no")

	var subObservation string
	p := &scriptedProvider{}
	p.script = func(n int, req llm.Request) llm.Response {
		switch n {
		case 1:
			return runAgentCall("store something", "")
		case 2:
			return memorySetCall
		case 3:
			// The sub-agent's next turn carries the rejected call's result.
			for _, m := range req.Messages {
				for _, r := range m.ToolResults {
					subObservation += r.Content
				}
			}
		}
		return llm.Response{Text: "done", StopReason: "end_turn"}
	}
	factory := rolesTestBuilder(t, p, func(c *runtime.AgentBuilderConfig) {
		c.Loop.Memory = store
		c.HITL = hitl
		c.ApprovalTools = []string{"memory_set"}
		c.GateSubAgents = true
	})
	if _, err := factory.Build("root-1", true).Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if msgs, _ := g.snapshot(); len(msgs) != 1 {
		t.Fatalf("approval prompts = %d, want 1", len(msgs))
	}
	if !strings.Contains(subObservation, "rejected by user") {
		t.Errorf("sub-agent observation = %q, want the rejection surfaced as a tool failure", subObservation)
	}
}

// A run_agents fan-out can have several sub-agents at a gate simultaneously.
// Each must get its own request (distinct IDs), all routed to the one session
// the human is watching — a shared pending row would collide and one asker
// would be answered twice while the other waits out its timeout.
func TestParallelSubAgentGatesGetDistinctRequests(t *testing.T) {
	const fanout = 3

	store := newRoleTestStore(t)
	hitl := runtime.NewHITL(store, time.Minute)
	g := &gateRecorder{}

	// Hold every prompt until all `fanout` have arrived, forcing them to be
	// genuinely concurrent rather than serialised by luck of scheduling.
	var mu sync.Mutex
	var held []string
	hitl.SetEmit(func(agentID string, msg protocol.Msg) {
		if msg.Type != "human_input_required" {
			return
		}
		g.record(agentID, msg)
		mu.Lock()
		held = append(held, msg.RequestID)
		full := len(held) == fanout
		ids := append([]string(nil), held...)
		mu.Unlock()
		if full {
			for _, id := range ids {
				go hitl.Answer(id, "yes")
			}
		}
	})

	tasks := make([]map[string]string, fanout)
	for i := range tasks {
		tasks[i] = map[string]string{"task": string(rune('a'+i)) + "-task"}
	}
	input, err := json.Marshal(map[string]any{"tasks": tasks, "timeout_seconds": 60})
	if err != nil {
		t.Fatal(err)
	}

	var turn int
	var tmu sync.Mutex
	p := &scriptedProvider{}
	p.script = func(n int, _ llm.Request) llm.Response {
		tmu.Lock()
		defer tmu.Unlock()
		turn++
		switch {
		case turn == 1:
			return llm.Response{
				StopReason: "tool_use",
				ToolCalls:  []llm.ToolCall{{ID: "t1", Name: "run_agents", Input: input}},
			}
		case turn <= 1+fanout: // each sub-agent's first turn hits the gate
			return memorySetCall
		}
		return llm.Response{Text: "done", StopReason: "end_turn"}
	}

	factory := rolesTestBuilder(t, p, func(c *runtime.AgentBuilderConfig) {
		c.Loop.Memory = store
		c.HITL = hitl
		c.ApprovalTools = []string{"memory_set"}
		c.GateSubAgents = true
	})
	if _, err := factory.Build("root-1", true).Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	msgs, to := g.snapshot()
	if len(msgs) != fanout {
		t.Fatalf("approval prompts = %d, want %d (one per parallel sub-agent)", len(msgs), fanout)
	}
	ids := make(map[string]bool, fanout)
	for i, m := range msgs {
		if ids[m.RequestID] {
			t.Errorf("duplicate RequestID %q — parallel askers collided on one pending row", m.RequestID)
		}
		ids[m.RequestID] = true
		if to[i] != "root-1" {
			t.Errorf("prompt %d routed to %q, want the owning session root-1", i, to[i])
		}
		if m.Origin == "" {
			t.Errorf("prompt %d has no Origin; every sub-agent prompt must say who is asking", i)
		}
	}
}
