package skills

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// NamePattern is the required shape of a skill name: lowercase letters,
// digits, and single interior hyphens. It is also the file's basename, so it
// stays portable across filesystems and safe to use as a store key.
var NamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// MaxDescriptionLen bounds a description. Descriptions are surfaced in
// skill_list and, for roles, concatenated into the run_agent tool schema
// (R-ROLE.8), so an essay here costs every delegating turn context it needs
// for actual work.
const MaxDescriptionLen = 200

// KnownRoutineKinds are the routine kinds a role's profile may name. A profile
// naming anything else would silently never run.
var KnownRoutineKinds = []string{"active", "pursue", "idle-reflection"}

// ErrNotMarkdown is returned by LoadFile for a path that is not a .md file.
var ErrNotMarkdown = errors.New("not a .md file")

// Validate checks a parsed skill against the documented format
// (spec/contracts/skills.md R-SKILL.1). It returns every problem found, not
// just the first, so a user fixing a file sees the whole list at once.
//
// Validation applies to operator-supplied skills only. Parse itself stays
// infallible by design (docs/roles.md §10): a malformed built-in degrades to a
// plain knowledge skill rather than breaking boot. Validate is the stricter
// gate we can apply to files a human is actively editing.
func Validate(s Skill) []error {
	var errs []error

	switch {
	case s.Name == "":
		errs = append(errs, errors.New("name is required (frontmatter `name:` or the filename)"))
	case !NamePattern.MatchString(s.Name):
		errs = append(errs, fmt.Errorf("name %q must be lowercase letters, digits, and single hyphens (e.g. \"data-wrangler\")", s.Name))
	}

	switch {
	case strings.TrimSpace(s.Description) == "":
		// Without a description the skill is invisible: skill_list shows it as
		// a bare name and there is nothing to embed for retrieval.
		errs = append(errs, errors.New("description is required — it is what skill_list shows and what gets embedded for retrieval"))
	case len(s.Description) > MaxDescriptionLen:
		errs = append(errs, fmt.Errorf("description is %d characters; keep it under %d", len(s.Description), MaxDescriptionLen))
	}

	// A role skill MAY have an empty body: that is the documented way to say
	// "use the daemon's system prompt as this role's persona" (R-ROLE.3), and
	// three built-in roles rely on it. A non-role skill with no body is just
	// an empty note.
	if s.Role == nil && strings.TrimSpace(s.Content) == "" {
		errs = append(errs, errors.New("body is empty — a skill needs instructions under the frontmatter"))
	}

	if s.Role != nil {
		errs = append(errs, validateRole(*s.Role)...)
	}
	return errs
}

// validateRole checks the `role:` frontmatter block. Tool names are
// deliberately NOT checked here: the core-intercepted tools (memory_*,
// skill_*, file_*) are not registered with the plugin manager, so any
// name-based check at seed time would reject valid allowlists. An unknown tool
// in an allowlist is already harmless — it is dropped at build time.
func validateRole(r RoleSpec) []error {
	var errs []error

	// An empty tools list is NOT an error: a tool-less role is a legitimate
	// pattern (the built-in analyst role is exactly that — a pure reasoning
	// persona with `tools: ""`).
	for _, tool := range r.Tools {
		if strings.ContainsAny(tool, " \t") {
			errs = append(errs, fmt.Errorf("tool name %q contains whitespace — write the list as [a, b, c]", tool))
		}
	}
	for _, kind := range r.Profile {
		if !slices.Contains(KnownRoutineKinds, kind) {
			errs = append(errs, fmt.Errorf("profile names unknown routine kind %q (known: %s)", kind, strings.Join(KnownRoutineKinds, ", ")))
		}
	}
	return errs
}

// LoadFile reads and parses one skill file, defaulting the name to the file's
// basename when the frontmatter omits it. It does not validate; callers pair
// it with Validate so they control how failures are reported.
func LoadFile(path string) (Skill, error) {
	if !strings.HasSuffix(path, ".md") {
		return Skill{}, ErrNotMarkdown
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Skill{}, err
	}
	s := Parse(string(b))
	if s.Name == "" {
		s.Name = strings.TrimSuffix(filepath.Base(path), ".md")
	}
	return s, nil
}

// isReadme reports whether name is a README, which LoadDir skips. Documenting
// your own skills directory is the obvious thing to do, and without this the
// README would parse as a nameless, descriptionless skill and log a validation
// error on every boot.
func isReadme(name string) bool {
	return strings.EqualFold(name, "README.md")
}

// LoadedSkill is one skill read from a directory, with the path it came from
// so callers can report problems against a file the user can open.
type LoadedSkill struct {
	Skill
	Path string
	// Errs are the validation failures for this file; empty means valid.
	Errs []error
}

// Valid reports whether the skill passed validation.
func (l LoadedSkill) Valid() bool { return len(l.Errs) == 0 }

// LoadDir reads skills from dir, mirroring the built-in layout: `*.md` at the
// top level plus `roles/*.md`. Every file found is returned — valid or not —
// so the caller can log each rejection against its path. A missing dir is not
// an error; it means the operator has not created one.
func LoadDir(dir string) ([]LoadedSkill, error) {
	if dir == "" {
		return nil, nil
	}
	info, err := os.Stat(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}

	var out []LoadedSkill
	for _, sub := range []string{".", "roles"} {
		path := dir
		if sub != "." {
			path = filepath.Join(dir, sub)
		}
		entries, err := os.ReadDir(path)
		if os.IsNotExist(err) {
			continue // no roles/ subdirectory is fine
		}
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") || isReadme(e.Name()) {
				continue
			}
			full := filepath.Join(path, e.Name())
			s, err := LoadFile(full)
			if err != nil {
				out = append(out, LoadedSkill{Path: full, Errs: []error{err}})
				continue
			}
			out = append(out, LoadedSkill{Skill: s, Path: full, Errs: Validate(s)})
		}
	}
	return out, nil
}
