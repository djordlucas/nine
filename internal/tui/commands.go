package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"nine/internal/config"
	ninectx "nine/internal/context"
	"nine/internal/protocol"
)

// runCmd executes a slash command and returns the text to display, or an error.
// cmd is the command name (without the leading /), arg is everything after the first space.
// curAgentID is the session the TUI is currently attached to; it is the default
// target for commands like /context when no id argument is given.
func runCmd(cmd, arg string, client *protocol.Client, cfg *config.Config, curAgentID string) (string, error) {
	switch cmd {
	case "help":
		return cmdHelp(), nil
	case "status":
		return cmdStatus(client)
	case "config":
		return cmdConfig(cfg), nil
	case "context":
		return cmdContext(client, arg, curAgentID)
	case "plan-mode":
		return cmdPlanMode(client, arg, curAgentID)
	case "goals":
		return cmdGoals(client)
	case "tools":
		return cmdTools(client, arg)
	case "skills":
		return cmdSkills(client, arg)
	case "memory":
		return cmdMemory(client, arg)
	case "workflows":
		return cmdWorkflows(client)
	case "sessions":
		return cmdSessions(client)
	default:
		return "", fmt.Errorf("unknown command /%s — type /help for a list", cmd)
	}
}

func cmdHelp() string {
	return strings.Join([]string{
		"Slash commands:",
		"  /help              this list",
		"  /sessions          list running sessions (copy ID to reattach)",
		"  /status            daemon uptime, active agents, loaded plugins",
		"  /config            show running configuration",
		"  /context [id]      show assembled-context token breakdown (no LLM call)",
		"  /plan-mode <mode>  set reasoning mode: off | plan-only | always",
		"  /goals             list goals",
		"  /workflows         list active and recent workflows",
		"  /tools [filter]    list all tools (optional name filter)",
		"  /skills [name]     list skills, or show a specific skill",
		"  /memory [key]      list KV keys, or show a specific key's value",
		"  /new               start a fresh conversation",
		"  /think <message>   send a message with reasoning forced on for this turn",
		"  /clear             clear the screen",
	}, "\n")
}

func cmdSessions(client *protocol.Client) (string, error) {
	info, err := client.Status()
	if err != nil {
		return "", err
	}
	if len(info.Agents) == 0 {
		return "(no active sessions)", nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Active sessions — reattach with: nine attach <id>\n")
	for _, a := range info.Agents {
		short := a.ID
		if len(short) > 8 {
			short = short[:8]
		}
		if a.Name != "" {
			fmt.Fprintf(&sb, "  %-10s  %s\n", short, a.Name)
		} else {
			fmt.Fprintf(&sb, "  %s\n", short)
		}
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

func cmdStatus(client *protocol.Client) (string, error) {
	info, err := client.Status()
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Uptime:   %s\n", info.Uptime)
	fmt.Fprintf(&sb, "Agents:   %d active\n", len(info.Agents))
	for _, a := range info.Agents {
		short := a.ID
		if len(short) > 8 {
			short = short[:8]
		}
		mode := ""
		if a.PlanMode != "" {
			mode = "  plan:" + a.PlanMode
		}
		if a.Name != "" {
			fmt.Fprintf(&sb, "           %-32s  %s%s\n", a.Name, short, mode)
		} else {
			fmt.Fprintf(&sb, "           %s%s\n", short, mode)
		}
	}
	if len(info.SubAgents) > 0 {
		fmt.Fprintf(&sb, "Sub-agents: %d running\n", len(info.SubAgents))
		for _, sa := range info.SubAgents {
			short := sa.ID
			if len(short) > 8 {
				short = short[:8]
			}
			desc := sa.Description
			if len(desc) > 48 {
				desc = desc[:48] + "…"
			}
			fmt.Fprintf(&sb, "           %s  %s\n", short, desc)
		}
	}
	fmt.Fprintf(&sb, "Plugins:  %s", strings.Join(info.Plugins, ", "))
	return sb.String(), nil
}

// cmdContext shows the assembled-context token breakdown for a session. With no
// id argument it targets the current session. It performs no LLM call.
func cmdContext(client *protocol.Client, arg, curAgentID string) (string, error) {
	id := strings.TrimSpace(arg)
	if id == "" {
		id = curAgentID
	}
	if id == "" {
		return "", fmt.Errorf("no session — usage: /context <id>")
	}
	raw, err := client.Context(id)
	if err != nil {
		return "", err
	}
	var rep ninectx.Report
	if err := json.Unmarshal([]byte(raw), &rep); err != nil {
		return "", fmt.Errorf("decode context: %w", err)
	}
	var sb strings.Builder
	pct := 0
	if rep.Budget > 0 {
		pct = rep.Used * 100 / rep.Budget
	}
	fmt.Fprintf(&sb, "Context: %d / %d tokens used (%d%%)\n", rep.Used, rep.Budget, pct)
	fmt.Fprintf(&sb, "  %-13s %-4s %8s  %-3s %s\n", "section", "pri", "tokens", "in", "detail")
	for _, s := range rep.Sections {
		in := "no"
		if s.Included {
			in = "yes"
		}
		fmt.Fprintf(&sb, "  %-13s %-4s %8d  %-3s %s\n", s.Name, s.Priority, s.Tokens, in, s.Detail)
	}
	fmt.Fprintf(&sb, "\n%d messages assembled — run `nine context %s --verbose` for the full prompt", len(rep.Messages), id)
	return sb.String(), nil
}

func cmdConfig(cfg *config.Config) string {
	if cfg == nil {
		return "(no config loaded)"
	}

	maskKey := func(k string) string {
		if len(k) > 4 {
			return k[:4] + strings.Repeat("*", len(k)-4)
		}
		if k != "" {
			return "****"
		}
		return ""
	}

	var sb strings.Builder
	kv := func(k, v string) { fmt.Fprintf(&sb, "  %-22s = %s\n", k, v) }
	sec := func(name string) { fmt.Fprintf(&sb, "[%s]\n", name) }

	sec("llm")
	kv("provider", cfg.LLM.Provider)
	kv("model", cfg.LLM.Model)
	kv("endpoint", cfg.LLM.Endpoint)
	kv("api_key", maskKey(cfg.LLM.APIKey))
	kv("context_budget", fmt.Sprintf("%d", cfg.LLM.ContextBudget))
	kv("max_concurrent", fmt.Sprintf("%d", cfg.LLM.MaxConcurrent))
	kv("num_ctx", fmt.Sprintf("%d", cfg.LLM.NumCtx))

	sec("daemon")
	kv("socket_path", cfg.Daemon.SocketPath)
	kv("task_timeout_seconds", fmt.Sprintf("%d", cfg.Daemon.TaskTimeoutSeconds))

	sec("plugins")
	kv("bin", cfg.Plugins.Bin)

	sec("memory")
	kv("path", cfg.Memory.Path)

	sec("embeddings")
	kv("provider", cfg.Embeddings.Provider)
	kv("model", cfg.Embeddings.Model)
	kv("endpoint", cfg.Embeddings.Endpoint)
	kv("api_key", maskKey(cfg.Embeddings.APIKey))

	sec("ui")
	kv("theme", cfg.UI.Theme)
	showCtx := ""
	if cfg.UI.ShowContext != nil {
		showCtx = fmt.Sprintf("%v", *cfg.UI.ShowContext)
	}
	kv("show_context", showCtx)

	sec("workspace")
	kv("root", cfg.Workspace.Root)

	return strings.TrimRight(sb.String(), "\n")
}

func cmdGoals(client *protocol.Client) (string, error) {
	raw, err := client.ListGoals()
	if err != nil {
		return "", err
	}
	return formatJSONList(raw, "goal", "description", "status"), nil
}

func cmdTools(client *protocol.Client, filter string) (string, error) {
	tools, err := client.ListTools()
	if err != nil {
		return "", err
	}
	if len(tools) == 0 {
		return "(no tools loaded)", nil
	}
	var sb strings.Builder
	prev := ""
	for _, t := range tools {
		if strings.Contains(filter, " ") {
			filter = ""
		}
		if filter != "" && !strings.Contains(t.Name, filter) && !strings.Contains(t.Plugin, filter) {
			continue
		}
		if t.Plugin != prev {
			if prev != "" {
				sb.WriteByte('\n')
			}
			fmt.Fprintf(&sb, "[%s]\n", t.Plugin)
			prev = t.Plugin
		}
		desc := t.Description
		if len(desc) > 72 {
			desc = desc[:72] + "…"
		}
		fmt.Fprintf(&sb, "  %-28s %s\n", t.Name, desc)
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

func cmdSkills(client *protocol.Client, name string) (string, error) {
	if name == "" {
		out, err := client.PluginCall("skill_list", json.RawMessage(`{}`))
		if err != nil {
			return "", err
		}
		return formatSkillList(out), nil
	}
	args, _ := json.Marshal(map[string]string{"name": name})
	out, err := client.PluginCall("skill_read", json.RawMessage(args))
	if err != nil {
		return "", err
	}
	return out, nil
}

func cmdMemory(client *protocol.Client, key string) (string, error) {
	if key == "" {
		out, err := client.PluginCall("memory_list", json.RawMessage(`{}`))
		if err != nil {
			return "", err
		}
		return formatKVList(out), nil
	}
	args, _ := json.Marshal(map[string]string{"key": key})
	out, err := client.PluginCall("memory_get", json.RawMessage(args))
	if err != nil {
		return "", err
	}
	return out, nil
}

// formatJSONList renders a JSON array of objects, showing nameField and statusField.
func formatJSONList(raw, label, nameField, statusField string) string {
	var items []map[string]any
	if err := json.Unmarshal([]byte(raw), &items); err != nil || len(items) == 0 {
		return fmt.Sprintf("(no %ss)", label)
	}
	var sb strings.Builder
	for _, item := range items {
		name, _ := item[nameField].(string)
		status, _ := item[statusField].(string)
		if status != "" {
			fmt.Fprintf(&sb, "  [%s] %s\n", status, name)
		} else {
			fmt.Fprintf(&sb, "  %s\n", name)
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// formatSkillList formats the JSON output of skill_list into readable text.
func formatSkillList(raw string) string {
	var skills []map[string]any
	if err := json.Unmarshal([]byte(raw), &skills); err != nil || len(skills) == 0 {
		return "(no skills)"
	}
	var sb strings.Builder
	for _, s := range skills {
		name, _ := s["name"].(string)
		desc, _ := s["description"].(string)
		if desc == "" {
			fmt.Fprintf(&sb, "  %s\n", name)
		} else {
			if len(desc) > 60 {
				desc = desc[:60] + "…"
			}
			fmt.Fprintf(&sb, "  %-24s %s\n", name, desc)
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

func cmdWorkflows(client *protocol.Client) (string, error) {
	raw, err := client.ListWorkflows()
	if err != nil {
		return "", err
	}
	var result struct {
		Workflows []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Status string `json:"status"`
			Steps  []struct {
				ID     string `json:"id"`
				Label  string `json:"label"`
				Status string `json:"status"`
			} `json:"steps"`
		} `json:"workflows"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil || len(result.Workflows) == 0 {
		return "(no workflows)", nil
	}
	var sb strings.Builder
	for i, w := range result.Workflows {
		if i > 0 {
			sb.WriteByte('\n')
		}
		id := w.ID
		if len(id) > 12 {
			id = id[:12]
		}
		name := w.Name
		if len(name) > 40 {
			name = name[:37] + "…"
		}
		fmt.Fprintf(&sb, "%-12s [%s] %s\n", id, w.Status, name)
		for _, s := range w.Steps {
			label := s.Label
			if len(label) > 50 {
				label = label[:47] + "…"
			}
			fmt.Fprintf(&sb, "  [%-7s] %s: %s\n", s.Status, s.ID, label)
		}
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

// formatKVList formats the newline-separated output of memory_list.
func formatKVList(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "(memory is empty)"
	}
	return raw
}

func cmdPlanMode(client *protocol.Client, arg, curAgentID string) (string, error) {
	mode := strings.TrimSpace(arg)
	if mode == "" {
		return "", fmt.Errorf("usage: /plan-mode <off | plan-only | always>")
	}
	if curAgentID == "" {
		return "", fmt.Errorf("no active session")
	}
	return client.SetPlanMode(curAgentID, mode)
}
