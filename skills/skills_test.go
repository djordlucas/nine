package skills

import (
	"strings"
	"testing"
)

func TestParsePlainSkillUnchanged(t *testing.T) {
	s := Parse("---\nname: plain\ndescription: a plain skill\ntags: [a, b]\n---\n\nBody text.\n")
	if s.Name != "plain" || s.Description != "a plain skill" {
		t.Errorf("name/description = %q/%q", s.Name, s.Description)
	}
	if len(s.Tags) != 2 || s.Tags[0] != "a" || s.Tags[1] != "b" {
		t.Errorf("tags = %v", s.Tags)
	}
	if s.Role != nil {
		t.Error("plain skill parsed a role block")
	}
	if !strings.Contains(s.Content, "Body text.") {
		t.Errorf("content = %q", s.Content)
	}
}

func TestParseRoleWildcardTools(t *testing.T) {
	s := Parse("---\nname: r\nrole:\n  tools: \"*\"\n  delegates: true\n---\nPersona.\n")
	if s.Role == nil {
		t.Fatal("role block not parsed")
	}
	if !s.Role.AllTools {
		t.Error("tools: \"*\" should set AllTools (R-ROLE.2)")
	}
	if !s.Role.Delegates {
		t.Error("delegates: true not parsed")
	}
	if strings.TrimSpace(s.Content) != "Persona." {
		t.Errorf("content = %q", s.Content)
	}
}

func TestParseRoleOmittedToolsMeansAllTools(t *testing.T) {
	s := Parse("---\nname: r\nrole:\n  delegates: false\n---\n")
	if s.Role == nil || !s.Role.AllTools {
		t.Error("omitted tools key with role block should mean AllTools (R-ROLE.2)")
	}
}

func TestParseRoleMultiLineAllowlistAndFlags(t *testing.T) {
	raw := `---
name: coder
description: writes code
tags: [role, dev]
role:
  tools: [shell, read_file, write_file,
          memory_get, memory_set]
  # a comment line inside the block
  delegates: false
  spawns_goals: false
  persists: true
  interactive: true
  profile: [active, pursue]
---

# Coder

Do the thing.
`
	s := Parse(raw)
	if s.Role == nil {
		t.Fatal("role block not parsed")
	}
	if s.Role.AllTools {
		t.Error("explicit list must not set AllTools")
	}
	want := []string{"shell", "read_file", "write_file", "memory_get", "memory_set"}
	if len(s.Role.Tools) != len(want) {
		t.Fatalf("tools = %v, want %v", s.Role.Tools, want)
	}
	for i, w := range want {
		if s.Role.Tools[i] != w {
			t.Errorf("tools[%d] = %q, want %q", i, s.Role.Tools[i], w)
		}
	}
	if !s.Role.Persists || !s.Role.Interactive || s.Role.Delegates || s.Role.SpawnsGoals {
		t.Errorf("flags = %+v", *s.Role)
	}
	if len(s.Role.Profile) != 2 || s.Role.Profile[0] != "active" || s.Role.Profile[1] != "pursue" {
		t.Errorf("profile = %v", s.Role.Profile)
	}
	if s.Name != "coder" || len(s.Tags) != 2 {
		t.Errorf("top-level keys disturbed: name=%q tags=%v", s.Name, s.Tags)
	}
}

func TestParseRoleEmptyToolsListIsEmptyAllowlist(t *testing.T) {
	s := Parse("---\nname: r\nrole:\n  tools: []\n---\n")
	if s.Role == nil {
		t.Fatal("role block not parsed")
	}
	if s.Role.AllTools {
		t.Error("tools: [] is an (empty) allowlist, not a wildcard (adr/roles-design.md §10)")
	}
	if len(s.Role.Tools) != 0 {
		t.Errorf("tools = %v, want empty", s.Role.Tools)
	}
}

func TestParseTopLevelKeyEndsRoleBlock(t *testing.T) {
	s := Parse("---\nrole:\n  tools: [memory_get]\nname: after\n---\n")
	if s.Name != "after" {
		t.Errorf("name = %q; a top-level key after the role block must be read", s.Name)
	}
	if s.Role == nil || s.Role.AllTools || len(s.Role.Tools) != 1 {
		t.Errorf("role = %+v", s.Role)
	}
}

func TestDefaultsIncludeBuiltinRoles(t *testing.T) {
	defaults, err := Defaults()
	if err != nil {
		t.Fatalf("Defaults: %v", err)
	}
	roles := map[string]*RoleSpec{}
	for _, sk := range defaults {
		if sk.Role != nil {
			roles[sk.Name] = sk.Role
		}
	}
	for _, name := range []string{"orchestrator", "executor", "reflection", "pursue", "software-dev", "sysadmin", "report-writer"} {
		if roles[name] == nil {
			t.Errorf("built-in role %q missing from embedded defaults", name)
		}
	}
	if r := roles["orchestrator"]; r != nil && (!r.AllTools || !r.Delegates || !r.SpawnsGoals || !r.Persists || !r.Interactive) {
		t.Errorf("orchestrator spec = %+v", *r)
	}
	if r := roles["executor"]; r != nil && (!r.AllTools || !r.Delegates || r.SpawnsGoals || r.Persists || r.Interactive) {
		t.Errorf("executor spec = %+v", *r)
	}
	if r := roles["reflection"]; r != nil && r.AllTools {
		t.Error("reflection must have a narrow allowlist, not AllTools")
	}
	if r := roles["report-writer"]; r != nil {
		for _, tool := range r.Tools {
			if tool == "shell" || tool == "write_file" {
				t.Errorf("report-writer allowlist must not contain %q", tool)
			}
		}
	}
	// The executor body carries the sub-agent persona (R-ROLE.10).
	for _, sk := range defaults {
		if sk.Name == "executor" && !strings.Contains(sk.Content, "sub-agent mode") {
			t.Errorf("executor body must carry the sub-agent persona, got %q", sk.Content)
		}
	}
}
