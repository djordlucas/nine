package main

import (
	"context"
	"encoding/json"
	"fmt"
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
)

func runDaemon() {
	// Load configuration
	cfg, err := config.LoadDefault()
	if err != nil {
		// A config Nine cannot understand is a stop, not a warning: running on
		// defaults would look like a clean boot while silently dropping every
		// setting the operator wrote.
		fmt.Fprintln(os.Stderr, "nine: "+err.Error())
		os.Exit(1)
	}

	// Refuse a backend Nine does not have, rather than quietly serving a
	// different one. Checked here, against the effective config, because
	// LoadDefault applies environment overrides after the file is validated.
	if err := cfg.CheckProvider(); err != nil {
		slog.Error("configuration check failed", "err", err)
		os.Exit(1)
	}

	// Initialize memory store
	dbPath, err := cfg.DatabasePath()
	if err != nil {
		slog.Error("failed to resolve memory store path", "err", err)
		os.Exit(1)
	}
	store, err := memory.Open(dbPath)
	if err != nil {
		slog.Error("failed to open memory store", "path", dbPath, "err", err)
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
	generatedTools := runtime.NewGeneratedToolStoreWithStanding(store, toolHost, pluginManager,
		runtime.NewDepsBundler(cfg), cfg.Tools.Agent.AllowNetworkDeps,
		cfg.Tools.Agent.AllowStanding, cfg.Tools.Agent.MaxStanding)

	embedder := embed.Build(cfg.Embeddings.Provider, cfg.Embeddings.Model, cfg.Embeddings.Endpoint)

	// Index the docs and spec embedded in this binary so Nine can retrieve its
	// own manual on demand (docs/self-documentation.md). Fingerprinted, so this
	// is a no-op on every boot that does not change the binary or the embedder.
	if err := runtime.SeedDocs(store, embedder, cfg.Embeddings.Provider+"/"+cfg.Embeddings.Model); err != nil {
		slog.Warn("seed docs index", "err", err)
	}

	// Files an agent saved with the retired file_store move into the workspace,
	// keeping their paths, so anything that recorded where it put a file still
	// finds it there (adr/file-namespaces.md §12). A no-op after the first boot
	// that runs it.
	runtime.MigrateStoredFilesToWorkspace(store, cfg.Workspace.Root)

	// Nine's own agent: skills, self-model, reflection (internal_agent.go).
	if err := bootInternalAgent(cfg, store, embedder, pluginManager); err != nil {
		slog.Error("internal agent boot failed", "err", err)
		os.Exit(1)
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
	// The workspace index: a scanner keeping it in step with the directory, and
	// the backend the file tools search. A file Nine never wrote — from a bind
	// mount, a git pull, or someone dropping one in — is findable because the
	// scan saw it (adr/file-namespaces.md §6).
	workspaceScanner := runtime.NewWorkspaceScanner(store, cfg.Workspace.Root,
		cfg.Workspace.IndexMaxFileBytesOrDefault(), cfg.Workspace.IndexMaxFilesOrDefault(),
		cfg.Workspace.ScanIntervalOrDefault())

	asm := runtime.Assemble(runtime.AssemblyConfig{
		Workspace:              runtime.NewWorkspaceBackend(store, workspaceScanner, cfg.Workspace.Root),
		WorkspaceRoot:          cfg.Workspace.Root,
		SocketPath:             cfg.SocketPath(),
		Store:                  store,
		Plugins:                pluginManager,
		Tools:                  toolHost,
		GeneratedTools:         generatedTools,
		GeneratedEval:          cfg.Tools.Agent.Eval,
		GeneratedApproval:      cfg.Tools.Agent.ApprovalMode(),
		GeneratedAllowStanding: cfg.Tools.Agent.AllowStanding,
		Embedder:               embedder,
		ContextBudget:          cfg.ContextBudget(),
		MaxTokens:              cfg.MaxReplyTokens(),
		SystemPrompt:           runtime.BuildSystemPrompt(),
		Runtime:                cfg.RuntimeLabel(),
		// Pull-surface related prior sessions only when the out-of-band indexer
		// that populates the store is enabled.
		RelatedSessions: cfg.Daemon.RelatedSessionsIndexEnabled(),
		// Index and pull-surface stored key-value memories relevant to the turn.
		SurfaceMemories:        cfg.Memory.SurfaceMemoriesEnabled(),
		MaxToolOutputTokens:    cfg.Tools.MaxOutputTokens,
		MaxJobsPerConversation: cfg.Plugins.MaxJobsPerConversation,
		MaxJobsTotal:           cfg.Plugins.MaxJobsTotal,
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
		JobMinDelayMS:          cfg.Tools.JobMinDelayMS,
		JobWorkers:             cfg.Tools.JobWorkers,
	})
	daemon := asm.Daemon
	supervisor := asm.Supervisor
	defer asm.EventSink.Close() //nolint:errcheck // best-effort drain on shutdown

	ctx, cancel := context.WithCancel(context.Background())

	// Graceful shutdown (docs/plugin-capabilities.md §5/§6). Without a handler a
	// SIGINT/SIGTERM kills the process outright, orphaning every plugin — and its
	// jobs, cache dir, and socket. Catch the signal, trigger checkpoints for all
	// active sessions (to prevent message loss), then cancel the context (which
	// stops the daemon's accept loop and returns from Start), and let the cleanup
	// after Start cancel running jobs and stop the plugins.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		slog.Info("shutdown signal received; checkpointing sessions...")
		daemon.CheckpointAll()
		slog.Info("shutdown signal received; stopping")
		cancel()
	}()

	// Standing tools: resumable tools the daemon runs indefinitely on their own
	// cadence (adr/standing-tools.md). Config owns each definition; the runtime
	// owns whether it is running, so reconciling does not restart one an operator
	// stopped. They share the job sweeper's worker budget — what both bound is
	// concurrent wasm instantiations.
	runtime.ReconcileProcesses(store, toolHost, cfg.Process)
	// The process runner, built by Assemble: standing tools, goal sessions,
	// standing agents and self-reflection all run through it
	// (adr/process-sessions.md).
	standing := asm.Processes
	// The capability surface behind `nine grants`, the TUI's /grants view and the
	// API's /capabilities endpoints — one decision path for all three, because an
	// approval widens the live ceiling and that must not be three implementations.
	daemon.ConfigureCapabilities(runtime.NewCapabilityService(store, cfg, toolHost, pluginManager))
	// So `tool_delete` on a generated standing tool also drops its activity ring.
	runtime.LinkStandingTools(generatedTools, standing)
	go runtime.RunStandingTools(ctx, standing,
		time.Duration(cfg.Plugins.JobPollSeconds)*time.Second)

	// Delete sessions nobody has touched in a while, on boot and daily. Never
	// one with an active goal or a process driving it — those are idle by design
	// (runtime.RunSessionReaper). 0 disables it.
	go runtime.RunSessionReaper(ctx, daemon,
		cfg.SessionRetention(runtime.DefaultSessionRetentionDays))

	// Spilled tool outputs are session debris: sweep the expired ones on boot
	// and hourly thereafter so large results cannot grow the file store without
	// bound (adr/tool-output-spill.md §5).
	go runtime.RunSpillSweeper(ctx, store)

	// The workspace scan: once at boot in the background, then on its interval.
	// A large workspace must not hold up startup, and a search before the first
	// scan finishes says so rather than reporting an empty index as an empty
	// directory.
	go workspaceScanner.Run(ctx)

	// Deleted and overwritten workspace files are kept under .nine/trash/ so a
	// mistake in an ungated session is recoverable. That directory is on the
	// operator's own disk, so it is bounded by both age and size
	// (adr/file-namespaces.md §9).
	go runtime.RunTrashSweeper(ctx, cfg.Workspace.Root,
		cfg.Workspace.TrashRetentionDuration(), cfg.Workspace.TrashSizeBound())

	// Any *plugin* job still marked running belongs to a plugin the previous
	// daemon left behind (this boot spawned fresh ones), so it is unreachable:
	// mark such rows lost and tell their owners (docs/plugin-capabilities.md §5).
	runtime.MarkOrphanedJobsLost(store)

	// A tool job is the opposite case and needs no repair. Its whole live state
	// is the cursor on its row, so this daemon simply makes the next call; the
	// only thing to do at boot is say which ones are being picked up.
	runtime.ResumeToolJobs(store)

	// Drive both job backends (docs/plugin-capabilities.md §5,
	// adr/durable-and-long-running-tools.md §4.3): poll running plugin jobs,
	// make the next call for due tool jobs, expire over-age ones, and on
	// completion cap-or-spill the result and notify the owning conversation so
	// the next turn learns of it.
	go runtime.RunJobSweeperWithTools(ctx, store, pluginManager, jobWaiters,
		time.Duration(cfg.Plugins.JobPollSeconds)*time.Second, cfg.Plugins.JobMaxSeconds,
		runtime.NewToolJobRunner(store, toolHost,
			cfg.Tools.JobMaxCalls, cfg.Tools.JobMinDelayMS, cfg.Tools.JobWorkers))

	// Nine's own agent's background work and standing agents
	// (internal_agent.go). The processes it writes are started by the runner.
	startInternalAgent(ctx, cfg, store, embedder, daemon, supervisor)

	// Start the daemon, and log any errors. The daemon will run until the process is killed.
	slog.Info("starting the nine daemon", "socket", cfg.SocketPath())
	if err := daemon.Start(ctx); err != nil {
		cancel()
		slog.Error("daemon failed", "err", err)
		os.Exit(1)
	}
	cancel()

	// Stop the daemon and wait for in-flight turns to complete and save their state
	daemon.Stop()

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
			slog.Error("failed to encode MCP server spec", "server", srv.Name, "err", err)
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
