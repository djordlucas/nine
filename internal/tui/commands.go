package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"nine/internal/config"
	ninectx "nine/internal/context"
	"nine/internal/protocol"
)

// slashCmd is one entry in the TUI's command catalog.
//
// The catalog is the single source of truth behind /help and the suggestion
// picker. Before it existed the command list was spelled out separately in
// runCmd's switch, in a hand-written cmdHelp string, and in the TUI's key
// handler — and the three had already drifted apart.
type slashCmd struct {
	name string // without the leading slash
	args string // "" when the command takes none; e.g. "[filter]", "<mode>"
	desc string // one short line, shown to the right in the picker
}

// slashCmds lists every command the TUI accepts, most-used first. That order is
// what /help prints and the order the picker offers before typing narrows it,
// so the commands worth reaching for are the ones on screen first.
//
// Commands are executed either by runCmd below or, for the three that need the
// TUI's own state, by the key handler in tui.go (/new, /clear, /think).
var slashCmds = []slashCmd{
	{"help", "", "this list"},
	{"sessions", "", "list running sessions (copy ID to reattach)"},
	{"status", "", "daemon uptime, active agents, loaded plugins"},
	{"config", "", "show running configuration"},
	{"context", "[id]", "show assembled-context token breakdown (no LLM call)"},
	{"plan-mode", "<mode>", "set reasoning mode: off | plan-only | always"},
	{"goals", "", "list goals"},
	{"workflows", "", "list active and recent workflows"},
	{"tools", "[filter]", "list all tools (optional name filter)"},
	{"standing", "[id]", "list standing tools, or show one with its recent activity"},
	{"grants", "[approve|deny|revoke <id>]", "capability requests and the ceiling in force, or decide one"},
	{"skills", "[name]", "list skills, or show a specific skill"},
	{"memory", "[key]", "list KV keys, or show a specific key's value"},
	{"new", "", "start a fresh conversation"},
	{"think", "<message>", "send a message with reasoning forced on for this turn"},
	{"clear", "", "clear the screen"},
}

// label renders the command as it appears in /help: "/tools [filter]".
func (c slashCmd) label() string {
	if c.args == "" {
		return "/" + c.name
	}
	return "/" + c.name + " " + c.args
}

// completion is the text the picker puts in the input box when the command is
// accepted. Commands that take an argument get a trailing space so the user can
// type it straight away.
func (c slashCmd) completion() string {
	if c.args == "" {
		return "/" + c.name
	}
	return "/" + c.name + " "
}

// matchCmds returns the catalog entries whose name starts with prefix (given
// without the leading slash), case-insensitively. An empty prefix matches every
// command, which is what the picker shows the moment "/" is typed.
func matchCmds(prefix string) []slashCmd {
	prefix = strings.ToLower(prefix)
	var out []slashCmd
	for _, c := range slashCmds {
		if strings.HasPrefix(c.name, prefix) {
			out = append(out, c)
		}
	}
	return out
}

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
	case "standing":
		return cmdStanding(client, arg)
	case "grants":
		return cmdGrants(client, arg)
	default:
		return "", fmt.Errorf("%w /%s — type /help for a list", errUnknownCmd, cmd)
	}
}

// errUnknownCmd heads the error runCmd returns for a command it does not
// dispatch. TestCatalogCommandsAreDispatched matches on it to prove no catalog
// entry can be suggested without an implementation behind it.
var errUnknownCmd = errors.New("unknown command")

// cmdHelp renders the catalog, so /help and the picker can never disagree.
func cmdHelp() string {
	w := 0
	for _, c := range slashCmds {
		if n := len(c.label()); n > w {
			w = n
		}
	}
	var sb strings.Builder
	sb.WriteString("Slash commands:")
	for _, c := range slashCmds {
		fmt.Fprintf(&sb, "\n  %-*s  %s", w, c.label(), c.desc)
	}
	return sb.String()
}

// cmdStanding lists standing tools, or shows one with its recent activity.
//
// Read-only, like every other slash command here. Stopping and starting one is
// deliberately CLI-only (`nine tool stop`): a keystroke away from halting
// something that runs unattended is the wrong ergonomics.
// cmdGrants lists the generated tier's capability ceiling and the requests to
// widen it, and — unlike the other listing views — can decide one.
//
// It mutates because the alternative is worse: the agent surfaces a request to
// `nine notifications`, which the TUI shows, and then the operator has to leave the
// session to act on it. `/plan-mode` is the existing precedent for a slash command
// that changes state.
func cmdGrants(client *protocol.Client, arg string) (string, error) {
	if arg != "" {
		fields := strings.Fields(arg)
		action := fields[0]
		switch action {
		case "approve", "deny", "revoke":
			if len(fields) < 2 {
				return "", fmt.Errorf("/grants %s needs an id — run /grants to see them", action)
			}
			return client.DecideCapability(fields[1], action)
		default:
			return "", fmt.Errorf("/grants %q is not approve, deny or revoke", action)
		}
	}

	state, err := client.ListCapabilities()
	if err != nil {
		return "", err
	}

	var b strings.Builder
	pending := 0
	for _, r := range state.Requests {
		if r.Status == "pending" {
			pending++
		}
	}
	if pending == 0 {
		b.WriteString("No capability requests waiting.\n")
	} else {
		fmt.Fprintf(&b, "%d capability request(s) waiting:\n", pending)
		for _, r := range state.Requests {
			if r.Status != "pending" {
				continue
			}
			fmt.Fprintf(&b, "  %s  %s", r.ID, r.Capability)
			if r.ToolName != "" {
				fmt.Fprintf(&b, " for %s", r.ToolName)
			}
			b.WriteString("\n")
			if r.Reason != "" {
				fmt.Fprintf(&b, "      %s\n", r.Reason)
			}
		}
		b.WriteString("\n  /grants approve <id>   /grants deny <id>\n")
	}

	fmt.Fprintf(&b, "\nCeiling in force (%d):\n", len(state.Grants))
	if len(state.Grants) == 0 {
		b.WriteString("  none — any capability a generated tool declares is refused\n")
	}
	for _, g := range state.Grants {
		fmt.Fprintf(&b, "  %-9s %-9s %s\n", g.Source, g.Capability, grantScope(g.Params))
	}
	return b.String(), nil
}

// grantScope renders a grant's parameters in one line, reporting what the JSON says
// rather than interpreting it so an unfamiliar shape still shows something.
func grantScope(params string) string {
	if params == "" {
		return ""
	}
	var p struct {
		Mounts []struct {
			Host  string `json:"host"`
			Guest string `json:"guest"`
		} `json:"mounts"`
		Env        []string `json:"env"`
		AllowHosts []string `json:"allow_hosts"`
		Scope      string   `json:"scope"`
	}
	if err := json.Unmarshal([]byte(params), &p); err != nil {
		return params
	}
	var parts []string
	for _, m := range p.Mounts {
		parts = append(parts, m.Host+" => "+m.Guest)
	}
	parts = append(parts, p.Env...)
	parts = append(parts, p.AllowHosts...)
	if p.Scope != "" {
		parts = append(parts, "scope "+p.Scope)
	}
	if len(parts) == 0 {
		return params
	}
	return strings.Join(parts, "; ")
}

func cmdStanding(client *protocol.Client, arg string) (string, error) {
	if arg != "" {
		r, err := client.ShowStanding(arg, 10)
		if err != nil {
			return "", err
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s (%s) — %s, %s\n", r.ID, r.Tool, r.State, r.Trigger)
		fmt.Fprintf(&b, "  %d cycles, %d calls\n", r.Cycles, r.Calls)
		if r.LastError != "" {
			fmt.Fprintf(&b, "  last error: %s\n", r.LastError)
		}
		for _, e := range r.Recent {
			fmt.Fprintf(&b, "  %s  %-9s %s\n", e.At, e.Outcome, e.Detail)
		}
		return b.String(), nil
	}

	runs, err := client.ListStanding()
	if err != nil {
		return "", err
	}
	if len(runs) == 0 {
		return "No standing tools.", nil
	}
	var b strings.Builder
	for _, r := range runs {
		fmt.Fprintf(&b, "%-20s %-12s %-9s %s\n", r.ID, r.Tool, r.State, r.Trigger)
	}
	return b.String(), nil
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
			short = shortID(short, 8)
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
			short = shortID(short, 8)
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
				short = shortID(short, 8)
			}
			desc := sa.Description
			if len(desc) > 48 {
				desc = clip(desc, 49)
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

// clip and shortID mirror the helpers in internal/cli, for the same reason: a
// fixed byte offset cuts free text mid-rune and emits invalid UTF-8, and a fixed
// slice on an id panics when the id is shorter than the offset. Descriptions,
// names, and step labels are model- or user-authored, so non-ASCII is ordinary
// input.
//
// Duplicated rather than shared: two ten-line helpers in two leaf packages beat
// a package that exists to hold them. If a third caller appears, extract them.
func clip(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return string(r[:max])
	}
	return string(r[:max-1]) + "…"
}

// shortID cuts an identifier to at most max runes, with no ellipsis, and cannot
// panic on a short id.
func shortID(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

func cmdConfig(cfg *config.Config) string {
	if cfg == nil {
		return "(no config loaded)"
	}

	// Shows the first four characters so an operator can tell which key is
	// configured, and masks the rest. Counts runes: a byte cut could split one,
	// and a byte-length mask would leak the encoded size rather than the key's.
	maskKey := func(k string) string {
		r := []rune(k)
		if len(r) > 4 {
			return string(r[:4]) + strings.Repeat("*", len(r)-4)
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
	kv("context_budget", fmt.Sprintf("%d", cfg.LLM.ContextBudget))
	kv("max_concurrent", fmt.Sprintf("%d", cfg.LLM.MaxConcurrent))
	kv("num_ctx", fmt.Sprintf("%d", cfg.LLM.NumCtx))
	kv("timeout_seconds", fmt.Sprintf("%d", cfg.LLM.TimeoutSeconds))

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
			desc = clip(desc, 73)
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
				desc = clip(desc, 61)
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
			id = shortID(id, 12)
		}
		name := w.Name
		if len(name) > 40 {
			name = clip(name, 38)
		}
		fmt.Fprintf(&sb, "%-12s [%s] %s\n", id, w.Status, name)
		for _, s := range w.Steps {
			label := s.Label
			if len(label) > 50 {
				label = clip(label, 48)
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
