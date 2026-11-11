package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"nine/internal/llm"
)

// Role discovery tools (adr/orchestrator-board.md §2.2.1): the orchestrator
// picks a worker role per task, so it must be able to list and search the role
// registry the way tool_list/tool_search cover the tool catalog.

// RoleToolNames are the role discovery tools, in the order they are advertised.
var RoleToolNames = []string{"role_list", "role_search"}

// RoleInfo is one role as the catalog reports it: enough for a caller to pick
// a worker role — name, description, and what it may do.
type RoleInfo struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	AllTools    bool     `json:"all_tools"`
	Tools       []string `json:"tools,omitempty"`
	Delegates   bool     `json:"delegates"`
}

// RoleCatalog is the role registry as the tools see it. Implemented in
// internal/runtime over RoleRegistry.
type RoleCatalog interface {
	Roles() []RoleInfo
}

var roleToolDefs = []llm.ToolDef{
	{
		Name:        "role_list",
		DisplayName: "List Roles",
		Description: "List every role that can be delegated to (the run_agent role choices), with each one's description and tool allowlist. Use it to see what worker kinds exist before dispatching.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	},
	{
		Name:        "role_search",
		DisplayName: "Search Roles",
		Description: "Search roles by name, description or tool — e.g. \"shell\" finds the roles that can run shell commands. Returns matching roles with their allowlists, so you can pick the narrowest role that fits a task.",
		InputSchema: json.RawMessage(`{"type":"object","required":["query"],"properties":{"query":{"type":"string","description":"What the role must be able to do, in a few words."}}}`),
	},
}

// RoleToolDefs returns the role discovery tools' definitions.
func RoleToolDefs() []llm.ToolDef { return roleToolDefs }

// RegisterRoleTools registers the role discovery tools named in names into d.
func RegisterRoleTools(d *Dispatcher, cat RoleCatalog, names []string) {
	defer func() {
		for _, name := range RoleToolNames {
			if !slices.Contains(names, name) {
				delete(d.handlers, name)
			}
		}
	}()
	for _, name := range names {
		switch name {
		case "role_list":
			d.handlers["role_list"] = func(_ context.Context, _ json.RawMessage) (string, error) {
				out, _ := json.Marshal(cat.Roles())
				return string(out), nil
			}
		case "role_search":
			d.handlers["role_search"] = func(_ context.Context, args json.RawMessage) (string, error) {
				var req struct {
					Query string `json:"query"`
				}
				if err := json.Unmarshal(args, &req); err != nil {
					return "", fmt.Errorf("role_search: %w", err)
				}
				q := strings.ToLower(strings.TrimSpace(req.Query))
				if q == "" {
					return "", fmt.Errorf("role_search: query is required")
				}
				var hits []RoleInfo
				for _, r := range cat.Roles() {
					hay := strings.ToLower(r.Name + " " + r.Description + " " + strings.Join(r.Tools, " "))
					if strings.Contains(hay, q) {
						hits = append(hits, r)
					}
				}
				out, _ := json.Marshal(hits)
				return string(out), nil
			}
		}
	}
}
