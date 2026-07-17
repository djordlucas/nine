package runtime

import (
	"log/slog"

	"nine/internal/memory"
	"nine/skills"
)

// Role names with structural meaning. OrchestratorRole is the root
// session-follower every conversation runs; DefaultLeafRoleName is the leaf
// spawned when a delegation names no role (overridable via
// roles.default_leaf).
const (
	AnalystRole         = "analyst"
	OrchestratorRole    = "orchestrator"
	DefaultLeafRoleName = "executor"
	ReflectionRole      = "reflection"
	PursueRole          = "pursue"
)

// DefaultMaxDelegationDepth is the depthGuard seed: how many delegation hops
// a root worker's subtree may make (docs/roles.md R-ROLE.6). It reproduces
// the old `depth < 2` rule.
const DefaultMaxDelegationDepth = 2

// Role is a worker kind: persona + enforced tool boundary + structural
// wiring (docs/roles.md §2). It subsumes the old depth-based gating and the
// sessionWorker/sub-agent fork of identity. Persists and Profile describe the
// worker shell a role runs in (session worker vs ephemeral leaf); they are
// realized by the daemon's session machinery, not by the loop builder.
type Role struct {
	Name        string
	Description string

	// SystemPrompt is the role's persona (the backing skill's body). Empty
	// means fall back to LoopConfig.SystemPrompt (R-ROLE.3).
	SystemPrompt string

	// AllTools grants every available tool; otherwise Tools is a strict
	// allowlist enforced at both the advertised list and the dispatcher
	// (R-ROLE.4). A role can only narrow the daemon's tool surface (R-ROLE.5).
	AllTools bool
	Tools    []string

	Delegates   bool     // run_agent/run_agents/workflow_*/goal_create tools (was: depth < 2)
	SpawnsGoals bool     // background pursue-session spawn fn (was: depth == 0)
	Persists    bool     // checkpointing session worker vs ephemeral leaf
	Interactive bool     // HITL-eligible; effective only when the caller is interactive
	Profile     []string // stage kinds; empty ⇒ ephemeral leaf

	// OwnsGoal marks a pursue-shell session that steers its own goal: it grants
	// the goal self-management tools (goal_get/goal_list/goal_update_status/
	// goal_append_subtree) regardless of the role's own allowlist, exactly as
	// gap_report is always granted (docs/predefined-agents.md §3.1). It is
	// conferred by the shell (the daemon), not declared in a role skill, and is
	// a root-only structural flag — ResolveLeaf strips it.
	OwnsGoal bool
}

// roleSkillStore is the slice of the memory store the registry needs to
// resolve agent-authored role skills. Nil-able (tests, storeless builders).
type roleSkillStore interface {
	SkillGet(name string) (memory.Skill, bool, error)
}

// RoleRegistry resolves role names to Roles. Built-in roles come from the
// embedded role skills (skills/roles/*.md) at construction; agent-authored
// role skills are looked up in the store at resolve time, so a skill_write
// takes effect on the next delegation with no refresh hook (R-ROLE.9).
type RoleRegistry struct {
	builtins    map[string]Role
	store       roleSkillStore
	defaultLeaf string
}

// NewRoleRegistry builds a registry from the embedded built-in role skills.
// store may be nil (agent-authored roles then never resolve). defaultLeaf
// names the role used when a delegation names none or an unknown one; empty
// falls back to DefaultLeafRoleName.
func NewRoleRegistry(store roleSkillStore, defaultLeaf string) *RoleRegistry {
	if defaultLeaf == "" {
		defaultLeaf = DefaultLeafRoleName
	}
	r := &RoleRegistry{
		builtins:    make(map[string]Role),
		store:       store,
		defaultLeaf: defaultLeaf,
	}
	defaults, err := skills.Defaults()
	if err != nil {
		slog.Error("load built-in role skills", "err", err)
	}
	for _, sk := range defaults {
		if sk.Role == nil {
			continue
		}
		r.builtins[sk.Name] = roleFromSkill(sk.Name, sk.Description, sk.Content, *sk.Role, true)
	}
	// The executor must always exist — it is the backward-compat anchor every
	// unknown delegation degrades to. Synthesize a minimal one if the embedded
	// role skills are ever broken, so resolution can never dead-end.
	if _, ok := r.builtins[DefaultLeafRoleName]; !ok {
		slog.Error("embedded executor role missing; synthesizing fallback")
		r.builtins[DefaultLeafRoleName] = Role{Name: DefaultLeafRoleName, AllTools: true, Delegates: true}
	}
	return r
}

// roleFromSkill converts a skill with a role block into a Role. Structural
// flags are honored only for built-in skills; agent-authored role skills are
// forced to leaf defaults so a role can never be an escalation path (R-ROLE.7).
func roleFromSkill(name, description, body string, spec skills.RoleSpec, builtin bool) Role {
	role := Role{
		Name:         name,
		Description:  description,
		SystemPrompt: body,
		AllTools:     spec.AllTools,
		Tools:        spec.Tools,
	}
	if builtin {
		role.Delegates = spec.Delegates
		role.SpawnsGoals = spec.SpawnsGoals
		role.Persists = spec.Persists
		role.Interactive = spec.Interactive
		role.Profile = spec.Profile
	}
	return role
}

// Resolve maps a role name to its Role. Empty or unknown names resolve to the
// default leaf role, never an error — the system degrades to pre-role
// behavior (R-ROLE.9). Agent-authored skills whose content carries a role
// block resolve as purely restrictive leaf roles (R-ROLE.7).
func (r *RoleRegistry) Resolve(name string) Role {
	if name == "" {
		name = r.defaultLeaf
	}
	if role, ok := r.builtins[name]; ok {
		return role
	}
	if r.store != nil {
		if sk, found, err := r.store.SkillGet(name); err == nil && found && sk.Source == memory.SkillSourceAgent {
			if parsed := skills.Parse(sk.Content); parsed.Role != nil {
				return roleFromSkill(sk.Name, sk.Description, parsed.Content, *parsed.Role, false)
			}
		}
	}
	if name != r.defaultLeaf {
		slog.Warn("unknown role, using default leaf", "role", name, "default", r.defaultLeaf)
	}
	if role, ok := r.builtins[r.defaultLeaf]; ok {
		return role
	}
	return r.builtins[DefaultLeafRoleName]
}

// ResolveLeaf resolves name for a spawned child: root-only structural flags
// are ignored — a delegated worker never persists, never gets HITL, and never
// spawns goal sessions, whatever role it runs (R-ROLE.9).
func (r *RoleRegistry) ResolveLeaf(name string) Role {
	role := r.Resolve(name)
	role.SpawnsGoals = false
	role.Persists = false
	role.Interactive = false
	role.OwnsGoal = false
	role.Profile = nil
	return role
}

// LeafRoles returns the names of all built-in leaf roles (non-persisting,
// non-goal-spawning), for surfacing in delegation tool descriptions and tests.
func (r *RoleRegistry) LeafRoles() []string {
	var names []string
	for name, role := range r.builtins {
		if name == AnalystRole {
			// Internal prompt-fragment role (M4): borrowed for the no-tool
			// analysis pass, never a delegation target — keep it off the
			// run_agent surface.
			continue
		}
		if !role.Persists && !role.SpawnsGoals {
			names = append(names, name)
		}
	}
	return names
}

// roleNameForPlan maps a session plan's stage profile to the role its worker
// runs (docs/roles.md §6): pursue sessions run the pursue role,
// idle-reflection sessions the reflection role, everything else (ordinary
// [active] conversations, nil plans) the orchestrator.
//
// A pursue stage may carry an explicit work-role name in its config (seeded by
// SpawnStandingSession for pre-defined agents, docs/predefined-agents.md §5
// piece 5); when present, that role's tools/persona run in place of the default
// pursue role, while the pursue shell itself (persistence, goal ownership) is
// unchanged.
func roleNameForPlan(plan *sessionPlanState) string {
	if plan == nil || plan.plan == nil {
		return OrchestratorRole
	}
	for _, st := range plan.plan.Stages {
		switch st.Kind {
		case "pursue":
			if role := stageRole(st.Config); role != "" {
				return role
			}
			return PursueRole
		case "idle-reflection":
			return ReflectionRole
		}
	}
	return OrchestratorRole
}
