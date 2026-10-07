package main

import (
	"context"
	"fmt"
	"log/slog"

	"nine/internal/config"
	"nine/internal/embed"
	"nine/internal/memory"
	"nine/internal/plugin"
	"nine/internal/runtime"
	"nine/internal/subscribers"
)

// Nine's own agent around the runtime (adr/agent-boundary.md): what it puts in
// the store before the daemon is assembled, and what it runs beside the daemon
// once it is wired. The sessions themselves come from runtime.InternalAgent.

// bootInternalAgent seeds the store with what the internal agent's sessions
// read: skills, the self-model, and the self-reflection session. An error
// stops the boot.
func bootInternalAgent(cfg *config.Config, store *memory.Store, embedder embed.Embedder, plugins *plugin.Manager) error {
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

	// A packaged self-model, if the operator configured one, runs first: it fills
	// self/identity so the generic default below is never written over the
	// identity this instance was shipped with (adr/personality-pattern.md §4).
	// A malformed file stops the boot rather than silently producing generic Nine
	// under a personality's name.
	if _, err := runtime.BootstrapSelfModel(store, cfg.Bootstrap.SelfModelPath); err != nil {
		return fmt.Errorf("self-model bootstrap: %w", err)
	}

	// Bootstrap the self-model with the current plugin list, so it can answer questions about them.
	if err := runtime.BootstrapSelfKV(store, plugins.ListRunning()); err != nil {
		slog.Error("failed to bootstrap self KV", "err", err)
	}

	// Reconcile the self-reflection process against config — writing it, or
	// stopping it when the operator has turned reflection off. The process
	// runner starts it.
	if err := runtime.ReconcileSelfReflection(store, cfg.SelfReflectionInterval()); err != nil {
		slog.Error("failed to reconcile self-reflection session", "err", err)
	}
	return nil
}

// startInternalAgent starts the internal agent's background work on a wired
// daemon: the related-session indexer, the instance name, the supervisor, and
// the standing agents declared in config. It runs before the daemon starts.
func startInternalAgent(ctx context.Context, cfg *config.Config, store *memory.Store, embedder embed.Embedder,
	daemon *runtime.Daemon, supervisor *runtime.Supervisor) {
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

	// Resolve this instance's display name (shown in the TUI top bar): the
	// configured name if set, else a previously generated one from the store,
	// else a placeholder that a background LLM call replaces with a random name
	// and persists (docs/configuration.md). Non-blocking.
	daemon.ResolveInstanceName(ctx, cfg.Daemon.InstanceName, store, cfg.BuildProvider())

	// Start the agent builder's main loop in the background, so it can manage agents while the daemon is running.
	go supervisor.Run(ctx)

	// Reconcile pre-defined agents declared in nine.toml: seed a config-owned
	// goal + pursue shell for each, and bring existing ones' definitions in line
	// with the file (docs/predefined-agents.md).
	reconcileStandingAgents(ctx, store, daemon, cfg.Process, cfg.Processes.Authoritative)
}
