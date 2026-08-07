package runtime

import (
	"log/slog"

	"nine/internal/embed"
	"nine/internal/llm"
	"nine/internal/memory"
	"nine/internal/plugin"
	"nine/internal/selfmodel"
	"nine/internal/toolvm"
)

// Assembly is the wired-but-not-started daemon core shared by the production
// entrypoint (cmd/nine/daemon.go) and the in-process eval harness
// (tests/evals/runner/harness.go). Both construct the same stores, supervisor,
// self-model assembler, agent builder, daemon, and event sink over an injected
// store / plugin manager / provider queue; Assemble centralizes that wiring so
// the harness cannot silently drift from production (docs/evals.md §5).
//
// Assemble deliberately does NOT perform the production-only bootstrap the
// harness omits by design — resume, standing agents, self-reflection,
// journal/workflow scrub, instance-name resolution, or out-of-band subscribers.
// Those stay in cmd/nine/daemon.go and run against the returned Daemon.
type Assembly struct {
	Daemon     *Daemon
	Builder    *AgentBuilder
	Supervisor *Supervisor
	Assembler  *selfmodel.Assembler
	EventSink  EventSink
}

// AssemblyConfig carries the inputs to Assemble. Values the production daemon and
// the eval harness legitimately source differently (nine.toml vs. an eval Case)
// are fields here; the wiring that must stay identical between the two lives in
// Assemble. See LoopConfig and AgentBuilderConfig for the behavioral knobs.
type AssemblyConfig struct {
	SocketPath string
	Store      *memory.Store
	Plugins    *plugin.Manager
	Embedder   embed.Embedder

	// Tools is the sandboxed-tool host (spec/contracts/toolvm.md). Nil when
	// [tools] enabled is unset, which is the default — every loop then builds
	// exactly the tool set it did before the host existed.
	Tools *toolvm.Host

	// Loop / builder behavior.
	ContextBudget   int
	SystemPrompt    string
	RelatedSessions bool
	SurfaceMemories bool
	// MaxToolOutputTokens mirrors [tools] max_output_tokens; 0 keeps the default.
	MaxToolOutputTokens int
	// MaxJobsPerConversation mirrors [plugins] max_jobs_per_conversation; 0 default.
	MaxJobsPerConversation int
	// JobWaiters is shared with the job sweeper so job_wait blocks on completion
	// signals rather than polling. Production passes one; the harness leaves it nil.
	JobWaiters         *JobWaiters
	Queue              *llm.Queue
	TaskTimeoutSeconds int
	HITL               *HITL
	ApprovalTools      []string
	GateSubAgents      bool
	PlanApproval       string
	PlanMode           string
	DefaultLeafRole    string
	MaxDelegationDepth int
	MaxGoalSessions    int

	// RoleFactory optionally decorates the builder's BuildForRole before it is
	// handed to the daemon. Production passes nil (BuildForRole is used as-is);
	// the eval harness passes a wrapper that forces a case's declared role. The
	// argument is the builder's own BuildForRole.
	RoleFactory func(LoopFactory) LoopFactory
}

// Assemble builds and wires the daemon core from c and returns it stopped. The
// caller starts the supervisor and the daemon, and (in production) layers on the
// bootstrap Assembly documents as intentionally omitted.
func Assemble(c AssemblyConfig) *Assembly {
	ckpt, notif, notifAdd := NewStores(c.Store)

	supervisor := NewSupervisor(64)
	supervisor.Attach(c.Store)

	assembler := selfmodel.New(c.Store, c.Embedder, c.Plugins.ListRunning)

	builder := NewAgentBuilder(AgentBuilderConfig{
		Loop: LoopConfig{
			Mgr:                    c.Plugins,
			Embedder:               c.Embedder,
			Memory:                 c.Store,
			ContextBudget:          c.ContextBudget,
			SystemPrompt:           c.SystemPrompt,
			Assembler:              assembler,
			RelatedSessions:        c.RelatedSessions,
			SurfaceMemories:        c.SurfaceMemories,
			MaxToolOutputTokens:    c.MaxToolOutputTokens,
			MaxJobsPerConversation: c.MaxJobsPerConversation,
			JobWaiters:             c.JobWaiters,
			Tools:                  c.Tools,
		},
		InitialQueue: c.Queue,
		NotifAdd:     notifAdd,
		NotifyUser: func(agentID, text string) {
			if err := c.Store.UserNotificationCreate(memory.NewID(), agentID, text); err != nil {
				slog.Warn("post user notification", "agent_id", agentID, "err", err)
			}
		},
		Sup:                supervisor,
		TaskTimeoutSeconds: c.TaskTimeoutSeconds,
		HITL:               c.HITL,
		ApprovalTools:      c.ApprovalTools,
		GateSubAgents:      c.GateSubAgents,
		PlanApproval:       c.PlanApproval,
		PlanMode:           c.PlanMode,
		DefaultLeafRole:    c.DefaultLeafRole,
		MaxDelegationDepth: c.MaxDelegationDepth,
	})

	// The daemon resolves each session's role from its plan profile and passes it
	// to the factory (docs/roles.md §6). RoleFactory lets a caller intercept that
	// — the harness forces a case's role before delegating to BuildForRole.
	factory := LoopFactory(builder.BuildForRole)
	if c.RoleFactory != nil {
		factory = c.RoleFactory(builder.BuildForRole)
	}

	daemon := New(c.SocketPath, factory, ckpt, notif)

	// Durable session-event journal (docs/event-log.md): every new session worker
	// writes its full execution trajectory through this async batched sink.
	sink := NewSQLEventSink(c.Store, daemon.NotifySubscribers)
	daemon.SetEventSink(sink)
	// Sub-agents journal through the same sink under their own IDs, so a
	// delegated sub-agent's trajectory is persisted and `nine trace --sub-agents`
	// can nest it beneath the parent.
	builder.SetEventSink(sink)

	// ask_human emits questions onto the asking session's progress stream; the
	// daemon resolves session interactivity for HITL on attach.
	if c.HITL != nil {
		c.HITL.SetEmit(daemon.EmitProgress)
		daemon.ConfigureHITL(c.HITL)
	}

	daemon.SetSubAgentLister(builder.SubAgents)
	daemon.SetQueueStatFn(builder.QueueDepth)
	daemon.ConfigureMemory(c.Store)
	daemon.ConfigurePlugins(c.Plugins)
	daemon.ConfigureSandboxedTools(c.Tools)
	daemon.ConfigureCoreTools(builder.CoreDispatcher())
	daemon.ConfigureSupervisor(supervisor)
	daemon.ConfigurePlanStore(c.Store)
	daemon.SetMaxGoalSessions(c.MaxGoalSessions)

	// goal_create spawns a background pursue session for each new top-level goal
	// (docs/goal-sessions.md); the stage handler closes over c.Store, so callers
	// that share process state (the eval runner) must serialize runs.
	StageRegistry["pursue"] = func() StageHandler {
		return NewPursueStage(c.Store)
	}
	builder.SetGoalSessionSpawnFn(daemon.SpawnGoalSession)
	builder.SetEmitProgressFn(daemon.EmitProgress)

	return &Assembly{
		Daemon:     daemon,
		Builder:    builder,
		Supervisor: supervisor,
		Assembler:  assembler,
		EventSink:  sink,
	}
}
