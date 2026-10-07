package runtime

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"nine/internal/memory"
)

// SelfReflectionAgentID is the well-known, fixed session ID for the dedicated
// self-reflection session, and the id of the process driving it.
// new_conversation always assigns a generated UUID, so this fixed string can
// never collide.
const SelfReflectionAgentID = "self-reflection"

// BootstrapSelfKV writes initial self/* KV seeds on first start.
// self/learned is intentionally omitted — the first reflection turn creates it.
func BootstrapSelfKV(store *memory.Store, loadedPlugins []string) error {
	_, found, err := store.Get("self/identity")
	if err != nil {
		return fmt.Errorf("check self/identity: %w", err)
	}
	if found {
		return nil // already bootstrapped
	}

	toolList := strings.Join(loadedPlugins, ", ")
	if err := store.Set("self/identity", "Nine is a persistent AI agent daemon. It maintains conversation history, can execute shell commands, manage files, make HTTP requests, browse the web, run background goals and workflows, delegate to sub-agents, and write its own skills. It is self-improving and stores its evolving self-model in the self/ key-value namespace."); err != nil {
		return fmt.Errorf("set self/identity: %w", err)
	}
	if err := store.Set("self/capabilities", "Loaded plugins: "+toolList+". Can use all tools exposed by these plugins."); err != nil {
		return fmt.Errorf("set self/capabilities: %w", err)
	}
	slog.Info("bootstrapped self/* KV keys")
	return nil
}

// ReconcileSelfReflection brings the self-reflection process in line with the
// operator's configuration, in both directions (adr/process-sessions.md §3).
//
// interval > 0 writes the shipped `reflect` process owning the self-reflection
// session, under the reflection role, at that cadence; configuration owns the
// definition, so a changed interval applies at once. interval == 0 means the
// operator removed reflection, and an existing process is **stopped** — not
// merely left uncreated, because a process outlives the boot that made it, so
// "stop creating it" would leave every machine that had ever run reflection
// still running it. The row and the session are kept, so the history stays
// readable with `nine reflections`.
func ReconcileSelfReflection(store ProcessBackend, idleInterval time.Duration) error {
	existing, found, err := store.ProcessGet(SelfReflectionAgentID)
	if err != nil {
		return fmt.Errorf("check the self-reflection process: %w", err)
	}
	if idleInterval <= 0 {
		if !found || existing.State == memory.ProcessStopped {
			return nil
		}
		if _, err := store.ProcessSetState(SelfReflectionAgentID, memory.ProcessStopped); err != nil {
			return fmt.Errorf("stop the self-reflection process: %w", err)
		}
		slog.Info("self-reflection disabled by config; process stopped")
		return nil
	}
	if err := store.ProcessUpsertDefinition(memory.Process{
		ID: SelfReflectionAgentID, Tool: "reflect", Mode: memory.ProcessLive,
		SessionID: SelfReflectionAgentID, Owner: true, Role: ReflectionRole,
		IntervalSecs: int(idleInterval.Seconds()),
	}); err != nil {
		return fmt.Errorf("write the self-reflection process: %w", err)
	}
	// Turned back on after being turned off: run again.
	if found && existing.State == memory.ProcessStopped {
		if _, err := store.ProcessSetState(SelfReflectionAgentID, memory.ProcessRunning); err != nil {
			return err
		}
	}
	if !found {
		slog.Info("self-reflection process created", "interval", idleInterval)
	}
	return nil
}
