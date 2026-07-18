package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"nine/internal/config"
	"nine/skills"
)

// SkillValidate checks operator-authored skill and role files against the
// format Nine seeds from (spec/contracts/skills.md R-SKILL.1), reporting every
// problem per file. It runs the same skills.Validate the daemon runs at boot,
// so a file that passes here is a file that will seed.
//
// With no path it validates the configured [skills].user_dir. A path may be a
// single .md file or a directory.
func (c *CLI) SkillValidate(cfg *config.Config, path string) error {
	if path == "" {
		path = cfg.Skills.UserDir
		if path == "" {
			return fmt.Errorf("no path given and [skills].user_dir is not set in nine.toml")
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		return err
	}

	var loaded []skills.LoadedSkill
	if info.IsDir() {
		loaded, err = skills.LoadDir(path)
		if err != nil {
			return err
		}
		if len(loaded) == 0 {
			fmt.Fprintf(c.Out, "No skill files found in %s\n", path)
			fmt.Fprintf(c.Out, "Expected %s/*.md, with role skills under %s/roles/.\n", path, path)
			return nil
		}
	} else {
		s, err := skills.LoadFile(path)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		loaded = []skills.LoadedSkill{{Skill: s, Path: path, Errs: skills.Validate(s)}}
	}

	// Built-in collisions are a seed-time rejection, so check them here too —
	// otherwise a file passes validation but silently never loads.
	builtins, err := skills.BuiltinNames()
	if err != nil {
		return err
	}
	seen := make(map[string]string, len(loaded))

	sort.Slice(loaded, func(i, j int) bool { return loaded[i].Path < loaded[j].Path })

	var bad int
	for _, l := range loaded {
		errs := append([]error(nil), l.Errs...)
		if l.Name != "" {
			if builtins[l.Name] {
				errs = append(errs, fmt.Errorf("name %q collides with a built-in skill; built-ins win, so this file would never load", l.Name))
			} else if prev, dup := seen[l.Name]; dup {
				errs = append(errs, fmt.Errorf("name %q is already claimed by %s", l.Name, prev))
			} else {
				seen[l.Name] = l.Path
			}
		}

		rel := relPath(l.Path)
		if len(errs) == 0 {
			kind := "skill"
			if l.Role != nil {
				kind = "role"
			}
			fmt.Fprintf(c.Out, "  ok    %s  (%s: %s)\n", rel, kind, l.Name)
			continue
		}
		bad++
		fmt.Fprintf(c.Out, "  FAIL  %s\n", rel)
		for _, e := range errs {
			fmt.Fprintf(c.Out, "          %v\n", e)
		}
	}

	fmt.Fprintf(c.Out, "\n%d file(s) checked, %d invalid.\n", len(loaded), bad)
	if bad > 0 {
		// Invalid files are skipped at boot, not fatal — say so, so the user
		// knows the daemon will still start.
		fmt.Fprintf(c.Out, "Invalid files are skipped at boot; the daemon still starts without them.\n")
		return fmt.Errorf("%d skill file(s) failed validation", bad)
	}
	return nil
}

// relPath shortens a path against the working directory for readable output,
// falling back to the original when that is not possible.
func relPath(p string) string {
	wd, err := os.Getwd()
	if err != nil {
		return p
	}
	rel, err := filepath.Rel(wd, p)
	if err != nil || len(rel) > len(p) {
		return p
	}
	return rel
}
