// Package skills holds Nine's built-in skills, embedded into the binary at
// build time. These are the curated, immutable defaults: they are seeded into
// the memory store on every boot and cannot be modified at runtime. Nine can
// still author and update its own skills — those live only in the store.
//
// A skill MAY carry a `role:` frontmatter block (docs/roles.md R-ROLE.1); such
// a skill is also a role: its body is the role's persona and the block declares
// the role's tool boundary and structural wiring. Built-in role skills live
// under roles/.
//
// To change a built-in skill, edit its markdown file here and rebuild.
package skills

import (
	"embed"
	"strings"
)

//go:embed *.md roles/*.md
var defaultsFS embed.FS

// Skill is one parsed built-in skill: frontmatter metadata plus markdown body.
type Skill struct {
	Name        string
	Description string
	Tags        []string
	Content     string
	Role        *RoleSpec // non-nil when the skill declares a role: block
}

// RoleSpec is the parsed `role:` frontmatter block of a skill (docs/roles.md
// R-ROLE.1). A skill carrying one is also a role: the skill body becomes the
// role's persona (R-ROLE.3) and this block declares its tool boundary and
// structural wiring. Structural flags are honored only for built-in skills
// (R-ROLE.7).
type RoleSpec struct {
	AllTools    bool     // tools: "*" or tools key omitted (R-ROLE.2)
	Tools       []string // strict allowlist when AllTools is false
	Delegates   bool     // gets run_agent/run_agents/workflow_*/goal_* tools
	SpawnsGoals bool     // gets the background pursue-session spawn fn
	Persists    bool     // checkpointing worker (sessionWorker) vs ephemeral leaf
	Interactive bool     // eligible for HITL tools (effective only with an interactive caller)
	Profile     []string // aspect kinds; empty ⇒ ephemeral leaf, no stages
}

// Defaults parses and returns all embedded built-in skills, including the
// role skills under roles/.
func Defaults() ([]Skill, error) {
	var out []Skill
	for _, dir := range []string{".", "roles"} {
		entries, err := defaultsFS.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			path := e.Name()
			if dir != "." {
				path = dir + "/" + e.Name()
			}
			b, err := defaultsFS.ReadFile(path)
			if err != nil {
				return nil, err
			}
			s := Parse(string(b))
			if s.Name == "" {
				s.Name = strings.TrimSuffix(e.Name(), ".md")
			}
			out = append(out, s)
		}
	}
	return out, nil
}

// BuiltinNames returns the set of built-in skill names. User-supplied skills
// may not claim one of these: built-ins are reseeded from the binary on every
// boot, so a same-named user skill would be silently clobbered.
func BuiltinNames() (map[string]bool, error) {
	defaults, err := Defaults()
	if err != nil {
		return nil, err
	}
	names := make(map[string]bool, len(defaults))
	for _, s := range defaults {
		names[s.Name] = true
	}
	return names, nil
}

// Parse splits a skill markdown document into frontmatter metadata (name,
// description, tags, optional role block) and its body. A document without a
// leading `---` block is treated as all body. A malformed role block never
// fails the parse — unrecognized lines are skipped and the skill degrades to
// a plain knowledge skill (docs/roles.md §10).
func Parse(raw string) Skill {
	var s Skill
	if !strings.HasPrefix(raw, "---\n") {
		s.Content = raw
		return s
	}
	front, after, ok := strings.Cut(raw[4:], "\n---")
	if !ok {
		s.Content = raw
		return s
	}
	inRole := false
	sawTools := false
	for _, line := range joinBracketLines(strings.Split(front, "\n")) {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indented := len(line) > 0 && (line[0] == ' ' || line[0] == '\t')
		if !indented {
			inRole = false
		}
		k, v, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if indented {
			if inRole && s.Role != nil {
				switch k {
				case "tools":
					sawTools = true
					if isWildcard(v) {
						s.Role.AllTools = true
					} else {
						s.Role.Tools = parseList(v)
					}
				case "delegates":
					s.Role.Delegates = v == "true"
				case "spawns_goals":
					s.Role.SpawnsGoals = v == "true"
				case "persists":
					s.Role.Persists = v == "true"
				case "interactive":
					s.Role.Interactive = v == "true"
				case "profile":
					s.Role.Profile = parseList(v)
				}
			}
			continue
		}
		switch k {
		case "name":
			s.Name = v
		case "description":
			s.Description = v
		case "tags":
			s.Tags = parseList(v)
		case "role":
			s.Role = &RoleSpec{}
			inRole = true
			sawTools = false
		}
	}
	// tools omitted with a role block present means all tools (R-ROLE.2).
	if s.Role != nil && !sawTools {
		s.Role.AllTools = true
	}
	s.Content = strings.TrimPrefix(after, "\n")
	return s
}

// isWildcard reports whether v is the `"*"` tools value (R-ROLE.2), allowing
// bare and quoted forms.
func isWildcard(v string) bool {
	return strings.Trim(v, `"'`) == "*"
}

// parseList parses a `[a, b, c]` frontmatter list into its trimmed elements.
func parseList(v string) []string {
	v = strings.Trim(v, "[] ")
	var out []string
	for t := range strings.SplitSeq(v, ",") {
		if t = strings.Trim(strings.TrimSpace(t), `"'`); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// joinBracketLines merges frontmatter lines whose `[…]` list value spans
// multiple lines into a single line, so parseList sees the whole list.
func joinBracketLines(lines []string) []string {
	var out []string
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if open := strings.Index(line, "["); open != -1 && !strings.Contains(line[open:], "]") {
			for i+1 < len(lines) {
				i++
				line += " " + strings.TrimSpace(lines[i])
				if strings.Contains(lines[i], "]") {
					break
				}
			}
		}
		out = append(out, line)
	}
	return out
}
