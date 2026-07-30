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
	// Resolve per-plugin spawn env (built-in defaults + operator settings) by name.
	// Built-ins pass it explicitly below; user plugins reach it through the
	// manager (docs/plugin-capabilities.md §3).
	pluginManager.SetPluginEnv(cfg.PluginEnvs)
	pluginManager.TryStart("files", cfg.PluginEnvs("files")...)
	pluginManager.TryStart("shell", cfg.PluginEnvs("shell")...)
	pluginManager.TryStart("http", cfg.PluginEnvs("http")...)
	pluginManager.TryStart("time", cfg.PluginEnvs("time")...)
	browserPlug := pluginManager.TryStart("browser", cfg.PluginEnvs("browser")...)

	// Load operator-supplied plugins from [plugins].user_dir, after the built-ins
	// so their tools are reserved and a colliding user plugin is skipped (not
	// allowed to override). An invalid or colliding user plugin is surfaced and
	// skipped; the daemon still boots. Absent/empty dir is a no-op.
	pluginManager.LoadUserPlugins(cfg.Plugins.UserDir)

	embedder := embed.Build(cfg.Embeddings.Provider, cfg.Embeddings.Model, cfg.Embeddings.Endpoint)

	// Seed built-in skills from the binary into the store (immutable; refreshed
	// every boot). Agent-authored skills persist across restarts untouched.
	if err := runtime.SeedSkills(store, embedder); err != nil {
		slog.Warn("seed skills", "err", err)
	}

	// Seed operator-authored skills and roles from [skills].user_dir, after the
	// built-ins so a collision is caught against the full built-in set. Invalid
	// files are skipped with a logged reason; the daemon still boots.
	if err := runtime.SeedUserSkills(store, embedder, cfg.Skills.UserDir); err != nil {
		slog.Warn("seed user skills", "dir", cfg.Skills.UserDir, "err", err)
	}

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

	// Build and wire the daemon core (stores, supervisor, self-model, agent
	// builder, daemon, event sink) shared with the eval harness. Production-only
	// bootstrap — subscribers, standing agents, resume, instance name — is layered
	// on below against the returned daemon (docs/evals.md §5).
	asm := runtime.Assemble(runtime.AssemblyConfig{
		SocketPath:    cfg.SocketPath(),
		Store:         store,
		Plugins:       pluginManager,
		Embedder:      embedder,
		ContextBudget: cfg.ContextBudget(),
		SystemPrompt:  runtime.BuildSystemPrompt(browserPlug != nil),
		// Pull-surface related prior sessions only when the out-of-band indexer
		// that populates the store is enabled.
		RelatedSessions: cfg.Daemon.RelatedSessionsIndexEnabled(),
		// Index and pull-surface stored key-value memories relevant to the turn.
		SurfaceMemories:     cfg.Memory.SurfaceMemoriesEnabled(),
		MaxToolOutputTokens: cfg.Tools.MaxOutputTokens,
		Queue:               cfg.BuildQueue(),
		TaskTimeoutSeconds:  cfg.Daemon.TaskTimeoutSeconds,
		HITL:                hitl,
		ApprovalTools:       cfg.HITL.RequireApproval,
		GateSubAgents:       cfg.HITL.GateSubAgentsEnabled(),
		PlanApproval:        cfg.Planning.PlanApprovalMode(),
		PlanMode:            cfg.Planning.Mode(),
		DefaultLeafRole:     cfg.Roles.DefaultLeaf,
		MaxDelegationDepth:  cfg.Roles.MaxDelegationDepth,
		MaxGoalSessions:     cfg.Daemon.MaxGoalSessions,
	})
	daemon := asm.Daemon
	supervisor := asm.Supervisor
	defer asm.EventSink.Close() //nolint:errcheck // best-effort drain on shutdown

	// Out-of-band subscribers (docs/reactive-events.md): on by default, but a
	// no-op without an embedder and never on the agent loop. Set
	// related_sessions_index = false to disable. Intentionally omitted from the
	// shared assembly and the eval harness — it needs an embedder to be useful.
	if cfg.Daemon.RelatedSessionsIndexEnabled() {
		if embedder == nil {
			slog.Warn("related_sessions_index enabled but no embedder configured; skipping")
		} else {
			daemon.AddSubscriber(subscribers.NewRelatedIndexer(store, embedder))
		}
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Resolve this instance's display name (shown in the TUI top bar): the
	// configured name if set, else a previously generated one from the store,
	// else a placeholder that a background LLM call replaces with a random name
	// and persists (docs/configuration.md). Non-blocking.
	daemon.ResolveInstanceName(ctx, cfg.Daemon.InstanceName, store, cfg.BuildProvider())

	// Start the agent builder's main loop in the background, so it can manage agents while the daemon is running.
	go supervisor.Run(ctx)

	// Spilled tool outputs are session debris: sweep the expired ones on boot
	// and hourly thereafter so large results cannot grow the file store without
	// bound (docs/tool-output-spill.md §5).
	go runtime.RunSpillSweeper(ctx, store)

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
