package runtime

import (
	"context"

	"nine/internal/agent"
	ninectx "nine/internal/context"
	"nine/internal/embed"
	"nine/internal/llm"
	"nine/internal/memory"
	"nine/internal/protocol"
)

// PlanMentionRiskyTool exposes planMentionRiskyTool for testing
var PlanMentionRiskyTool = planMentionRiskyTool

// NameFromPrompt exposes nameFromPrompt for testing.
var NameFromPrompt = nameFromPrompt

// SanitizeInstanceName exposes sanitizeInstanceName for testing.
var SanitizeInstanceName = sanitizeInstanceName

// RandomInstanceName exposes randomInstanceName for testing.
var RandomInstanceName = randomInstanceName

// InstanceNameKV exposes the store key under which a generated instance name
// is persisted, so tests can seed and assert on it.
var InstanceNameKV = instanceNameKV

// ToolVectorForTest exposes the cached per-tool embedding lookup for testing.
func (f *AgentBuilder) ToolVectorForTest(emb embed.Embedder, name, desc string) []float32 {
	return f.toolVector(emb, llm.ToolDef{Name: name, Description: desc})
}

// JournalReplayForTest exposes journalReplay (the journal-backed reattach
// snapshot) for testing.
func (d *Daemon) JournalReplayForTest(agentID string) ([]protocol.Msg, string) {
	return d.journalReplay(agentID)
}

// JournalHistoryForTest exposes journalHistory (the full-transcript reattach
// reconstruction) for testing.
func (d *Daemon) JournalHistoryForTest(agentID string) []protocol.Msg {
	return d.journalHistory(agentID)
}

// PlanNeedsResumeForTest exposes planNeedsResume for testing.
var PlanNeedsResumeForTest = planNeedsResume

// LoadOrCreatePlanForTest exposes loadOrCreatePlan for testing, returning the
// fields tests care about without exposing the unexported sessionPlanState type.
func LoadOrCreatePlanForTest(store PlanStore, agentID string, profile []string, eager bool) (persisted bool, plan memory.SessionPlan, err error) {
	state, err := loadOrCreatePlan(context.Background(), store, agentID, profile, eager)
	if err != nil {
		return false, memory.SessionPlan{}, err
	}
	return state.persisted, *state.plan, nil
}

// NewAgentWorkerWithPlanForTest creates a AgentWorker whose plan is
// loaded/created via loadOrCreatePlan, for testing OnTurnEnd/idle-scheduler
// wiring against custom StageHandlers registered in StageRegistry.
func NewAgentWorkerWithPlanForTest(
	id string,
	loop *agent.Loop,
	saveCkpt func(string, []byte) error,
	getNotif func(string) ([]string, error),
	stall StallConfig,
	store PlanStore,
	profile []string,
	eager bool,
) (*AgentWorkerForTest, error) {
	plan, err := loadOrCreatePlan(context.Background(), store, id, profile, eager)
	if err != nil {
		return nil, err
	}
	return &AgentWorkerForTest{w: newAgentWorker(id, loop, saveCkpt, getNotif, stall, plan, nil)}, nil
}

// AgentWorkerForTest wraps an unexported AgentWorker to allow testing from package daemon_test.
type AgentWorkerForTest struct {
	w *AgentWorker
}

// ActiveGoalSessionCountForTest exposes activeGoalSessionCount for testing.
func (d *Daemon) ActiveGoalSessionCountForTest() int { return d.activeGoalSessionCount() }

// NewAgentWorkerForTest creates a AgentWorker and returns it wrapped for testing.
func NewAgentWorkerForTest(
	id string,
	loop *agent.Loop,
	saveCkpt func(string, []byte) error,
	getNotif func(string) ([]string, error),
	stall StallConfig,
) *AgentWorkerForTest {
	return &AgentWorkerForTest{w: newAgentWorker(id, loop, saveCkpt, getNotif, stall, nil, nil)}
}

// NewAgentWorkerWithSinkForTest creates a AgentWorker wired to sink, for
// asserting on the durable session-event journal.
func NewAgentWorkerWithSinkForTest(id string, loop *agent.Loop, sink EventSink) *AgentWorkerForTest {
	return &AgentWorkerForTest{w: newAgentWorker(id, loop, nil, nil, StallConfig{}, nil, sink)}
}

// NewSQLEventSinkForTest exposes the async batched SQL sink to tests.
func NewSQLEventSinkForTest(store sessionEventStore) EventSink { return NewSQLEventSink(store, nil) }

// TurnAgentWorker delegates to the unexported AgentWorker.turn.
func (st *AgentWorkerForTest) TurnAgentWorker(ctx context.Context, text string) (string, error) {
	return st.w.turn(ctx, text)
}

// StopAgentWorker delegates to the unexported AgentWorker.stop.
func (st *AgentWorkerForTest) StopAgentWorker() { st.w.stop() }

// InspectContextForTest delegates to the unexported AgentWorker.InspectContext.
func (st *AgentWorkerForTest) InspectContextForTest(ctx context.Context) (ninectx.Report, error) {
	return st.w.InspectContext(ctx)
}
