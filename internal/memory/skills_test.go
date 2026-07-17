package memory_test

import (
	"testing"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

func TestSkillUpsertGetListDelete(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}

	// Missing skill → (_, false, nil).
	if _, ok, err := store.SkillGet("nope"); err != nil || ok {
		t.Fatalf("missing skill: ok=%v err=%v, want false/nil", ok, err)
	}

	sk := memory.Skill{
		Name:        "git-workflow",
		Description: "how to branch and commit",
		Tags:        []string{"git", "vcs"},
		Content:     "# Git\n...",
		Source:      "builtin",
	}
	if err := store.SkillUpsert(sk); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.SkillGet("git-workflow")
	if err != nil || !ok {
		t.Fatalf("SkillGet: ok=%v err=%v", ok, err)
	}
	if got.Description != sk.Description || len(got.Tags) != 2 || got.Source != "builtin" {
		t.Errorf("skill round-trip = %+v", got)
	}

	// Upsert replaces by name.
	sk.Description = "updated"
	if err := store.SkillUpsert(sk); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := store.SkillGet("git-workflow"); got.Description != "updated" {
		t.Errorf("upsert did not replace: %q", got.Description)
	}

	// A second, agent-sourced skill; SkillNamesBySource filters by source.
	if err := store.SkillUpsert(memory.Skill{Name: "my-note", Source: "agent"}); err != nil {
		t.Fatal(err)
	}
	builtin, err := store.SkillNamesBySource("builtin")
	if err != nil {
		t.Fatal(err)
	}
	if len(builtin) != 1 || builtin[0] != "git-workflow" {
		t.Errorf("SkillNamesBySource(builtin) = %v, want [git-workflow]", builtin)
	}
	if all, _ := store.SkillList(); len(all) != 2 {
		t.Errorf("SkillList = %d, want 2", len(all))
	}

	// Delete removes one.
	if err := store.SkillDelete("my-note"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := store.SkillGet("my-note"); ok {
		t.Error("skill should be gone after delete")
	}
}
