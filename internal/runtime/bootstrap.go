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

// ReconcileSelfReflection brings the dedicated self-reflection session in line
// with the operator's configuration, in both directions.
//
// interval > 0 ensures the plan exists (a no-op once it does; later boots pick it
// up through the general resume pass). interval == 0 means the operator removed
// reflection, and an existing session is **deactivated** — not merely left
// uncreated. That distinction is the whole reason this replaced a one-shot
// bootstrap: a plan row outlives the boot that made it, so "stop creating it"
// would leave every machine that had ever run reflection still running it, and
// the setting would appear to do nothing.
//
// Deactivation mirrors TeardownStandingSession: the plan and its stages leave
// "active", which makes planNeedsResume false on every later boot. The row is
// kept rather than deleted so the history stays readable with
// `nine reflections`.
func ReconcileSelfReflection(store PlanStore, idleInterval time.Duration) error {
	existing, err := store.SessionPlanGet(SelfReflectionAgentID)
	if err != nil {
		return fmt.Errorf("check self-reflection session plan: %w", err)
	}

	if idleInterval <= 0 {
		if existing == nil || existing.Status != "active" {
			return nil
		}
		now := time.Now().UTC().Format(time.RFC3339)
		existing.Status = "archived"
		for i := range existing.Stages {
			existing.Stages[i].Status = "done"
			existing.Stages[i].UpdatedAt = now
		}
		existing.UpdatedAt = now
		if err := store.SessionPlanSave(existing); err != nil {
			return fmt.Errorf("deactivate self-reflection session plan: %w", err)
		}
		slog.Info("self-reflection disabled by config; session deactivated")
		return nil
	}

	if existing != nil {
		return nil // already present; cadence changes apply to a fresh plan only
	}

	// The role is stamped into the stage config rather than implied by the kind,
	// so this session keeps the reflection role while the same kind can ride
	// role-free beside a pursue shell elsewhere.
	plan, err := newIdleCapablePlan(SelfReflectionAgentID, "idle-reflection", ReflectionRole, idleInterval)
	if err != nil {
		return err
	}
	if err := store.SessionPlanSave(plan); err != nil {
		return fmt.Errorf("create self-reflection session plan: %w", err)
	}
	slog.Info("self-reflection session created", "interval", idleInterval)
	return nil
}
