package runtime

import (
	"log/slog"
	"slices"
	"sort"
	"strings"

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
// resolve store-backed role skills. Nil-able (tests, storeless builders).
type roleSkillStore interface {
	SkillGet(name string) (memory.Skill, bool, error)
	SkillsBySource(source string) ([]memory.Skill, error)
}

// storeRoleSources are the skill sources the registry scans for delegatable
// role skills, in precedence order after the built-ins.
var storeRoleSources = []string{memory.SkillSourceUser, memory.SkillSourceAgent}

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
// flags are honored only for trusted (operator-authored: built-in or user)
// skills; agent-authored role skills are forced to leaf defaults so a role can
// never be an escalation path (R-ROLE.7).
func roleFromSkill(name, description, body string, spec skills.RoleSpec, trusted bool) Role {
	role := Role{
		Name:         name,
		Description:  description,
		SystemPrompt: body,
		AllTools:     spec.AllTools,
		Tools:        spec.Tools,
	}
	if trusted {
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
		if sk, found, err := r.store.SkillGet(name); err == nil && found && slices.Contains(storeRoleSources, sk.Source) {
			if parsed := skills.Parse(sk.Content); parsed.Role != nil {
				return roleFromSkill(sk.Name, sk.Description, parsed.Content, *parsed.Role, trustedRoleSource(sk.Source))
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
// are ignored — a delegated worker never persists, never raises its own
// ask_human, and never spawns goal sessions, whatever role it runs (R-ROLE.9).
// Interactive=false here governs ask_human only; approval gates reach a child
// through its parent's gateCtx, not this flag (R-HITL.5).
func (r *RoleRegistry) ResolveLeaf(name string) Role {
	role := r.Resolve(name)
	role.SpawnsGoals = false
	role.Persists = false
	role.Interactive = false
	role.OwnsGoal = false
	role.Profile = nil
	return role
}

// LeafRole is one delegation-target role as surfaced to the model on
// run_agent/run_agents (R-ROLE.8): the name it passes and the one-line
// description it chooses by.
type LeafRole struct {
	Name        string
	Description string
}

// LeafRoles returns the names of every delegatable leaf role, built-ins first
// then store-backed, each group sorted by name.
func (r *RoleRegistry) LeafRoles() []string {
	leaves := r.LeafRoleDescriptions()
	names := make([]string, 0, len(leaves))
	for _, l := range leaves {
		names = append(names, l.Name)
	}
	return names
}

// LeafRoleDescriptions returns every delegatable leaf role (non-persisting,
// non-goal-spawning) with its description, for rendering the run_agent role
// enum (R-ROLE.8). Built-ins come first, then store-backed role skills; both
// groups are sorted by name so the rendered tool schema is stable across
// boots. A store role may not shadow a built-in.
func (r *RoleRegistry) LeafRoleDescriptions() []LeafRole {
	var builtins, stored []LeafRole
	seen := make(map[string]bool, len(r.builtins))

	for name, role := range r.builtins {
		if name == AnalystRole {
			// Internal prompt-fragment role (M4): borrowed for the no-tool
			// analysis pass, never a delegation target — keep it off the
			// run_agent surface.
			continue
		}
		seen[name] = true
		if !role.Persists && !role.SpawnsGoals {
			builtins = append(builtins, LeafRole{Name: name, Description: role.Description})
		}
	}

	if r.store != nil {
		for _, source := range storeRoleSources {
			roleSkills, err := r.store.SkillsBySource(source)
			if err != nil {
				slog.Warn("list role skills", "source", source, "err", err)
				continue
			}
			for _, sk := range roleSkills {
				if seen[sk.Name] || sk.Name == AnalystRole {
					continue
				}
				parsed := skills.Parse(sk.Content)
				if parsed.Role == nil {
					continue // a plain knowledge skill, not a role
				}
				// Structural flags are honored per R-ROLE.7 — agent-authored
				// roles are purely restrictive, so they are always leaves.
				role := roleFromSkill(sk.Name, sk.Description, parsed.Content, *parsed.Role, trustedRoleSource(sk.Source))
				if role.Persists || role.SpawnsGoals {
					continue
				}
				seen[sk.Name] = true
				stored = append(stored, LeafRole{Name: sk.Name, Description: sk.Description})
			}
		}
	}

	sortLeaves(builtins)
	sortLeaves(stored)
	return append(builtins, stored...)
}

// DefaultLeaf returns the role a delegation with no (or an unknown) role name
// resolves to (roles.default_leaf; R-ROLE.9).
func (r *RoleRegistry) DefaultLeaf() string { return r.defaultLeaf }

// RoleEnum renders the leaf-role list for the `role` field on
// run_agent/run_agents (R-ROLE.8) as "name — description" pairs, marking the
// default leaf. Returns "" when the registry has no leaf roles, letting the
// caller keep its static fallback text.
func (r *RoleRegistry) RoleEnum() string {
	leaves := r.LeafRoleDescriptions()
	if len(leaves) == 0 {
		return ""
	}
	// Entries are separated by " | " and name/description by ": ". Role
	// descriptions routinely contain em-dashes and semicolons, so neither of
	// those can serve as a separator without reading ambiguously.
	var b strings.Builder
	b.WriteString("One of: ")
	for i, l := range leaves {
		if i > 0 {
			b.WriteString(" | ")
		}
		b.WriteString(l.Name)
		if l.Name == r.defaultLeaf {
			b.WriteString(" (default)")
		}
		if desc := strings.TrimRight(strings.TrimSpace(l.Description), "."); desc != "" {
			b.WriteString(": ")
			b.WriteString(desc)
		}
	}
	b.WriteString(".")
	return b.String()
}

func sortLeaves(l []LeafRole) {
	sort.Slice(l, func(i, j int) bool { return l[i].Name < l[j].Name })
}

// trustedRoleSource reports whether role skills from this source may set
// structural flags (R-ROLE.7). Agent-authored roles are purely restrictive;
// operator-authored ones are trusted like built-ins.
func trustedRoleSource(source string) bool {
	return slices.Contains(trustedRoleSources, source)
}

// trustedRoleSources are the skill sources whose role blocks may set
// structural flags (R-ROLE.7). A user skill comes from a file the operator
// mounted — the same trust level as editing nine.toml — so it is trusted like
// a built-in. An agent-authored one is written by Nine at runtime and stays
// purely restrictive: defining a role must never become an escalation path.
var trustedRoleSources = []string{memory.SkillSourceUser}

// roleNameForPlan maps a session plan's stages to the role its worker runs
// (docs/roles.md §6). The role is data: a stage that declares one in its config
// supplies it, whatever its kind. Failing that, a pursue shell runs the pursue
// role, and everything else — ordinary [active] conversations, nil plans — the
// orchestrator.
//
// Resolving by config rather than by kind is what lets a stage kind mean
// different things in different plans: a reflection stage is the session's whole
// purpose when it stands alone (and declares the reflection role), and a
// passenger when it rides beside a pursue shell (and declares none).
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
	// A role declared in stage config wins wherever it appears; validateStages
	// guarantees at most one stage declares one.
	for _, st := range plan.plan.Stages {
		if role := stageRole(st.Config); role != "" {
			return role
		}
	}
	// Otherwise a pursue shell runs the default pursue role.
	for _, st := range plan.plan.Stages {
		if st.Kind == "pursue" {
			return PursueRole
		}
	}
	return OrchestratorRole
}
