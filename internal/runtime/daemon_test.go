package runtime_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nine/internal/agent"
	ninectx "nine/internal/context"
	"nine/internal/llm"
	"nine/internal/memory"
	"nine/internal/memory/memtest"
	"nine/internal/protocol"
	"nine/internal/runtime"
	"nine/internal/workflow"
)

// ---- test helpers ----

// seqProvider cycles through responses.
func seqProvider(responses []llm.Response) llm.Provider {
	var i atomic.Int32
	return llm.ProviderFunc(func(_ context.Context, _ llm.Request) (llm.Response, error) {
		idx := int(i.Add(1)) - 1
		if idx >= len(responses) {
			return llm.Response{Text: "done", StopReason: "end_turn"}, nil
		}
		return responses[idx], nil
	})
}

func finalResp(text string) llm.Response {
	return llm.Response{Text: text, StopReason: "end_turn"}
}

// makeFactory returns a LoopFactory backed by the given provider.
func makeFactory(provider llm.Provider) runtime.LoopFactory {
	builder := ninectx.New(ninectx.Config{Budget: 100_000})
	queue := llm.NewQueue(provider, 4)
	dispatcher := agent.New()
	return func(agentID string, p runtime.RoleParams) *agent.Loop {
		return agent.NewLoop(agent.Config{
			Role:       p.Role,
			SystemCore: "test agent",
			Priority:   llm.PriorityConversation,
		}, builder, queue, dispatcher)
	}
}

// startDaemon starts a daemon on a temp socket and returns it and the socket path.
// macOS limits Unix socket paths to 104 bytes, so we use /tmp not t.TempDir().
func startDaemon(t *testing.T, factory runtime.LoopFactory, ckpt runtime.CheckpointStore, notif runtime.NotifStore) (*runtime.Daemon, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "nine-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sockPath := dir + "/s.sock"
	d := runtime.New(sockPath, runtime.InternalAgent{Build: factory, Notifications: notif}, ckpt)
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})

	go func() {
		close(ready)
		if err := d.Start(ctx); err != nil && ctx.Err() == nil {
			t.Errorf("daemon.Start: %v", err)
		}
	}()

	<-ready
	// Poll until socket is available.
	for i := 0; i < 50; i++ {
		conn, err := net.Dial("unix", sockPath)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Cleanup(func() {
		cancel()
		d.Stop()
	})
	return d, sockPath
}

// dial creates a client connected to sockPath.
func dial(t *testing.T, sockPath string) *protocol.Client {
	t.Helper()
	c, err := protocol.Connect(sockPath)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// ---- unit tests ----

func TestNewConversation(t *testing.T) {
	provider := seqProvider([]llm.Response{finalResp("hello")})
	_, sock := startDaemon(t, makeFactory(provider), nil, nil)
	c := dial(t, sock)

	id, err := c.NewConversation()
	if err != nil {
		t.Fatalf("NewConversation: %v", err)
	}
	if id == "" {
		t.Error("expected non-empty conversation ID")
	}

	resp, err := c.Turn(id, "say hi")
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if resp != "hello" {
		t.Errorf("response = %q, want 'hello'", resp)
	}
}

func TestContextInspectionEndToEnd(t *testing.T) {
	// The provider fails the test if called: `context` must never trigger an LLM
	// round-trip, even end-to-end through the daemon.
	provider := llm.ProviderFunc(func(_ context.Context, _ llm.Request) (llm.Response, error) {
		t.Error("context inspection must not call the LLM")
		return llm.Response{}, nil
	})
	_, sock := startDaemon(t, makeFactory(provider), nil, nil)
	c := dial(t, sock)

	id, err := c.NewConversation()
	if err != nil {
		t.Fatalf("NewConversation: %v", err)
	}

	raw, err := c.Context(id)
	if err != nil {
		t.Fatalf("Context: %v", err)
	}
	var rep ninectx.Report
	if err := json.Unmarshal([]byte(raw), &rep); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if rep.Budget != 100_000 {
		t.Errorf("Budget = %d, want 100000", rep.Budget)
	}
	if len(rep.Sections) == 0 {
		t.Error("expected per-section breakdown, got none")
	}
	if !strings.Contains(rep.System, "test agent") {
		t.Errorf("assembled system prompt missing core: %q", rep.System)
	}

	// An unknown session id is a clean error, not a panic.
	if _, err := c.Context("no-such-id"); err == nil {
		t.Error("expected error for unknown session id")
	}
}

func TestUnknownConversation(t *testing.T) {
	provider := seqProvider([]llm.Response{finalResp("hi")})
	_, sock := startDaemon(t, makeFactory(provider), nil, nil)
	c := dial(t, sock)

	_, err := c.Turn("no-such-id", "hello")
	if err == nil {
		t.Error("expected error for unknown conversation ID")
	}
}

func TestConcurrentIndependentRouting(t *testing.T) {
	// Two clients with different agent IDs must receive independent responses.
	var mu sync.Mutex
	received := map[string]string{} // agentID → response

	// Use a per-call provider that echoes the conversation ID it was created with.
	// We do this by creating separate factories per conversation, but since the
	// factory doesn't know the agent ID at construction time, we use a capturing
	// provider that returns distinct answers based on call order.
	callN := atomic.Int32{}
	provider := llm.ProviderFunc(func(_ context.Context, req llm.Request) (llm.Response, error) {
		n := callN.Add(1)
		return llm.Response{Text: fmt.Sprintf("response-%d", n), StopReason: "end_turn"}, nil
	})

	_, sock := startDaemon(t, makeFactory(provider), nil, nil)

	var wg sync.WaitGroup
	wg.Add(2)
	errors := make(chan error, 2)

	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			c, err := protocol.Connect(sock)
			if err != nil {
				errors <- err
				return
			}
			defer c.Close() //nolint:errcheck

			id, err := c.NewConversation()
			if err != nil {
				errors <- err
				return
			}
			resp, err := c.Turn(id, "hi")
			if err != nil {
				errors <- err
				return
			}
			mu.Lock()
			received[id] = resp
			mu.Unlock()
		}()
	}
	wg.Wait()
	close(errors)

	for err := range errors {
		t.Fatal(err)
	}

	if len(received) != 2 {
		t.Fatalf("received %d responses, want 2", len(received))
	}
	// Each conversation gets its own response (no cross-contamination).
	ids := make([]string, 0, 2)
	for id := range received {
		ids = append(ids, id)
	}
	if ids[0] == ids[1] {
		t.Error("two conversations have the same agent ID")
	}
	if received[ids[0]] == received[ids[1]] {
		t.Error("two conversations got the same response text (routing may be broken)")
	}
}

func TestAttachResumesCheckpoint(t *testing.T) {
	ckpt := runtime.NewInMemoryCheckpointStore()
	responses := []llm.Response{
		finalResp("turn1"),
		finalResp("turn2-after-attach"),
	}
	provider := seqProvider(responses)

	_, sock := startDaemon(t, makeFactory(provider), ckpt, nil)
	c := dial(t, sock)

	// First turn — establishes history in the checkpoint.
	id, err := c.NewConversation()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Turn(id, "first message"); err != nil {
		t.Fatal(err)
	}

	// Simulate reconnect: create a new daemon backed by the same checkpoint store.
	_, sock2 := startDaemon(t, makeFactory(provider), ckpt, nil)
	c2 := dial(t, sock2)

	if _, err := c2.Attach(id); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	resp, err := c2.Turn(id, "second message")
	if err != nil {
		t.Fatalf("second turn: %v", err)
	}
	if resp != "turn2-after-attach" {
		t.Errorf("response = %q, want 'turn2-after-attach'", resp)
	}
}

// TestStopSession verifies that stopping a session drops it from daemon status
// and deletes its checkpoint, so it cannot be attached again.
func TestStopSession(t *testing.T) {
	ckpt := runtime.NewInMemoryCheckpointStore()
	provider := seqProvider([]llm.Response{finalResp("hi")})
	_, sock := startDaemon(t, makeFactory(provider), ckpt, nil)
	c := dial(t, sock)

	id, err := c.NewConversation()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Turn(id, "first message"); err != nil {
		t.Fatal(err) // establishes a checkpoint
	}
	if _, found, _ := ckpt.Load(id); !found {
		t.Fatal("checkpoint should exist after a turn")
	}

	msg, err := c.StopSession(id, false)
	if err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	if !strings.Contains(msg, "stopped") {
		t.Errorf("stop reply = %q, want it to mention 'stopped'", msg)
	}

	// The session is gone from status and its checkpoint is deleted.
	info, err := c.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	for _, a := range info.Agents {
		if a.ID == id {
			t.Errorf("stopped session %q still listed in status", id)
		}
	}
	if _, found, _ := ckpt.Load(id); found {
		t.Errorf("checkpoint for stopped session %q was not deleted", id)
	}
}

// TestStopSessionNotFound verifies stopping an unknown id is reported as an error.
func TestStopSessionNotFound(t *testing.T) {
	ckpt := runtime.NewInMemoryCheckpointStore()
	provider := seqProvider(nil)
	_, sock := startDaemon(t, makeFactory(provider), ckpt, nil)
	c := dial(t, sock)

	if _, err := c.StopSession("no-such-session", false); err == nil {
		t.Error("expected an error stopping an unknown session, got nil")
	}
}

// TestStopAllSessions verifies --all clears every active session.
func TestStopAllSessions(t *testing.T) {
	ckpt := runtime.NewInMemoryCheckpointStore()
	provider := seqProvider([]llm.Response{finalResp("a"), finalResp("b")})
	_, sock := startDaemon(t, makeFactory(provider), ckpt, nil)
	c := dial(t, sock)

	for _, m := range []string{"one", "two"} {
		id, err := c.NewConversation()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Turn(id, m); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := c.StopSession("", true); err != nil {
		t.Fatalf("StopSession --all: %v", err)
	}
	info, err := c.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(info.Agents) != 0 {
		t.Errorf("after stop --all, status still lists %d agent(s): %v", len(info.Agents), info.Agents)
	}
}

func TestNotificationPrepend(t *testing.T) {
	notif := runtime.NewInMemoryNotifStore()

	var capturedRequest llm.Request
	provider := llm.ProviderFunc(func(_ context.Context, req llm.Request) (llm.Response, error) {
		capturedRequest = req
		return finalResp("ok"), nil
	})

	_, sock := startDaemon(t, makeFactory(provider), nil, notif)
	c := dial(t, sock)

	id, err := c.NewConversation()
	if err != nil {
		t.Fatal(err)
	}

	notif.Add(id, "background task completed")

	if _, err := c.Turn(id, "what's new?"); err != nil {
		t.Fatal(err)
	}

	// The notification should be prepended to the user message.
	found := false
	for _, m := range capturedRequest.Messages {
		if strings.Contains(m.Text, "background task completed") {
			found = true
		}
	}
	if !found {
		t.Error("notification text not found in LLM request messages")
	}
}

func TestThreeTurnsHistory(t *testing.T) {
	responses := []llm.Response{
		finalResp("reply-1"),
		finalResp("reply-2"),
		finalResp("reply-3"),
	}
	provider := seqProvider(responses)

	_, sock := startDaemon(t, makeFactory(provider), nil, nil)
	c := dial(t, sock)

	id, err := c.NewConversation()
	if err != nil {
		t.Fatal(err)
	}

	for i, want := range []string{"reply-1", "reply-2", "reply-3"} {
		got, err := c.Turn(id, fmt.Sprintf("message %d", i+1))
		if err != nil {
			t.Fatalf("turn %d: %v", i+1, err)
		}
		if got != want {
			t.Errorf("turn %d: got %q, want %q", i+1, got, want)
		}
	}
}

func TestInvalidJSON(t *testing.T) {
	provider := seqProvider(nil)
	_, sock := startDaemon(t, makeFactory(provider), nil, nil)

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	fmt.Fprintln(conn, "not-json")

	scanner := json.NewDecoder(conn)
	var reply protocol.Msg
	if err := scanner.Decode(&reply); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	if reply.Type != protocol.TypeError {
		t.Errorf("reply.Type = %q, want 'error'", reply.Type)
	}
}

// ---- status / list_goals ----

func TestDaemonStatus(t *testing.T) {
	provider := seqProvider([]llm.Response{finalResp("ok")})
	d, sock := startDaemon(t, makeFactory(provider), nil, nil)
	// Wire a queue-stat source so status surfaces LLM back-pressure (in prod
	// this is agentBuilder.QueueDepth; here a fixed stub is enough to prove the
	// daemon threads it through).
	d.SetQueueStatFn(func() (int, int, int) { return 3, 1, 4 })
	c := dial(t, sock)

	// Create a conversation so there's at least one agent. The reply carries the
	// session's resolved role (orchestrator for a normal conversation).
	id, role, _, err := c.NewConversationInteractive(false)
	if err != nil {
		t.Fatalf("NewConversation: %v", err)
	}
	if role != runtime.OrchestratorRole {
		t.Errorf("new-conversation role = %q, want %q", role, runtime.OrchestratorRole)
	}

	info, err := c.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if info.Uptime == "" {
		t.Error("uptime should be non-empty")
	}
	var found *protocol.AgentInfo
	for i, a := range info.Agents {
		if a.ID == id {
			found = &info.Agents[i]
		}
	}
	if found == nil {
		t.Fatalf("agent %q not in status agents: %v", id, info.Agents)
	}
	if found.Role != runtime.OrchestratorRole {
		t.Errorf("status agent role = %q, want %q", found.Role, runtime.OrchestratorRole)
	}
	if info.LLMQueue == nil {
		t.Fatal("status missing LLM queue stat")
	}
	if got := *info.LLMQueue; got.Pending != 3 || got.Inflight != 1 || got.MaxConcurrent != 4 {
		t.Errorf("queue stat = %+v, want {Pending:3 Inflight:1 MaxConcurrent:4}", got)
	}
}

func TestDaemonListGoals(t *testing.T) {
	provider := seqProvider(nil)
	d, sock := startDaemon(t, makeFactory(provider), nil, nil)
	c := dial(t, sock)

	d.ConfigureMemory(&mockGoalStore{goals: []memory.Goal{{ID: "g1", Description: "achieve greatness", Status: "active"}}})

	raw, err := c.ListGoals()
	if err != nil {
		t.Fatalf("ListGoals: %v", err)
	}
	if !strings.Contains(raw, "g1") || !strings.Contains(raw, "achieve greatness") {
		t.Errorf("ListGoals = %q, want entry for g1", raw)
	}
}

// plugin_call reaches a core-intercepted tool. list_tools advertises skill_*,
// memory_*, file_* and doc_* under the "core" plugin, but no subprocess serves
// them, so a plugin-only lookup answered "unknown tool" for a tool the client
// had just been shown — which is what the TUI's /skills and /memory hit.
func TestDaemonPluginCallReachesCoreTools(t *testing.T) {
	store := seedTestStore(t)
	if err := store.SkillUpsert(memory.Skill{
		Name:        "deploy-checklist",
		Description: "Steps to verify before shipping a release build.",
		Source:      memory.SkillSourceAgent,
	}); err != nil {
		t.Fatalf("SkillUpsert: %v", err)
	}
	core := agent.New()
	agent.RegisterSkillTools(core, store, nil)

	d, sock := startDaemon(t, makeFactory(seqProvider(nil)), nil, nil)
	d.ConfigureCoreTools(core)
	c := dial(t, sock)

	out, err := c.PluginCall("skill_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("PluginCall(skill_list): %v", err)
	}
	if !strings.Contains(out, "deploy-checklist") {
		t.Errorf("skill_list = %q, want it to include the stored skill", out)
	}
}

// A tool the core dispatcher does not hold still falls through to the plugin
// lookup rather than being answered by core — the new branch must not swallow
// names it cannot serve.
func TestDaemonPluginCallUnknownToolFallsThrough(t *testing.T) {
	d, sock := startDaemon(t, makeFactory(seqProvider(nil)), nil, nil)
	d.ConfigureCoreTools(agent.New()) // core holds nothing
	c := dial(t, sock)

	_, err := c.PluginCall("no_such_tool", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("PluginCall(no_such_tool) = nil error, want failure")
	}
	// No plugin manager is configured here, so the fall-through path is the one
	// that reports it.
	if !strings.Contains(err.Error(), "plugin caller not available") {
		t.Errorf("error = %v, want the plugin-side error", err)
	}
}

// ---- integration test ----

func TestDaemonIntegration(t *testing.T) {
	// Start daemon with mock LLM (no real API key needed).
	ckpt := runtime.NewInMemoryCheckpointStore()
	notif := runtime.NewInMemoryNotifStore()

	allResponses := []llm.Response{
		finalResp("turn-1-reply"),
		finalResp("turn-2-reply"),
		finalResp("turn-3-reply"),
		finalResp("after-reconnect"),
		finalResp("with-notification"),
	}
	provider := seqProvider(allResponses)
	factory := makeFactory(provider)

	_, sock := startDaemon(t, factory, ckpt, notif)
	c := dial(t, sock)

	// Start conversation.
	id, err := c.NewConversation()
	if err != nil {
		t.Fatalf("NewConversation: %v", err)
	}

	// 3 turns.
	for i, want := range []string{"turn-1-reply", "turn-2-reply", "turn-3-reply"} {
		got, err := c.Turn(id, fmt.Sprintf("turn %d", i+1))
		if err != nil {
			t.Fatalf("turn %d: %v", i+1, err)
		}
		if got != want {
			t.Errorf("turn %d: got %q, want %q", i+1, got, want)
		}
	}
	_ = c.Close()

	// Reconnect via a new daemon instance backed by the same checkpoint store.
	_, sock2 := startDaemon(t, factory, ckpt, notif)
	c2 := dial(t, sock2)

	if _, err := c2.Attach(id); err != nil {
		t.Fatalf("Attach after reconnect: %v", err)
	}
	resp, err := c2.Turn(id, "continuing after reconnect")
	if err != nil {
		t.Fatalf("turn after reconnect: %v", err)
	}
	if resp != "after-reconnect" {
		t.Errorf("reconnect resp = %q, want 'after-reconnect'", resp)
	}

	// Notification delivery.
	notif.Add(id, "async event arrived")
	var captured llm.Request
	capturingProvider := llm.ProviderFunc(func(_ context.Context, req llm.Request) (llm.Response, error) {
		captured = req
		return finalResp("with-notification"), nil
	})
	_, sock3 := startDaemon(t, makeFactory(capturingProvider), ckpt, notif)
	c3 := dial(t, sock3)
	c3.Attach(id)                      //nolint:errcheck,dogsled
	c3.Turn(id, "check notifications") //nolint:errcheck

	found := false
	for _, m := range captured.Messages {
		if strings.Contains(m.Text, "async event arrived") {
			found = true
		}
	}
	if !found {
		t.Error("notification not found in captured LLM request")
	}
}

// mockGoalStore is a minimal queryBackend stub for TestDaemonListGoals.
type mockGoalStore struct {
	goals  []memory.Goal
	events []memory.SessionEvent
}

func (m *mockGoalStore) GoalList() ([]memory.Goal, error)                   { return m.goals, nil }
func (m *mockGoalStore) GeneratedToolList() ([]memory.GeneratedTool, error) { return nil, nil }
func (m *mockGoalStore) WorkflowList(_ string) ([]workflow.Workflow, error) { return nil, nil }
func (m *mockGoalStore) WorkflowCancel(_ string) (int, error)               { return 0, nil }
func (m *mockGoalStore) WorkflowFail(_ string, _ bool) (int, error)         { return 0, nil }
func (m *mockGoalStore) UserNotificationList(_ bool) ([]memory.UserNotification, error) {
	return nil, nil
}
func (m *mockGoalStore) UserNotificationMarkSeen(_ string) error { return nil }
func (m *mockGoalStore) SessionEventsByAgent(_ string) ([]memory.SessionEvent, error) {
	return m.events, nil
}

// The session-lifecycle half of queryBackend. The daemon tests do not exercise
// deletion — internal/memory covers the cascade and session_delete_test.go covers
// the wiring — so these are inert stubs rather than a second implementation.
func (m *mockGoalStore) SessionList() ([]memory.SessionSummary, error) { return nil, nil }
func (m *mockGoalStore) SessionGet(string) (memory.SessionSummary, bool, error) {
	return memory.SessionSummary{}, false, nil
}
func (m *mockGoalStore) SessionsReapable(time.Duration) ([]memory.SessionSummary, error) {
	return nil, nil
}
func (m *mockGoalStore) SessionDelete(string) (memory.SessionDeleteCounts, error) {
	return memory.SessionDeleteCounts{}, nil
}
func (m *mockGoalStore) JobsRunning() ([]memory.Job, error) { return nil, nil }
func (m *mockGoalStore) SessionEventsAfter(_ int64, _ int) ([]memory.SessionEvent, error) {
	return nil, nil
}
func (m *mockGoalStore) EventCursorGet(_ string) (int64, error) { return 0, nil }
func (m *mockGoalStore) EventCursorSet(_ string, _ int64) error { return nil }
func (m *mockGoalStore) QueueMessage(_, _ string) error           { return nil }
func (m *mockGoalStore) UnconsumedMessagesCount(_ string) (int, error) { return 0, nil }
func (m *mockGoalStore) DrainQueuedMessage(_ string) (string, error) { return "", nil }

// Goal and skill verbs: inert, so these mocks still satisfy queryBackend.
func (m *mockGoalStore) GoalGet(string) (*memory.Goal, error)          { return nil, nil }
func (m *mockGoalStore) GoalCreate(_, _, _, _ string) error            { return nil }
func (m *mockGoalStore) GoalDelete(string) ([]string, error)           { return nil, nil }
func (m *mockGoalStore) SkillList() ([]memory.Skill, error)            { return nil, nil }

// A condition trigger wakes a session with a turn nobody waits on. Its reply must
// not block the worker: the session has to answer the next turn, and stop.
func TestWokenSessionKeepsTakingTurns(t *testing.T) {
	var calls atomic.Int32
	provider := llm.ProviderFunc(func(_ context.Context, _ llm.Request) (llm.Response, error) {
		if calls.Add(1) == 1 {
			return finalResp("handled the finding"), nil
		}
		return finalResp("still here"), nil
	})
	d, sock := startDaemon(t, makeFactory(provider), nil, nil)
	c := dial(t, sock)

	id, err := c.NewConversation()
	if err != nil {
		t.Fatalf("NewConversation: %v", err)
	}
	if !d.WakeAgent(id, "the predicate found something") {
		t.Fatal("WakeAgent declined an idle session")
	}
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	reply := make(chan string, 1)
	go func() {
		resp, err := c.Turn(id, "anything new?")
		if err != nil {
			resp = "error: " + err.Error()
		}
		reply <- resp
	}()
	select {
	case got := <-reply:
		if got != "still here" {
			t.Errorf("reply = %q, want %q", got, "still here")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the session never answered after a woken turn: its worker is blocked")
	}
}

// fakeGenerated records deletes for the operator's tool_delete; "built_in"
// stands for a tool Nine did not write.
type fakeGenerated struct{ deleted []string }

func (f *fakeGenerated) Write(context.Context, agent.GeneratedToolSpec) (agent.WriteResult, error) {
	return agent.WriteResult{}, nil
}
func (f *fakeGenerated) Eval(context.Context, string, json.RawMessage, json.RawMessage) (string, error) {
	return "", nil
}
func (f *fakeGenerated) Delete(_ context.Context, name string) error {
	if name == "built_in" {
		return fmt.Errorf("tool %q is built into Nine and cannot be deleted", name)
	}
	f.deleted = append(f.deleted, name)
	return nil
}

// The operator's delete goes through the generated-tool store, so it deletes
// what the model's tool_delete would and refuses what it would refuse.
func TestOperatorToolDelete(t *testing.T) {
	d, sock := startDaemon(t, makeFactory(seqProvider(nil)), nil, nil)
	c := dial(t, sock)

	if _, err := c.DeleteTool("mine"); err == nil || !strings.Contains(err.Error(), "tool writing is off") {
		t.Errorf("with tool writing off: err = %v", err)
	}

	g := &fakeGenerated{}
	d.ConfigureGeneratedTools(g)
	out, err := c.DeleteTool("mine")
	if err != nil || !strings.Contains(out, "deleted") {
		t.Fatalf("DeleteTool(mine) = %q, %v", out, err)
	}
	if len(g.deleted) != 1 || g.deleted[0] != "mine" {
		t.Errorf("store deletes = %v, want [mine]", g.deleted)
	}
	if _, err := c.DeleteTool("built_in"); err == nil || !strings.Contains(err.Error(), "cannot be deleted") {
		t.Errorf("deleting a built-in: err = %v, want a refusal", err)
	}
}

// A process's turn runs in its own session, created on first use and reused
// afterwards: the second turn's request carries the first exchange.
func TestProcessTurnRunsInTheProcessSession(t *testing.T) {
	var mu sync.Mutex
	var requests []llm.Request
	provider := llm.ProviderFunc(func(_ context.Context, req llm.Request) (llm.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, req)
		return finalResp(fmt.Sprintf("reply %d", len(requests))), nil
	})
	d, _ := startDaemon(t, makeFactory(provider), nil, nil)

	for i, want := range []string{"reply 1", "reply 2"} {
		got, _, err := d.ProcessTurn(context.Background(), "digest-session", runtime.RoleParams{}, "tick", "idle")
		if err != nil || got != want {
			t.Fatalf("ProcessTurn %d = %q, %v; want %q", i+1, got, err, want)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	last := requests[len(requests)-1]
	var sawFirstReply bool
	for _, m := range last.Messages {
		if m.Role == "assistant" && m.Text == "reply 1" {
			sawFirstReply = true
		}
	}
	if !sawFirstReply {
		t.Error("the second turn did not see the first: the session was not reused")
	}
}

// A goal-bound process session that stalls — turns calling no tool — pauses its
// goal, as the pursue routine did.
func TestStalledGoalSessionPausesItsGoal(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.GoalCreate("g1", "keep notes tidy", "", "operator"); err != nil {
		t.Fatal(err)
	}
	provider := seqProvider(nil) // every turn answers "done", calling no tool
	d := runtime.New("", runtime.InternalAgent{
		Build: makeFactory(provider),
		Stall: runtime.StallConfig{Limit: 2},
	}, nil)
	d.ConfigureProcesses(store, nil)

	for i := 0; i < 2; i++ {
		if _, _, err := d.ProcessTurn(context.Background(), "g1", runtime.RoleParams{OwnsGoal: true}, "tick", "idle"); err != nil {
			t.Fatal(err)
		}
	}
	g, err := store.GoalGet("g1")
	if err != nil || g == nil || g.Status != "paused" {
		t.Errorf("goal after a stall = %+v, %v; want paused", g, err)
	}
}
