package runtime_test

import (
	"encoding/json"
	"strings"
	"testing"

	"nine/internal/agent"
	"nine/internal/memory"
	"nine/internal/memory/memtest"
	"nine/internal/runtime"
)

func newRoleTestStore(t *testing.T) *memory.Store {
	t.Helper()
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestRegistryBuiltinRoots(t *testing.T) {
	reg := runtime.NewRoleRegistry(nil, "")

	orch := reg.Resolve("orchestrator")
	if !orch.AllTools || !orch.Delegates || !orch.SpawnsGoals || !orch.Persists || !orch.Interactive {
		t.Errorf("orchestrator = %+v", orch)
	}
	if orch.SystemPrompt != "" {
		t.Errorf("orchestrator body must be empty (falls back to daemon prompt, R-ROLE.3), got %q", orch.SystemPrompt)
	}

	exec := reg.Resolve("executor")
	if !exec.AllTools || !exec.Delegates || exec.SpawnsGoals || exec.Persists || exec.Interactive {
		t.Errorf("executor = %+v", exec)
	}
	if !strings.Contains(exec.SystemPrompt, "sub-agent mode") {
		t.Errorf("executor persona = %q, want the sub-agent stance (R-ROLE.10)", exec.SystemPrompt)
	}

	refl := reg.Resolve("reflection")
	if refl.AllTools || refl.Delegates || refl.SpawnsGoals {
		t.Errorf("reflection = %+v; must be narrowed (docs/roles.md §4)", refl)
	}
	for _, want := range []string{"memory_get", "memory_set", "skill_read"} {
		found := false
		for _, tool := range refl.Tools {
			if tool == want {
				found = true
			}
		}
		if !found {
			t.Errorf("reflection allowlist missing %q", want)
		}
	}

	pursue := reg.Resolve("pursue")
	if !pursue.AllTools || !pursue.Delegates || pursue.SpawnsGoals {
		t.Errorf("pursue = %+v; full toolset + delegation, no goal-spawn", pursue)
	}
}

func TestRegistryUnknownFallsBackToDefaultLeaf(t *testing.T) {
	reg := runtime.NewRoleRegistry(nil, "")
	if got := reg.Resolve("no-such-role"); got.Name != "executor" {
		t.Errorf("unknown role resolved to %q, want executor (R-ROLE.9)", got.Name)
	}
	if got := reg.Resolve(""); got.Name != "executor" {
		t.Errorf("empty role resolved to %q, want executor", got.Name)
	}

	custom := runtime.NewRoleRegistry(nil, "report-writer")
	if got := custom.Resolve(""); got.Name != "report-writer" {
		t.Errorf("roles.default_leaf not honored: got %q", got.Name)
	}
}

// Gate 3 (docs/roles.md §12): an agent-authored skill with a role block is
// purely restrictive — structural flags are ignored and forced to leaf
// defaults (R-ROLE.7).
func TestRegistryAgentAuthoredRoleIsPurelyRestrictive(t *testing.T) {
	store := newRoleTestStore(t)
	err := store.SkillUpsert(memory.Skill{
		Name:        "auditor",
		Description: "read-only auditor",
		Source:      memory.SkillSourceAgent,
		Content:     "---\nrole:\n  tools: [memory_get, memory_list]\n  delegates: true\n  spawns_goals: true\n  persists: true\n  interactive: true\n  profile: [active]\n---\nAudit persona.",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	reg := runtime.NewRoleRegistry(store, "")
	role := reg.Resolve("auditor")
	if role.Name != "auditor" {
		t.Fatalf("agent role not resolved from store: got %q", role.Name)
	}
	if role.Delegates || role.SpawnsGoals || role.Persists || role.Interactive || role.Profile != nil {
		t.Errorf("structural flags must be forced to leaf defaults (R-ROLE.7): %+v", role)
	}
	if role.AllTools || len(role.Tools) != 2 {
		t.Errorf("allowlist not honored: %+v", role)
	}
	if !strings.Contains(role.SystemPrompt, "Audit persona") {
		t.Errorf("persona = %q", role.SystemPrompt)
	}

	// A wildcard agent role stays a leaf too (gate 3's tools: "*" variant).
	err = store.SkillUpsert(memory.Skill{
		Name:    "escalator",
		Source:  memory.SkillSourceAgent,
		Content: "---\nrole:\n  tools: \"*\"\n  persists: true\n---\n",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if got := reg.Resolve("escalator"); got.Persists || !got.AllTools {
		t.Errorf("wildcard agent role = %+v, want AllTools leaf", got)
	}
}

// An agent-authored skill without a role block is not a role: it resolves to
// the default leaf.
func TestRegistryAgentSkillWithoutRoleBlockIsNotARole(t *testing.T) {
	store := newRoleTestStore(t)
	if err := store.SkillUpsert(memory.Skill{
		Name: "notes", Source: memory.SkillSourceAgent, Content: "just knowledge",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	reg := runtime.NewRoleRegistry(store, "")
	if got := reg.Resolve("notes"); got.Name != "executor" {
		t.Errorf("plain skill resolved as role %q, want executor fallback", got.Name)
	}
}

// ResolveLeaf ignores root-only flags even for built-in roots (R-ROLE.9).
func TestResolveLeafForcesLeafFlags(t *testing.T) {
	reg := runtime.NewRoleRegistry(nil, "")
	leaf := reg.ResolveLeaf("orchestrator")
	if leaf.SpawnsGoals || leaf.Persists || leaf.Interactive || leaf.Profile != nil {
		t.Errorf("ResolveLeaf(orchestrator) = %+v; root-only flags must be dropped", leaf)
	}
	// executor keeps Delegates (backward compat: a depth-1 leaf may sub-delegate).
	if exec := reg.ResolveLeaf("executor"); !exec.Delegates {
		t.Error("executor leaf must keep Delegates=true (docs/roles.md §4)")
	}
	if sd := reg.ResolveLeaf("software-dev"); sd.Delegates {
		t.Error("coarse leaf roles must not delegate")
	}
}

// agent.DefaultRoleEnum — the static fallback used by storeless callers —
// stays in sync with the built-in leaf roles (guards against description/
// registry drift). A live daemon renders the enum instead; see
// TestRenderedRoleEnumCoversStoreRoles.
func TestDefaultRoleEnumMentionsBuiltinLeafRoles(t *testing.T) {
	var schema string
	for _, def := range agent.InterceptedDefs {
		if def.Name == "run_agent" {
			schema = string(def.InputSchema)
		}
	}
	if schema == "" {
		t.Fatal("run_agent def not found")
	}
	if !strings.Contains(schema, `"role"`) {
		t.Fatal("run_agent schema has no role field (R-ROLE.8)")
	}
	// Derive the expected set from the registry itself rather than a hardcoded
	// list: adding a new built-in leaf role skill then forces the run_agent
	// description to mention it (or this test fails), closing the drift window.
	reg := runtime.NewRoleRegistry(nil, "")
	leaves := reg.LeafRoles()
	if len(leaves) == 0 {
		t.Fatal("registry reports no leaf roles")
	}
	for _, name := range leaves {
		if reg.Resolve(name).Name != name {
			t.Errorf("built-in leaf role %q missing from registry", name)
		}
		if !strings.Contains(schema, name) {
			t.Errorf("run_agent role description does not mention leaf role %q", name)
		}
	}
}

// A store-backed role skill reaches the delegation schemas the model actually
// sees (R-ROLE.8). Before the enum was rendered from the live registry, such a
// role resolved correctly but was never advertised — it only worked if the
// model happened to guess its name. The nil-store fallback cannot catch this,
// so this test uses a real store.
func TestRenderedRoleEnumCoversStoreRoles(t *testing.T) {
	store := newRoleTestStore(t)
	if err := store.SkillUpsert(memory.Skill{
		Name:        "data-wrangler",
		Description: `Clean and reshape datasets — "files" and shell, no network.`,
		Content:     "---\nname: data-wrangler\nrole:\n  tools: [shell, read_file]\n---\n\nWrangle data.",
		Source:      memory.SkillSourceAgent,
	}); err != nil {
		t.Fatalf("seed role skill: %v", err)
	}

	reg := runtime.NewRoleRegistry(store, "")
	enum := reg.RoleEnum()
	if !strings.Contains(enum, "data-wrangler") {
		t.Fatalf("rendered enum omits store-backed role: %q", enum)
	}
	// Built-ins must survive alongside it.
	if !strings.Contains(enum, "executor") {
		t.Errorf("rendered enum dropped the built-in executor: %q", enum)
	}

	var checked int
	for _, def := range agent.SubAgentDefs(enum) {
		if def.Name != "run_agent" && def.Name != "run_agents" {
			continue
		}
		checked++
		schema := string(def.InputSchema)
		if !strings.Contains(schema, "data-wrangler") {
			t.Errorf("%s schema omits store-backed role (R-ROLE.8)", def.Name)
		}
		// The description above carries embedded quotes on purpose: the role
		// property is spliced into raw JSON, so it must be escaped, not concatenated.
		if !json.Valid(def.InputSchema) {
			t.Errorf("%s schema is not valid JSON: %s", def.Name, schema)
		}
	}
	if checked != 2 {
		t.Errorf("checked %d delegation schemas, want 2 (run_agent and run_agents)", checked)
	}
}

// The rendered enum is stable across calls: it feeds a tool schema, so a
// nondeterministic order would churn the prompt on every build.
func TestRoleEnumIsDeterministic(t *testing.T) {
	store := newRoleTestStore(t)
	for _, name := range []string{"zeta-role", "alpha-role", "mid-role"} {
		if err := store.SkillUpsert(memory.Skill{
			Name:        name,
			Description: "Test role " + name,
			Content:     "---\nname: " + name + "\nrole:\n  tools: [read_file]\n---\n\nBody.",
			Source:      memory.SkillSourceAgent,
		}); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	reg := runtime.NewRoleRegistry(store, "")
	first := reg.RoleEnum()
	for i := range 8 {
		if got := reg.RoleEnum(); got != first {
			t.Fatalf("role enum unstable on call %d:\n first: %s\n  got: %s", i+2, first, got)
		}
	}
	// Store roles are sorted among themselves.
	a, m, z := strings.Index(first, "alpha-role"), strings.Index(first, "mid-role"), strings.Index(first, "zeta-role")
	if a >= m || m >= z {
		t.Errorf("store roles not sorted by name: %s", first)
	}
}

func TestAnalystRoleLoads(t *testing.T) {
	reg := runtime.NewRoleRegistry(nil, "")

	role := reg.Resolve(runtime.AnalystRole)
	if role.SystemPrompt == "" {
		t.Error("the analyst role is empty or malformed")
	}

	if role.AllTools == true {
		t.Error("the analyst role must not have access to tools")
	}

	if len(role.Tools) != 0 {
		t.Error("the analyst role must have no tools")
	}
}
