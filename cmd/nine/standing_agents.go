package main

import (
	"context"
	"log/slog"
	"time"

	"nine/internal/config"
	"nine/internal/cron"
	"nine/internal/memory"
	"nine/internal/runtime"
)

// configGoalOrigin is the goals.parent_type sentinel marking a goal seeded from
// a goal [[process]] block rather than a conversation. Reconciliation touches
// only these, never a conversation-created goal (adr/predefined-agents-design.md §3.3).
const configGoalOrigin = runtime.ConfigGoalOrigin

// defaultStandingRole is the role a goal [[process]] runs when none is configured: the
// read-only monitor (adr/predefined-agents-design.md §6).
const defaultStandingRole = "monitor"

// reconcileStandingAgents brings the pre-defined agents declared in nine.toml
// up to their desired state at boot (adr/predefined-agents-design.md §3.3, §4). Config
// owns each agent's *definition* (description/role/delegates/trigger, reconciled
// in place every boot); the agent owns its *run-state* (goal status) — a goal
// the agent paused or finished is never resurrected. It runs after the daemon is
// fully wired and before ResumeSessions, which revives anything already seeded.
func reconcileStandingAgents(ctx context.Context, store *memory.Store, daemon *runtime.Daemon, blocks []config.ProcessConfig, authoritative bool) {
	agents := goalBlocks(blocks)
	for _, a := range agents {
		if a.Name == "" || a.Goal == "" {
			continue
		}

		// Resolve the wake trigger: a cron schedule, a fixed interval, or the
		// default interval when neither is set.
		var interval time.Duration
		if a.Every != "" {
			d, err := time.ParseDuration(a.Every)
			if err != nil || d <= 0 {
				slog.Warn("skipping goal [[process]]: invalid interval", "id", a.Name, "every", a.Every, "err", err)
				continue
			}
			interval = d
		}
		if a.Schedule != "" {
			if _, err := cron.Parse(a.Schedule); err != nil {
				slog.Warn("skipping goal [[process]]: invalid cron schedule", "id", a.Name, "schedule", a.Schedule, "err", err)
				continue
			}
		}

		role := a.Role
		if role == "" {
			role = defaultStandingRole
		}

		goal, err := store.GoalGet(a.Name)
		if err != nil {
			slog.Warn("standing agent reconcile: goal lookup failed", "id", a.Name, "err", err)
			continue
		}

		if goal == nil {
			// First sighting: seed the goal (config origin) and start its shell.
			if err := store.GoalCreate(a.Name, a.Goal, "", configGoalOrigin); err != nil {
				slog.Warn("standing agent reconcile: goal create failed", "id", a.Name, "err", err)
				continue
			}
			if _, err := daemon.SpawnStandingSession(ctx, a.Name, role, a.Delegates, interval, a.Schedule); err != nil {
				slog.Warn("standing agent reconcile: spawn failed", "id", a.Name, "err", err)
				continue
			}
			slog.Info("standing agent created", "id", a.Name, "role", role)
			continue
		}

		// Never reconcile a goal Nine didn't seed from config — a conversation
		// may have created a goal that happens to share this id.
		if goal.ParentType != configGoalOrigin {
			slog.Warn("standing agent reconcile: id collides with a non-config goal; skipping", "id", a.Name)
			continue
		}

		// Config owns the definition: reconcile the description in place.
		if goal.Description != a.Goal {
			if err := store.GoalUpdateDescription(a.Name, a.Goal); err != nil {
				slog.Warn("standing agent reconcile: description update failed", "id", a.Name, "err", err)
			}
		}

		// The agent owns run-state: only an active goal is (re)spawned; a paused
		// or finished agent keeps its status and is not resurrected (§4).
		if goal.Status == "active" {
			if _, err := daemon.SpawnStandingSession(ctx, a.Name, role, a.Delegates, interval, a.Schedule); err != nil {
				slog.Warn("standing agent reconcile: spawn failed", "id", a.Name, "err", err)
			}
		} else {
			slog.Info("standing agent not resurrected (agent-owned status)", "id", a.Name, "status", goal.Status)
		}
	}

	// Subtractive reconciliation (opt-in): when config is authoritative, retire
	// config-origin goals no longer listed in nine.toml (docs/predefined-agents.md
	// §7 v3). Runs after the additive pass so a re-declared id is never treated as
	// removed.
	if authoritative {
		subtractStandingAgents(ctx, store, daemon, agents)
	}
}

// subtractStandingAgents archives and stops every config-origin standing agent
// whose id is no longer present in the config, leaving conversation-created
// goals untouched. Only live (active/paused) agents are archived; already-
// terminal ones are left as-is (their session is not running post-boot anyway).
func subtractStandingAgents(ctx context.Context, store *memory.Store, daemon *runtime.Daemon, agents []config.ProcessConfig) {
	desired := desiredAgentIDs(agents)
	goals, err := store.GoalList()
	if err != nil {
		slog.Warn("subtractive reconcile: goal list failed", "err", err)
		return
	}
	for _, g := range removedConfigGoals(goals, desired) {
		if err := daemon.TeardownStandingSession(ctx, g.ID); err != nil {
			slog.Warn("subtractive reconcile: teardown failed", "id", g.ID, "err", err)
		}
		if g.Status == "active" || g.Status == "paused" {
			if err := store.GoalUpdateStatus(g.ID, "archived"); err != nil {
				slog.Warn("subtractive reconcile: archive failed", "id", g.ID, "err", err)
				continue
			}
		}
		slog.Info("standing agent removed from config; retired", "id", g.ID, "was", g.Status)
	}
}

// desiredAgentIDs is the set of non-empty ids declared in the config — the
// standing agents the operator still wants Nine to manage.
func desiredAgentIDs(agents []config.ProcessConfig) map[string]bool {
	set := make(map[string]bool, len(agents))
	for _, a := range agents {
		if a.Name != "" {
			set[a.Name] = true
		}
	}
	return set
}

// removedConfigGoals returns the config-origin goals not present in desired —
// the standing agents to tear down. Goals created by a conversation
// (parent_type != "config") are never included, so subtractive reconciliation
// can never touch a human-created goal (adr/predefined-agents-design.md §4).
func removedConfigGoals(goals []memory.Goal, desired map[string]bool) []memory.Goal {
	var out []memory.Goal
	for _, g := range goals {
		if g.ParentType != configGoalOrigin {
			continue
		}
		if desired[g.ID] {
			continue
		}
		out = append(out, g)
	}
	return out
}

// goalBlocks are the [[process]] blocks that declare a standing agent: a pursue
// process bound to a goal this file owns.
func goalBlocks(blocks []config.ProcessConfig) []config.ProcessConfig {
	var out []config.ProcessConfig
	for _, b := range blocks {
		if b.Goal != "" {
			out = append(out, b)
		}
	}
	return out
}
