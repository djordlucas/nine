package runtime

import (
	"context"
	"fmt"
	"log/slog"

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
