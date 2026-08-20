package skills_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nine/skills"
)

func TestValidateAcceptsWellFormedSkill(t *testing.T) {
	s := skills.Parse("---\nname: deploy-checklist\ndescription: Steps before shipping.\ntags: [ops]\n---\n\nBody.\n")
	if errs := skills.Validate(s); len(errs) != 0 {
		t.Errorf("Validate returned %v, want no errors", errs)
	}
}

func TestValidateRejects(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"missing description", "---\nname: a-skill\n---\n\nBody.\n", "description is required"},
		{"empty body on a non-role skill", "---\nname: a-skill\ndescription: Something.\n---\n", "body is empty"},
		{"uppercase name", "---\nname: BadName\ndescription: Something.\n---\n\nBody.\n", "must be lowercase"},
		{"underscore name", "---\nname: bad_name\ndescription: Something.\n---\n\nBody.\n", "must be lowercase"},
		{"long description", "---\nname: a-skill\ndescription: " + strings.Repeat("x", 250) + "\n---\n\nBody.\n", "characters; keep it under"},
		{"bad aspect kind", "---\nname: a-role\ndescription: A role.\nrole:\n  tools: [shell]\n  profile: [nonsense]\n---\n\nBody.\n", "unknown aspect kind"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := skills.Validate(skills.Parse(tc.raw))
			if len(errs) == 0 {
				t.Fatalf("Validate accepted an invalid skill")
			}
			var joined string
			for _, e := range errs {
				joined += e.Error() + "\n"
			}
			if !strings.Contains(joined, tc.want) {
				t.Errorf("errors = %q, want one containing %q", joined, tc.want)
			}
		})
	}
}

// An empty role body is legitimate: it means "fall back to the daemon's system
// prompt" (R-ROLE.3), which the built-in orchestrator/pursue/reflection roles
// all rely on.
func TestValidateAcceptsEmptyRoleBody(t *testing.T) {
	s := skills.Parse("---\nname: a-root\ndescription: Uses the daemon prompt.\nrole:\n  tools: \"*\"\n---\n")
	if errs := skills.Validate(s); len(errs) != 0 {
		t.Errorf("Validate rejected an empty role body: %v", errs)
	}
}

// A tool-less role is legitimate — the built-in analyst role is exactly that.
func TestValidateAcceptsToolessRole(t *testing.T) {
	s := skills.Parse("---\nname: thinker\ndescription: Reasons, no tools.\nrole:\n  tools: \"\"\n---\n\nThink.\n")
	if errs := skills.Validate(s); len(errs) != 0 {
		t.Errorf("Validate rejected a tool-less role: %v", errs)
	}
}

// Every built-in skill must pass the validator we hold users to — otherwise we
// are shipping examples that our own format would reject.
func TestBuiltinSkillsAreValid(t *testing.T) {
	defaults, err := skills.Defaults()
	if err != nil {
		t.Fatalf("Defaults: %v", err)
	}
	if len(defaults) == 0 {
		t.Fatal("no built-in skills found")
	}
	for _, s := range defaults {
		if errs := skills.Validate(s); len(errs) != 0 {
			t.Errorf("built-in skill %q fails validation: %v", s.Name, errs)
		}
	}
}

func TestLoadDirReadsTopLevelAndRoles(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a-skill.md", "---\nname: a-skill\ndescription: A skill.\n---\n\nBody.\n")
	write("roles/a-role.md", "---\nname: a-role\ndescription: A role.\nrole:\n  tools: [shell]\n---\n\nBody.\n")
	write("ignored.txt", "not markdown")

	loaded, err := skills.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("loaded %d skills, want 2 (non-.md must be ignored): %+v", len(loaded), loaded)
	}
	byName := map[string]skills.LoadedSkill{}
	for _, l := range loaded {
		if !l.Valid() {
			t.Errorf("%s should be valid: %v", l.Path, l.Errs)
		}
		byName[l.Name] = l
	}
	if byName["a-role"].Role == nil {
		t.Error("roles/a-role.md did not parse as a role")
	}
	if byName["a-skill"].Role != nil {
		t.Error("a-skill.md must not be a role")
	}
}

// The name falls back to the filename, matching how built-ins are loaded.
func TestLoadFileDefaultsNameToFilename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "from-filename.md")
	if err := os.WriteFile(path, []byte("---\ndescription: No name key.\n---\n\nBody.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := skills.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if s.Name != "from-filename" {
		t.Errorf("name = %q, want %q", s.Name, "from-filename")
	}
}

// A missing directory is not an error: it means the operator has not created one.
func TestLoadDirMissingIsNotAnError(t *testing.T) {
	loaded, err := skills.LoadDir(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Errorf("LoadDir on a missing dir returned %v, want nil", err)
	}
	if len(loaded) != 0 {
		t.Errorf("loaded %d skills from a missing dir", len(loaded))
	}
}
