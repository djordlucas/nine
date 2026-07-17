package runtime

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"nine/internal/memory"
)

// SelfReflectionAgentID is the well-known, fixed session ID for the dedicated
// self-reflection session (docs/session-plans.md, Pilot 3: "reserved
// agentIDs: not actually reserved"). new_conversation always assigns a
// generated UUID, so this fixed string can never collide.
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

// BootstrapSelfReflection ensures the self-reflection session_plans row
// exists, seeded with profile [idle-reflection] and the given idle interval.
// A no-op if the row already exists (docs/session-plans.md, Pilot 4's
// one-time bootstrap — every subsequent boot is handled by the general
// resume pass instead).
func BootstrapSelfReflection(store PlanStore, idleInterval time.Duration) error {
	existing, err := store.SessionPlanGet(SelfReflectionAgentID)
	if err != nil {
		return fmt.Errorf("check self-reflection session plan: %w", err)
	}
	if existing != nil {
		return nil // already bootstrapped
	}

	plan, err := newIdleCapablePlan(SelfReflectionAgentID, "idle-reflection", idleInterval)
	if err != nil {
		return err
	}
	if err := store.SessionPlanSave(plan); err != nil {
		return fmt.Errorf("create self-reflection session plan: %w", err)
	}
	slog.Info("bootstrapped self-reflection session")
	return nil
}
