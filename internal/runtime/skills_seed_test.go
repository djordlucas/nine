package runtime_test

import (
	"os"
	"path/filepath"
	"testing"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
	"nine/internal/runtime"
)

func seedTestStore(t *testing.T) *memory.Store {
	t.Helper()
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// writeSkillFile writes body to <dir>/<rel>, creating parent directories.
func writeSkillFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

const validSkill = `---
name: deploy-checklist
description: Steps to verify before shipping a release build.
tags: [ops]
---

Run the tests, then tag.
`

const validRole = `---
name: data-wrangler
description: Clean and reshape datasets.
role:
  tools: [shell, read_file]
---

Wrangle data.
`

// A valid user skill and role seed as source=user, and an invalid sibling is
// skipped without taking the others (or the boot) down.
func TestSeedUserSkillsSeedsValidAndSkipsInvalid(t *testing.T) {
	store := seedTestStore(t)
	dir := t.TempDir()
	writeSkillFile(t, dir, "deploy-checklist.md", validSkill)
	writeSkillFile(t, dir, "roles/data-wrangler.md", validRole)
	// No description — invalid.
	writeSkillFile(t, dir, "broken.md", "---\nname: broken\n---\n\nBody.\n")

	if err := runtime.SeedUserSkills(store, nil, dir); err != nil {
		t.Fatalf("SeedUserSkills: %v", err)
	}

	for _, name := range []string{"deploy-checklist", "data-wrangler"} {
		sk, found, err := store.SkillGet(name)
		if err != nil || !found {
			t.Fatalf("skill %q not seeded (found=%v err=%v)", name, found, err)
		}
		if sk.Source != memory.SkillSourceUser {
			t.Errorf("skill %q source = %q, want %q", name, sk.Source, memory.SkillSourceUser)
		}
	}
	if _, found, _ := store.SkillGet("broken"); found {
		t.Error("invalid skill was seeded; it must be skipped")
	}
}

// A user file may not claim a built-in's name: built-ins are reseeded every
// boot and would clobber it.
func TestSeedUserSkillsRejectsBuiltinCollision(t *testing.T) {
	store := seedTestStore(t)
	if err := runtime.SeedSkills(store, nil); err != nil {
		t.Fatalf("SeedSkills: %v", err)
	}

	dir := t.TempDir()
	writeSkillFile(t, dir, "git-workflow.md", "---\nname: git-workflow\ndescription: My own version.\n---\n\nMine.\n")
	if err := runtime.SeedUserSkills(store, nil, dir); err != nil {
		t.Fatalf("SeedUserSkills: %v", err)
	}

	sk, found, err := store.SkillGet("git-workflow")
	if err != nil || !found {
		t.Fatalf("built-in git-workflow missing: found=%v err=%v", found, err)
	}
	if sk.Source != memory.SkillSourceBuiltin {
		t.Errorf("source = %q, want the built-in to survive", sk.Source)
	}
	if sk.Description == "My own version." {
		t.Error("user file overwrote a built-in")
	}
}

// The directory is the source of truth: removing a file removes the skill,
// while built-in and agent-authored skills are untouched.
func TestSeedUserSkillsPrunesRemovedFiles(t *testing.T) {
	store := seedTestStore(t)
	dir := t.TempDir()
	writeSkillFile(t, dir, "deploy-checklist.md", validSkill)
	if err := runtime.SeedUserSkills(store, nil, dir); err != nil {
		t.Fatalf("seed 1: %v", err)
	}
	if _, found, _ := store.SkillGet("deploy-checklist"); !found {
		t.Fatal("skill not seeded on first pass")
	}

	// An agent-authored skill must survive the prune.
	if err := store.SkillUpsert(memory.Skill{
		Name: "agent-note", Description: "Written by Nine.", Content: "Body.",
		Source: memory.SkillSourceAgent,
	}); err != nil {
		t.Fatalf("seed agent skill: %v", err)
	}

	if err := os.Remove(filepath.Join(dir, "deploy-checklist.md")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := runtime.SeedUserSkills(store, nil, dir); err != nil {
		t.Fatalf("seed 2: %v", err)
	}

	if _, found, _ := store.SkillGet("deploy-checklist"); found {
		t.Error("user skill survived removal of its file")
	}
	if _, found, _ := store.SkillGet("agent-note"); !found {
		t.Error("agent-authored skill was pruned; only source=user may be pruned")
	}
}

// An unconfigured or absent directory is a no-op — it must not be read as
// "the operator wants zero user skills" and prune the store.
func TestSeedUserSkillsAbsentDirDoesNotPrune(t *testing.T) {
	store := seedTestStore(t)
	dir := t.TempDir()
	writeSkillFile(t, dir, "deploy-checklist.md", validSkill)
	if err := runtime.SeedUserSkills(store, nil, dir); err != nil {
		t.Fatalf("seed: %v", err)
	}

	for _, absent := range []string{"", filepath.Join(dir, "nope")} {
		if err := runtime.SeedUserSkills(store, nil, absent); err != nil {
			t.Fatalf("SeedUserSkills(%q): %v", absent, err)
		}
		if _, found, _ := store.SkillGet("deploy-checklist"); !found {
			t.Fatalf("SeedUserSkills(%q) pruned existing user skills", absent)
		}
	}
}

// R-ROLE.7: a user role is operator-authored, so its structural flags are
// honored like a built-in's — while an agent-authored one stays restrictive.
func TestUserRoleIsTrustedAgentRoleIsNot(t *testing.T) {
	store := seedTestStore(t)
	roleBody := "role:\n  tools: [shell]\n  delegates: true\n  persists: true\n---\n\nBody.\n"

	if err := store.SkillUpsert(memory.Skill{
		Name: "user-role", Description: "Operator role.",
		Content: "---\nname: user-role\n" + roleBody, Source: memory.SkillSourceUser,
	}); err != nil {
		t.Fatalf("upsert user role: %v", err)
	}
	if err := store.SkillUpsert(memory.Skill{
		Name: "agent-role", Description: "Agent role.",
		Content: "---\nname: agent-role\n" + roleBody, Source: memory.SkillSourceAgent,
	}); err != nil {
		t.Fatalf("upsert agent role: %v", err)
	}

	reg := runtime.NewRoleRegistry(store, "")

	user := reg.Resolve("user-role")
	if !user.Delegates || !user.Persists {
		t.Errorf("user role = %+v, want structural flags honored (operator-authored)", user)
	}
	agent := reg.Resolve("agent-role")
	if agent.Delegates || agent.Persists {
		t.Errorf("agent role = %+v, want structural flags forced off (R-ROLE.7)", agent)
	}
	// Both still narrow tools — trust affects structure, never the allowlist.
	if user.AllTools || agent.AllTools {
		t.Error("a role block with an explicit tools list must not grant all tools")
	}
}
