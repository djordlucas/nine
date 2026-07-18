package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"nine/internal/embed"
	"nine/internal/llm"
	"nine/internal/memory"
)

var skillToolDefs = []llm.ToolDef{
	{
		Name:        "skill_list",
		DisplayName: "List Skills",
		Description: "List all available skills with their names, descriptions, and tags.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	},
	{
		Name:        "skill_read",
		DisplayName: "Read Skill",
		Description: "Read the full content of a skill by name.",
		InputSchema: json.RawMessage(`{"type":"object","required":["name"],"properties":{"name":{"type":"string"}}}`),
	},
	{
		Name:        "skill_write",
		DisplayName: "Write Skill",
		Description: "Create or replace one of your own skills (a markdown how-to note). Built-in skills are read-only and cannot be overwritten.",
		InputSchema: json.RawMessage(`{"type":"object","required":["name","description","content"],"properties":{"name":{"type":"string"},"description":{"type":"string"},"tags":{"type":"array","items":{"type":"string"}},"content":{"type":"string"}}}`),
	},
	{
		Name:        "skill_modify",
		DisplayName: "Modify Skill",
		Description: "Update one of your own skills, replacing description, tags, and/or content as provided. Built-in skills cannot be modified.",
		InputSchema: json.RawMessage(`{"type":"object","required":["name"],"properties":{"name":{"type":"string"},"description":{"type":"string"},"tags":{"type":"array","items":{"type":"string"}},"content":{"type":"string"}}}`),
	},
}

// RegisterSkillTools registers the core-intercepted skill_list/read/write/modify
// handlers into d, backed by the memory store. File-backed skills — built-in and
// user (both boot-seeded, see memory.SkillSourceImmutable) — are immutable:
// skill_write refuses to overwrite one and skill_modify refuses to change one.
// When embedder is non-nil, write/modify also (re-)embed the skill's
// description into the "skills" vector namespace so it is semantically retrievable.
func RegisterSkillTools(d *Dispatcher, store *memory.Store, embedder embed.Embedder) {
	embedSkill := func(name, description string) {
		if embedder == nil || description == "" {
			return
		}
		vec, err := embedder.Embed(context.Background(), description)
		if err != nil {
			return
		}
		store.VectorStore("skills:"+name, "skills", name, vec) //nolint:errcheck
	}

	d.handlers["skill_list"] = func(_ context.Context, _ json.RawMessage) (string, error) {
		return store.SkillListJSON()
	}

	d.handlers["skill_read"] = func(_ context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("skill_read: %w", err)
		}
		sk, found, err := store.SkillGet(req.Name)
		if err != nil {
			return "", err
		}
		if !found {
			return "", fmt.Errorf("skill %q not found", req.Name)
		}
		return sk.Content, nil
	}

	d.handlers["skill_write"] = func(_ context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Name        string   `json:"name"`
			Description string   `json:"description"`
			Tags        []string `json:"tags"`
			Content     string   `json:"content"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("skill_write: %w", err)
		}
		if req.Name == "" {
			return "", fmt.Errorf("skill_write: name is required")
		}
		if existing, found, err := store.SkillGet(req.Name); err != nil {
			return "", err
		} else if found && memory.SkillSourceImmutable(existing.Source) {
			return "", fmt.Errorf("skill %q is a %s skill and cannot be overwritten; it is owned by its source file", req.Name, existing.Source)
		}
		if err := store.SkillUpsert(memory.Skill{
			Name:        req.Name,
			Description: req.Description,
			Tags:        req.Tags,
			Content:     req.Content,
			Source:      memory.SkillSourceAgent,
		}); err != nil {
			return "", err
		}
		embedSkill(req.Name, req.Description)
		return "ok", nil
	}

	d.handlers["skill_modify"] = func(_ context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Name        string   `json:"name"`
			Description string   `json:"description"`
			Tags        []string `json:"tags"`
			Content     string   `json:"content"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("skill_modify: %w", err)
		}
		existing, found, err := store.SkillGet(req.Name)
		if err != nil {
			return "", err
		}
		if !found {
			return "", fmt.Errorf("skill %q not found", req.Name)
		}
		if memory.SkillSourceImmutable(existing.Source) {
			return "", fmt.Errorf("skill %q is a %s skill and cannot be modified; it is owned by its source file", req.Name, existing.Source)
		}
		// Merge: keep existing values where new ones are not provided.
		if req.Description != "" {
			existing.Description = req.Description
		}
		if len(req.Tags) > 0 {
			existing.Tags = req.Tags
		}
		if req.Content != "" {
			existing.Content = req.Content
		}
		existing.Source = memory.SkillSourceAgent
		if err := store.SkillUpsert(existing); err != nil {
			return "", err
		}
		embedSkill(existing.Name, existing.Description)
		return "ok", nil
	}
}
