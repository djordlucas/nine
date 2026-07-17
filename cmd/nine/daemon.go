package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"nine/internal/config"
	"nine/internal/embed"
	"nine/internal/memory"
	"nine/internal/plugin"
	"nine/internal/runtime"
	"nine/internal/selfmodel"
	"nine/internal/subscribers"
)

func runDaemon() {
	// Load configuration
	cfg := config.LoadDefault()

	// Initialize memory store
	store, err := memory.Open(cfg.DatabaseURL())
	if err != nil {
		slog.Error("open memory store", "database_url", cfg.DatabaseURL(), "err", err)
		os.Exit(1)
	}

	// Initialize plugin manager and start plugins
	pluginManager := plugin.NewManager(cfg.Plugins.Bin)
	pluginManager.TryStart("files", cfg.PluginEnvs("files")...)
	pluginManager.TryStart("shell")
	pluginManager.TryStart("http")
	pluginManager.TryStart("time")
	browserPlug := pluginManager.TryStart("browser", cfg.PluginEnvs("browser")...)

	// Initialize checkpoint and notification stores
	checkpointStore, notifStore, notifAdd := runtime.NewStores(store)
	embedder := embed.Build(cfg.Embeddings.Provider, cfg.Embeddings.Model, cfg.Embeddings.Endpoint)

	// Seed built-in skills from the binary into the store (immutable; refreshed
	// every boot). Agent-authored skills persist across restarts untouched.
	if err := runtime.SeedSkills(store, embedder); err != nil {
		slog.Warn("seed skills", "err", err)
	}

	// The supervisor manages the execution of agent tasks, and provides a shared context for plugins to use for cancellation and timeouts.
	// It journals its control-plane events durably and consumes them via a
	// cursor-backed subscription, so reactions survive a restart (docs/event-log.md §8a).
	supervisor := runtime.NewSupervisor(64)
	supervisor.Attach(store)

	// Initialize the self-model assembler with a callback to get the current plugin list.
	assembler := selfmodel.New(store, embedder, pluginManager.ListRunning)

	// Bootstrap the self-model with the current plugin list, so it can answer questions about them.
	if err := runtime.BootstrapSelfKV(store, pluginManager.ListRunning()); err != nil {
		slog.Error("bootstrap self KV", "err", err)
	}

	// Register the idle-reflection stage handler and ensure the dedicated
	// self-reflection session's plan exists (one-time; subsequent boots pick
	// it up via daemon.ResumeSessions).
	runtime.StageRegistry["idle-reflection"] = func() runtime.StageHandler {
		return runtime.NewIdleReflectionStage(store)
	}
	if err := runtime.BootstrapSelfReflection(store, 2*time.Minute); err != nil {
		slog.Error("bootstrap self-reflection session", "err", err)
	}

	// Register the pursue stage handler (docs/goal-sessions.md): each
	// top-level goal gets its own background session running this stage.
	runtime.StageRegistry["pursue"] = func() runtime.StageHandler {
		return runtime.NewPursueStage(store)
	}

	// Scrub old workflows on startup, to prevent unbounded growth of the workflow store.
	if _, err := store.WorkflowScrub(); err != nil {
		slog.Warn("workflow scrub", "err", err)
	}

	// Bound the session_events journal on startup (docs/event-log.md v4): keep the
	// last N turns per agent (and optionally drop events older than M days) so the
	// verbose execution journal cannot grow without limit.
	if turns, age := cfg.EventRetention(); turns > 0 || age > 0 {
		if n, err := store.SessionEventsScrub(turns, age); err != nil {
			slog.Warn("session events scrub", "err", err)
		} else if n > 0 {
			slog.Info("scrubbed session events", "deleted", n, "keep_turns", turns)
		}
	}

	// Human-in-the-loop coordinator: bridges blocking ask_human calls and
	// human_input_answer messages. Stale pending requests from a prior run are
	// expired on boot (R-HITL.4).
	hitl := runtime.NewHITL(store, cfg.HITLTimeout())
	if err := hitl.ExpireStale(); err != nil {
		slog.Warn("expire stale human requests", "err", err)
	}

	// Build the agent builder with all dependencies, and start the daemon.
	agentBuilder := runtime.NewAgentBuilder(runtime.AgentBuilderConfig{
		Loop: runtime.LoopConfig{
			Mgr:           pluginManager,
			Embedder:      embedder,
			Memory:        store,
			ContextBudget: cfg.ContextBudget(),
			SystemPrompt:  runtime.BuildSystemPrompt(browserPlug != nil),
			Assembler:     assembler,
			// Pull-surface related prior sessions on later turns only when the
			// out-of-band indexer that populates the store is enabled.
			RelatedSessions: cfg.Daemon.RelatedSessionsIndexEnabled(),
		},
		InitialQueue: cfg.BuildQueue(),
		NotifAdd:     notifAdd,
		NotifyUser: func(agentID, text string) {
			if err := store.UserNotificationCreate(memory.NewID(), agentID, text); err != nil {
				slog.Warn("post user notification", "agent_id", agentID, "err", err)
			}
		},
		Sup:                supervisor,
		TaskTimeoutSeconds: cfg.Daemon.TaskTimeoutSeconds,
		HITL:               hitl,
		ApprovalTools:      cfg.HITL.RequireApproval,
		PlanApproval:       cfg.Planning.PlanApprovalMode(),
		PlanMode:           cfg.Planning.Mode(),
		DefaultLeafRole:    cfg.Roles.DefaultLeaf,
		MaxDelegationDepth: cfg.Roles.MaxDelegationDepth,
	})

	// Wire up the runtime with the agent builder, stores, and plugins. The
	// daemon resolves each session's role from its plan profile and passes it
	// to the factory (docs/roles.md §6).
	daemon := runtime.New(cfg.SocketPath(), agentBuilder.BuildForRole, checkpointStore, notifStore)

	// Durable session-event journal (docs/event-log.md): every new session
	// worker writes its full execution trajectory — turn boundaries, exact LLM
	// request/response, tool I/O — through this async batched sink.
	eventSink := runtime.NewSQLEventSink(store, daemon.NotifySubscribers)
	defer eventSink.Close() //nolint:errcheck // best-effort drain on shutdown
	daemon.SetEventSink(eventSink)

	// ask_human emits questions onto the asking session's progress stream;
	// the daemon resolves session interactivity for HITL on attach.
	hitl.SetEmit(daemon.EmitProgress)
	daemon.ConfigureHITL(hitl)

	// The self-model needs to be able to list plugins for reflection and question-answering, so we provide it with a callback that returns the current plugin list.
	// Sub-agents are a special case since they're not real plugins, but we still want them to be discoverable and show up in the self-model.
	daemon.SetSubAgentLister(agentBuilder.SubAgents)
	daemon.SetQueueStatFn(agentBuilder.QueueDepth)

	// Wire the plugins with the daemon, so they can be used in agents and show up in the self-model.
	daemon.ConfigureMemory(store)
	daemon.ConfigurePlugins(pluginManager)
	daemon.ConfigureSupervisor(supervisor)
	daemon.ConfigurePlanStore(store)
	daemon.SetMaxGoalSessions(cfg.Daemon.MaxGoalSessions)

	// Out-of-band subscribers (docs/reactive-events.md): on by default, but a
	// no-op without an embedder and never on the agent loop. Set
	// related_sessions_index = false to disable. Must follow ConfigureMemory — a
	// subscriber needs the store to read the journal.
	if cfg.Daemon.RelatedSessionsIndexEnabled() {
		if embedder == nil {
			slog.Warn("related_sessions_index enabled but no embedder configured; skipping")
		} else {
			daemon.AddSubscriber(subscribers.NewRelatedIndexer(store, embedder))
		}
	}

	// goal_create spawns a background pursue session for each new top-level
	// goal (docs/goal-sessions.md); only depth-0 loops get this wired.
	agentBuilder.SetGoalSessionSpawnFn(daemon.SpawnGoalSession)

	// Sub-agent lifecycle events (run_agent/run_agents) stream to the
	// spawning conversation's progress feed, so the TUI shows delegated work
	// in flight instead of going silent until the tool call returns.
	agentBuilder.SetEmitProgressFn(daemon.EmitProgress)

	ctx, cancel := context.WithCancel(context.Background())

	// Start the agent builder's main loop in the background, so it can manage agents while the daemon is running.
	go supervisor.Run(ctx)

	// Reconcile pre-defined agents declared in nine.toml: seed a config-owned
	// goal + pursue shell for each, and bring existing ones' definitions in line
	// with the file (docs/predefined-agents.md). Must run after the daemon is
	// fully wired and before ResumeSessions (which revives what this seeds).
	reconcileStandingAgents(ctx, store, daemon, cfg.Agents, cfg.Daemon.StandingAgentsAuthoritative)

	// Resume any session (e.g. a goal's pursue session, once that lands) whose
	// plan was active with an idle-capable stage when the daemon last stopped.
	if err := daemon.ResumeSessions(ctx); err != nil {
		slog.Warn("resume sessions", "err", err)
	}

	// Start the daemon, and log any errors. The daemon will run until the process is killed.
	slog.Info("starting the nine daemon", "socket", cfg.SocketPath())
	if err := daemon.Start(ctx); err != nil {
		cancel()
		slog.Error("daemon error", "err", err)
		os.Exit(1)
	}
	cancel()
}
