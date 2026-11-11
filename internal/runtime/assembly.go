package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"nine/internal/agent"
	"nine/internal/config"
	"nine/internal/embed"
	"nine/internal/llm"
	"nine/internal/memory"
	"nine/internal/plugin"
	"nine/internal/selfmodel"
	"nine/internal/sqldb"
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
	// Core is the dispatcher the daemon's plugin_call path and the process
	// runner's tool caller share; tests can register fixture tools on it.
	Core *agent.Dispatcher
	// Processes runs every process.
	Processes *StandingRunner
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

	// Loop / builder behavior.
	ContextBudget int
	// MaxTokens caps one reply of an agent loop's call ([llm] max_tokens);
	// 0 keeps the builder's default.
	MaxTokens    int
	SystemPrompt string

	// Runtime describes where the daemon is running (config.RuntimeLabel), shown
	// to the model in the self-model's Environment block. Empty omits the line —
	// which is what the eval harness wants, since a case's environment is the
	// harness's, not the operator's.
	Runtime         string
	RelatedSessions bool
	SurfaceMemories bool
	// MaxToolOutputTokens mirrors [tools] max_output_tokens; 0 keeps the default.
	MaxToolOutputTokens int
	// ToolExposition, ResidentTools, PromotedBudget, HybridPicks and ToolTopN
	// mirror [context].
	ToolExposition string
	ResidentTools  []string
	PromotedBudget int
	HybridPicks    int
	ToolTopN       int
	// MaxJobsPerConversation mirrors [plugins] max_jobs_per_conversation; 0 default.
	MaxJobsPerConversation int
	// MaxJobsTotal mirrors [plugins] max_jobs_total; 0 default.
	MaxJobsTotal int
	// JobWaiters is shared with the job sweeper so job_wait blocks on completion
	// signals rather than polling. Production passes one; the harness leaves it nil.
	JobWaiters         *JobWaiters
	Queue              *llm.Queue
	TaskTimeoutSeconds int
	HITL               *HITL
	ApprovalTools      []string
	// Workspace is the index over [workspace].root, or nil when unset.
	Workspace agent.WorkspaceBackend
	// WorkspaceRoot is [workspace].root itself, for the ref resolver.
	WorkspaceRoot      string
	GateSubAgents      bool
	PlanApproval       string
	PlanMode           string
	DefaultLeafRole    string
	MaxDelegationDepth int
	MaxRunning         int // [processes] max_running
	// ProcessBudget is [processes] budget; zero fields take the defaults.
	ProcessBudget config.BudgetConfig
	// MaxDepth is [processes] max_depth; <= 0 is the default.
	MaxDepth int
	// Board is the [board] table (adr/orchestrator-board.md §2.4); zero fields
	// take the defaults.
	Board config.BoardConfig
	// Database is the database: written by live processes through nine:sql,
	// read through sql_query. Nil leaves both out.
	Database *sqldb.DB
	// JobMinDelayMS and JobWorkers bound the process runner's calls, as they do
	// the job sweeper's ([tools] job_min_delay_ms, job_workers); 0 is the default.
	JobMinDelayMS int
	JobWorkers    int

	// RoleFactory optionally decorates the builder's BuildForRole before it is
	// handed to the daemon. Production passes nil (BuildForRole is used as-is);
	// the eval harness passes a wrapper that forces a case's declared role. The
	// argument is the builder's own BuildForRole.
	RoleFactory func(LoopFactory) LoopFactory
}

// processToolCaller runs a standing tool's tick: Go builtins through the core
// dispatcher, MCP plugin tools through the plugin manager.
type processToolCaller struct {
	core *agent.Dispatcher
	mgr  *plugin.Manager
}

// CallProcessTool implements ProcessToolCaller.
func (c processToolCaller) CallProcessTool(ctx context.Context, tool string, args json.RawMessage) (string, error) {
	if c.core != nil && c.core.Has(tool) {
		cr, err := c.core.Dispatch(ctx, tool, args)
		if err != nil {
			return "", err
		}
		return cr.Output, nil
	}
	if c.mgr != nil {
		for _, p := range c.mgr.Running() {
			for _, t := range p.Tools {
				if t.Name == tool {
					cr, err := c.mgr.Call(ctx, p, tool, args)
					if err != nil {
						return "", err
					}
					return cr.Output, nil
				}
			}
		}
	}
	return "", fmt.Errorf("unknown tool: %s", tool)
}

// Assemble builds and wires the daemon core from c and returns it stopped. The
// caller starts the supervisor and the daemon, and (in production) layers on the
// bootstrap Assembly documents as intentionally omitted.
func Assemble(c AssemblyConfig) *Assembly {
	ckpt, notif, notifAdd := NewStores(c.Store)

	// The human-facing notification feed. Shared by notify_user and by a capability
	// request, which is the one message an operator must see even though no session
	// of theirs produced it.
	notifyUser := func(agentID, text string) {
		if err := c.Store.UserNotificationCreate(memory.NewID(), agentID, text); err != nil {
			slog.Warn("post user notification", "agent_id", agentID, "err", err)
		}
	}

	supervisor := NewSupervisor(64)
	supervisor.Attach(c.Store)

	assembler := selfmodel.New(c.Store, c.Embedder, c.Plugins.ListRunning, c.Runtime)

	builder := NewAgentBuilder(AgentBuilderConfig{
		Loop: LoopConfig{
			Mgr:                    c.Plugins,
			Embedder:               c.Embedder,
			Memory:                 c.Store,
			ContextBudget:          c.ContextBudget,
			MaxTokens:              c.MaxTokens,
			SystemPrompt:           c.SystemPrompt,
			Assembler:              assembler,
			RelatedSessions:        c.RelatedSessions,
			SurfaceMemories:        c.SurfaceMemories,
			MaxToolOutputTokens:    c.MaxToolOutputTokens,
			MaxJobsPerConversation: c.MaxJobsPerConversation,
			MaxJobsTotal:           c.MaxJobsTotal,
			JobWaiters:             c.JobWaiters,
			Workspace:              c.Workspace,
			WorkspaceRoot:          c.WorkspaceRoot,
			ToolExposition:         c.ToolExposition,
			ResidentTools:          c.ResidentTools,
			PromotedBudget:         c.PromotedBudget,
			HybridPicks:            c.HybridPicks,
			ToolTopN:               c.ToolTopN,
		},
		InitialQueue: c.Queue,
		NotifAdd:     notifAdd,
		NotifyUser:   notifyUser,
		// The path out of a ceiling refusal (capability_grants.go). Wired from the
		// same store and the same notification feed, so a request an agent makes
		// while nobody is watching still reaches `nine notifications`.
		RequestCapability:  NewCapabilityRequester(c.Store, notifyUser),
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
	// to the factory (adr/roles-design.md §6). RoleFactory lets a caller intercept that
	// — the harness forces a case's role before delegating to BuildForRole.
	factory := LoopFactory(builder.BuildForRole)
	if c.RoleFactory != nil {
		factory = c.RoleFactory(builder.BuildForRole)
	}

	daemon := New(c.SocketPath, InternalAgent{
		Build:         factory,
		Notifications: notif,
		Supervisor:    supervisor,
		RoleInfos:      builder.RoleInfos,
	}, ckpt)

	// Durable session-event journal (adr/event-log.md): every new session worker
	// writes its full execution trajectory through this async batched sink.
	sink := NewSQLEventSink(c.Store, daemon.NotifySubscribers)
	// Every journal write, the sink's and the store's own state changes, wakes
	// the daemon's journal followers.
	c.Store.OnJournal(daemon.JournalWritten)
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
	core := builder.CoreDispatcher()
	daemon.ConfigureCoreTools(core)
	daemon.SetMaxRunning(c.MaxRunning)
	daemon.ConfigureWorkspace(c.WorkspaceRoot)
	daemon.ConfigureDatabase(c.Database)

	// The process runner (adr/process-sessions.md): every process — goal
	// sessions, standing agents, self-reflection, standing tools — runs through
	// it, so production and the eval harness share it; the caller starts it with
	// RunStandingTools.
	runner := NewStandingRunner(c.Store, c.JobMinDelayMS, c.JobWorkers)
	runner.SetToolCaller(processToolCaller{core: core, mgr: c.Plugins})
	runner.SetWaker(daemon)
	runner.SetSessions(daemon)
	runner.SetBudget(c.ProcessBudget)
	runner.SetMaxRunning(c.MaxRunning)
	runner.SetMaxDepth(c.MaxDepth)
	runner.SetDatabase(c.Database)
	// The generated-tool store and the runner: deleting a tool drops its
	// activity ring, a rewritten process restarts, a failing one rolls back.
	if c.Database != nil {
		builder.SetSQLReader(sqlReader{c.Database})
	}
	if runner != nil {
		builder.SetProcessControl(modelProcesses{r: runner})
		// Event triggers: a journal subscriber that delivers events to the live
		// processes they trigger (process_events.go).
		daemon.AddSubscriber(runner.EventRouter())
	}
	// The board tools (adr/orchestrator-board.md §2.1): store-backed control,
	// granted per role by the builder's filterByRole. Finished posted/detached
	// tasks wake their poster (restricted condition turn, §2.3); a gone poster
	// gets the human feed via the runner's notification path.
	if c.Store != nil {
		bc := boardControl{
			store:     c.Store,
			maxDepth:  c.MaxDepth,
			maxQueued: c.Board.MaxQueuedOrDefault(),
			wake:      daemon.WakeAgent,
		}
		if runner != nil {
			bc.feed = runner.notifyHuman
			runner.SetBoard(&bc)
		}
		builder.SetBoardControl(bc)
	}
	// agent_stop (§2.4) runs over the daemon's session stop.
	builder.SetSessionControl(sessionControl{daemon: daemon})
	daemon.ConfigureStandingTools(runner)
	daemon.ConfigureProcesses(c.Store, runner.Wake)

	// goal_create spawns a background pursue process for each new top-level goal
	// (docs/goal-sessions.md).
	builder.SetGoalSessionSpawnFn(daemon.SpawnGoalSession)
	builder.SetEmitProgressFn(daemon.EmitProgress)

	return &Assembly{
		Daemon:     daemon,
		Builder:    builder,
		Supervisor: supervisor,
		Assembler:  assembler,
		EventSink:  sink,
		Core:       core,
		Processes:  runner,
	}
}

// sqlReader is sql_query's view of the database.
type sqlReader struct{ db *sqldb.DB }

func (r sqlReader) Query(ctx context.Context, q string, args ...any) (any, error) {
	return r.db.Query(ctx, q, args...)
}
