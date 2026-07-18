package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"nine/internal/embed"
	"nine/internal/memory"
	"nine/skills"
)

// SeedSkills loads the built-in skills embedded in the binary into the store,
// marking them as immutable (source=builtin) and (re-)embedding each
// description for semantic retrieval. It runs on every boot: the binary is the
// single source of truth for built-in skills, so editing a skill file and
// rebuilding updates the store, and removing one prunes it (agent-authored
// skills are left untouched). embedder may be nil to skip embedding.
func SeedSkills(store *memory.Store, embedder embed.Embedder) error {
	defaults, err := skills.Defaults()
	if err != nil {
		return fmt.Errorf("load built-in skills: %w", err)
	}

	seeded := make(map[string]bool, len(defaults))
	for _, sk := range defaults {
		seeded[sk.Name] = true
		if err := store.SkillUpsert(memory.Skill{
			Name:        sk.Name,
			Description: sk.Description,
			Tags:        sk.Tags,
			Content:     sk.Content,
			Source:      memory.SkillSourceBuiltin,
		}); err != nil {
			return fmt.Errorf("seed skill %q: %w", sk.Name, err)
		}
		if embedder != nil && sk.Description != "" {
			if vec, err := embedder.Embed(context.Background(), sk.Description); err == nil {
				store.VectorStore("skills:"+sk.Name, "skills", sk.Name, vec) //nolint:errcheck
			}
		}
	}

	// Prune built-ins that no longer ship in the binary. Agent-authored skills
	// (source=agent) are never touched here.
	existing, err := store.SkillNamesBySource(memory.SkillSourceBuiltin)
	if err != nil {
		return fmt.Errorf("list built-in skills: %w", err)
	}
	for _, name := range existing {
		if seeded[name] {
			continue
		}
		if err := store.SkillDelete(name); err != nil {
			return fmt.Errorf("prune skill %q: %w", name, err)
		}
		store.VectorDelete("skills:" + name) //nolint:errcheck
	}

	slog.Info("seeded built-in skills", "count", len(defaults))
	return nil
}

// SeedUserSkills loads operator-supplied skills and roles from dir into the
// store as source=user, and prunes user skills whose files are gone. Like
// SeedSkills it runs on every boot: the directory is the source of truth, so
// editing a file and restarting updates the store and deleting one removes it.
// Agent-authored and built-in skills are never touched.
//
// Discovery is boot-only — there is no watcher. A malformed or colliding file
// is skipped with a logged reason rather than failing the boot, so one typo in
// one skill cannot take the daemon down. embedder may be nil to skip embedding.
//
// An empty or nonexistent dir is a no-op, so this is safe to call
// unconditionally.
func SeedUserSkills(store *memory.Store, embedder embed.Embedder, dir string) error {
	// An unconfigured or absent directory means the operator is not using the
	// feature: do nothing, and in particular do not prune — there is no
	// desired state to reconcile against. An existing but *empty* directory is
	// different: it says "no user skills", so pruning must still run below.
	if dir == "" {
		return nil
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}

	loaded, err := skills.LoadDir(dir)
	if err != nil {
		return fmt.Errorf("read user skills dir %q: %w", dir, err)
	}

	builtins, err := skills.BuiltinNames()
	if err != nil {
		return fmt.Errorf("load built-in skill names: %w", err)
	}

	seeded := make(map[string]bool, len(loaded))
	var skipped int
	for _, l := range loaded {
		switch {
		case !l.Valid():
			skipped++
			for _, e := range l.Errs {
				slog.Error("skipping invalid user skill", "path", l.Path, "err", e)
			}
			continue
		case builtins[l.Name]:
			// R-SKILL.2: built-ins win. Overriding one is not supported —
			// the next boot would reseed over it anyway.
			skipped++
			slog.Error("skipping user skill: name collides with a built-in",
				"path", l.Path, "name", l.Name)
			continue
		case seeded[l.Name]:
			// Two files claiming one name (e.g. foo.md and roles/foo.md).
			skipped++
			slog.Error("skipping user skill: duplicate name", "path", l.Path, "name", l.Name)
			continue
		}

		seeded[l.Name] = true
		if err := store.SkillUpsert(memory.Skill{
			Name:        l.Name,
			Description: l.Description,
			Tags:        l.Tags,
			Content:     l.Content,
			Source:      memory.SkillSourceUser,
		}); err != nil {
			return fmt.Errorf("seed user skill %q: %w", l.Name, err)
		}
		if embedder != nil && l.Description != "" {
			if vec, err := embedder.Embed(context.Background(), l.Description); err == nil {
				store.VectorStore("skills:"+l.Name, "skills", l.Name, vec) //nolint:errcheck
			}
		}
	}

	// Prune user skills whose files are gone. Scoped to source=user, so
	// built-in and agent-authored skills are unaffected.
	existing, err := store.SkillNamesBySource(memory.SkillSourceUser)
	if err != nil {
		return fmt.Errorf("list user skills: %w", err)
	}
	for _, name := range existing {
		if seeded[name] {
			continue
		}
		if err := store.SkillDelete(name); err != nil {
			return fmt.Errorf("prune user skill %q: %w", name, err)
		}
		store.VectorDelete("skills:" + name) //nolint:errcheck
	}

	slog.Info("seeded user skills", "dir", dir, "count", len(seeded), "skipped", skipped)
	return nil
}
