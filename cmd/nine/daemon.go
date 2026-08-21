package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"nine/internal/builtins"
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
	dbPath, err := cfg.DatabasePath()
	if err != nil {
		slog.Error("resolve memory store path", "err", err)
		os.Exit(1)
	}
	store, err := memory.Open(dbPath)
	if err != nil {
		slog.Error("open memory store", "path", dbPath, "err", err)
		os.Exit(1)
	}
	// Closing checkpoints the write-ahead log, so the database file is left
	// self-contained rather than depending on its -wal sidecar to be complete.
	defer store.Close() //nolint:errcheck

	// Shared between the job sweeper and every worker's job_wait, so a completing
	// job wakes its waiters at once instead of each polling (§5).
	jobWaiters := runtime.NewJobWaiters()

	// Initialize plugin manager and start plugins
	pluginManager := plugin.NewManager(cfg.Plugins.Bin)
	// Resolve per-plugin spawn env (built-in defaults + operator settings) by name.
	// Built-ins pass it explicitly below; user plugins reach it through the
	// manager (docs/plugin-capabilities.md §3).
	pluginManager.SetPluginEnv(cfg.PluginEnvs)
	// Per-plugin cache dirs (docs/plugin-capabilities.md §4). Sweep leftover
	// ephemeral dirs from a previous daemon that exited without stopping its
	// plugins, before any new plugin allocates one.
	pluginManager.SetCacheConfig(cfg.PluginCacheRoot(), cfg.PluginPersistCache)
	// Plugins the operator switched off ([plugins].disabled, R-PLUG.14). Set
	// before any start so nothing disabled is ever spawned, not even briefly.
	pluginManager.SetDisabled(cfg.Plugins.Disabled)
	pluginManager.SweepCache()
	// The Go built-ins are served by this same binary (`nine plugin serve <name>`,
	// internal/builtins) — still one process each, just no separate artifact to
	// ship or keep in protocol lockstep. Nothing else starts by name: a capability
	// Nine does not implement itself arrives as an [[mcp.server]] below, browser
	// automation included (docs/browser.md).
	for _, name := range builtins.AutoStart() {
		pluginManager.TryStartBuiltin(name, cfg.PluginEnvs(name)...)
	}

	// One MCP server, one plugin. Each [[mcp.server]] gets its own `mcp` bridge
	// instance (R-PLUG.15), so an MCP server has the same failure domain and the
	// same operator controls as any other plugin: it crashes alone, it shows up
	// in `nine plugins` under its own name, and [plugins].disabled switches it
	// off by that name. Started before user plugins so a user plugin colliding
	// with an MCP tool is the one skipped.
	startMCPServers(pluginManager, cfg)

	// Load operator-supplied plugins from [plugins].user_dir, after the built-ins
	// so their tools are reserved and a colliding user plugin is skipped (not
	// allowed to override). An invalid or colliding user plugin is surfaced and
	// skipped; the daemon still boots. Absent/empty dir is a no-op.
	pluginManager.LoadUserPlugins(cfg.Plugins.UserDir)

	// Every plugin has now had its chance to start, so any [plugins].disabled
	// entry that refused nothing is a name that matched nothing — a typo, or a
	// plugin that is not installed. Silence there is the dangerous outcome:
	// `disabled = ["shel"]` withholds nothing and leaves `shell` running while
	// the operator believes it is off. Warn rather than fail, since the name may
	// legitimately belong to a user plugin they have not deployed yet.
	if unmatched := pluginManager.UnmatchedDisabled(); len(unmatched) > 0 {
		slog.Warn("[plugins].disabled names no plugin that exists; these are NOT disabled because nothing by that name was found",
			"names", unmatched, "loaded", pluginManager.ListRunning())
	}

	// Sandboxed tools (spec/contracts/toolvm.md), after the plugins so their tool
	// names are already reserved and a colliding sandboxed tool is skipped rather
	// than allowed to override. nil when [tools] enabled is unset, which is the
	// default and leaves every loop exactly as it was.
	toolHost := runtime.OpenSandboxedTools(context.Background(), cfg, store, pluginManager)
	if toolHost != nil {
		defer toolHost.Close(context.Background()) //nolint:errcheck // best-effort on shutdown
	}
	// The generated tier's write/delete/eval backend (docs/sandboxed-tools.md §5.2),
	// or nil when `[tools.agent]` is off — in which case the meta-tools are neither
	// registered nor advertised. The deps bundler resolves external npm imports at
	// write time (§4.4); nil when [tools.agent.deps] is off.
	generatedTools := runtime.NewGeneratedToolStore(store, toolHost, pluginManager,
		runtime.NewDepsBundler(cfg), cfg.Tools.Agent.AllowNetworkDeps)

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

	// Index the docs and spec embedded in this binary so Nine can retrieve its
	// own manual on demand (docs/self-documentation.md). Fingerprinted, so this
	// is a no-op on every boot that does not change the binary or the embedder.
	if err := runtime.SeedDocs(store, embedder, cfg.Embeddings.Provider+"/"+cfg.Embeddings.Model); err != nil {
		slog.Warn("seed docs index", "err", err)
	}

	// Bootstrap the self-model with the current plugin list, so it can answer questions about them.
	if err := runtime.BootstrapSelfKV(store, pluginManager.ListRunning()); err != nil {
		slog.Error("bootstrap self KV", "err", err)
	}

	// Register the idle-reflection routine handler, then reconcile the dedicated
	// self-reflection session against config — creating it, or deactivating it
	// when the operator has turned reflection off. Later boots pick a live one up
	// via daemon.ResumeSessions.
	runtime.RoutineRegistry["idle-reflection"] = func() runtime.RoutineHandler {
		return runtime.NewIdleReflectionRoutine()
	}
	if err := runtime.ReconcileSelfReflection(store, cfg.SelfReflectionInterval()); err != nil {
		slog.Error("reconcile self-reflection session", "err", err)
	}

	// Scrub old workflows on startup, to prevent unbounded growth of the workflow store.
	if _, err := store.WorkflowScrub(); err != nil {
		slog.Warn("workflow scrub", "err", err)
	}

	// Bound the session_events journal on startup (adr/event-log.md v4): keep the
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
		SocketPath:        cfg.SocketPath(),
		Store:             store,
		Plugins:           pluginManager,
		Tools:             toolHost,
		GeneratedTools:    generatedTools,
		GeneratedEval:     cfg.Tools.Agent.Eval,
		GeneratedApproval: cfg.Tools.Agent.ApprovalMode(),
		Embedder:          embedder,
		ContextBudget:     cfg.ContextBudget(),
		SystemPrompt:      runtime.BuildSystemPrompt(),
		Runtime:           cfg.RuntimeLabel(),
		// Pull-surface related prior sessions only when the out-of-band indexer
		// that populates the store is enabled.
		RelatedSessions: cfg.Daemon.RelatedSessionsIndexEnabled(),
		// Index and pull-surface stored key-value memories relevant to the turn.
		SurfaceMemories:        cfg.Memory.SurfaceMemoriesEnabled(),
		MaxToolOutputTokens:    cfg.Tools.MaxOutputTokens,
		MaxJobsPerConversation: cfg.Plugins.MaxJobsPerConversation,
		JobWaiters:             jobWaiters,
		Queue:                  cfg.BuildQueue(),
		TaskTimeoutSeconds:     cfg.Daemon.TaskTimeoutSeconds,
		HITL:                   hitl,
		ApprovalTools:          cfg.HITL.RequireApproval,
		GateSubAgents:          cfg.HITL.GateSubAgentsEnabled(),
		PlanApproval:           cfg.Planning.PlanApprovalMode(),
		PlanMode:               cfg.Planning.Mode(),
		DefaultLeafRole:        cfg.Roles.DefaultLeaf,
		MaxDelegationDepth:     cfg.Roles.MaxDelegationDepth,
		MaxGoalSessions:        cfg.Daemon.MaxGoalSessions,
	})
	daemon := asm.Daemon
	supervisor := asm.Supervisor
	defer asm.EventSink.Close() //nolint:errcheck // best-effort drain on shutdown

	// Out-of-band subscribers (adr/reactive-events.md): on by default, but a
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

	// Graceful shutdown (docs/plugin-capabilities.md §5/§6). Without a handler a
	// SIGINT/SIGTERM kills the process outright, orphaning every plugin — and its
	// jobs, cache dir, and socket. Catch the signal, cancel the context (which
	// stops the daemon's accept loop and returns from Start), and let the cleanup
	// after Start cancel running jobs and stop the plugins.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		slog.Info("shutdown signal received; stopping")
		cancel()
	}()

	// Resolve this instance's display name (shown in the TUI top bar): the
	// configured name if set, else a previously generated one from the store,
	// else a placeholder that a background LLM call replaces with a random name
	// and persists (docs/configuration.md). Non-blocking.
	daemon.ResolveInstanceName(ctx, cfg.Daemon.InstanceName, store, cfg.BuildProvider())

	// Start the agent builder's main loop in the background, so it can manage agents while the daemon is running.
	go supervisor.Run(ctx)

	// Spilled tool outputs are session debris: sweep the expired ones on boot
	// and hourly thereafter so large results cannot grow the file store without
	// bound (adr/tool-output-spill.md §5).
	go runtime.RunSpillSweeper(ctx, store)

	// Any plugin job still marked running belongs to a plugin the previous daemon
	// left behind (this boot spawned fresh ones), so it is unreachable: mark such
	// rows lost and tell their owners (docs/plugin-capabilities.md §5).
	runtime.MarkOrphanedJobsLost(store)

	// Poll running plugin jobs (docs/plugin-capabilities.md §5): reconcile their
	// state, expire over-age ones, and on completion cap-or-spill the result and
	// notify the owning conversation so the next turn learns of it.
	go runtime.RunJobSweeper(ctx, store, pluginManager, jobWaiters,
		time.Duration(cfg.Plugins.JobPollSeconds)*time.Second, cfg.Plugins.JobMaxSeconds)

	// Reconcile pre-defined agents declared in nine.toml: seed a config-owned
	// goal + pursue shell for each, and bring existing ones' definitions in line
	// with the file (docs/predefined-agents.md). Must run after the daemon is
	// fully wired and before ResumeSessions (which revives what this seeds).
	reconcileStandingAgents(ctx, store, daemon, cfg.Agents, cfg.Daemon.StandingAgentsAuthoritative)

	// Resume any session (e.g. a goal's pursue session, once that lands) whose
	// plan was active with an idle-capable routine when the daemon last stopped.
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

	// Graceful cleanup: ask every running plugin job to cancel, then stop the
	// plugin processes (which also removes their ephemeral cache dirs and sockets).
	// Bounded so shutdown cannot hang on an unresponsive plugin.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	runtime.ShutdownJobs(shutdownCtx, store, pluginManager)
	shutdownCancel()
	if err := pluginManager.StopAll(); err != nil {
		slog.Warn("stop plugins on shutdown", "err", err)
	}
}

// startMCPServers starts one `mcp` bridge instance per [[mcp.server]].
//
// The daemon reads the config and hands each bridge only its own server's spec,
// as JSON in a Nine-owned environment variable. That indirection is required,
// not stylistic: a plugin child must not read nine.toml (R-PLUG.13a), which
// carries the embeddings API key and every other plugin's settings. Passing the
// slice it needs keeps the bridge to exactly the data it is entitled to.
//
// A server that fails to start is logged and skipped by TryStartBuiltinInstance
// — one unreachable MCP server must not stop the daemon from booting.
// startMCPServers brings up one bridge per configured MCP server.
//
// Concurrently, because each start blocks for that server's whole handshake and
// a measured `npx` server takes ~72s to answer tools/list. Serially, four
// servers would be five minutes of boot during which the daemon has not yet
// listened and no client can connect. Starting them together makes the cost the
// slowest server rather than their sum.
func startMCPServers(mgr *plugin.Manager, cfg *config.Config) {
	var wg sync.WaitGroup
	for _, srv := range cfg.MCP.Servers {
		spec, err := json.Marshal(map[string]any{
			"name":         srv.Name,
			"command":      srv.Command,
			"args":         srv.Args,
			"env":          srv.Env,
			"url":          srv.URL,
			"headers":      srv.Headers,
			"nine_version": Version,
		})
		if err != nil {
			// Only unmarshalable values could cause this, and the config types are
			// all strings; log rather than fail the boot.
			slog.Error("encode MCP server spec", "server", srv.Name, "err", err)
			continue
		}
		instance := plugin.MCPInstanceName(srv.Name)
		env := append(cfg.PluginEnvs(instance), "NINE_MCP_SERVER="+string(spec))
		wg.Add(1)
		go func() {
			defer wg.Done()
			mgr.TryStartBuiltinInstance(builtins.MCPBuiltinName, instance, env...)
		}()
	}
	// Waited on rather than left running: the tool registry has to be complete
	// before the first turn, or a conversation can start without tools that were
	// configured.
	wg.Wait()
}
