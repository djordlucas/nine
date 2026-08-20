package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	ninectx "nine/internal/context"
	"nine/internal/protocol"

	"nine/internal/memory"
)

// clip shortens s to at most max characters, appending an ellipsis when it had
// to cut. It counts *runes*, not bytes.
//
// Byte slicing was the previous behavior and it corrupts output: a goal
// description or workflow name is free text a model or user wrote, so a cut at a
// fixed byte offset lands mid-rune for anything outside ASCII and emits invalid
// UTF-8, which a terminal renders as a replacement character. "日本語のゴールの
// 説明でありこれは非常に長いテキストです" cut at byte 37 produced
// "日本語のゴールの説明であ\xe3...".
//
// Note this still pads by rune count, not display width, so a table of
// full-width CJK text is aligned loosely. Fixing that needs a width table; this
// fixes the correctness half.
func clip(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 3 {
		return string(r[:max])
	}
	return string(r[:max-3]) + "..."
}

// shortID cuts an identifier to at most max runes with no ellipsis, matching how
// ID columns have always rendered.
//
// It also cannot panic. `sa.ID[:8]` did: the id arrives from the daemon over the
// wire, and a short or empty one — a malformed reply, a truncated record — took
// the whole CLI down rather than printing a short id. Slicing a string by a
// fixed offset is only safe when something guarantees the length, and nothing
// here did.
func shortID(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

func printGoals(w io.Writer, raw string) {
	var result struct {
		Goals []struct {
			ID          string `json:"id"`
			Description string `json:"description"`
			Status      string `json:"status"`
		} `json:"goals"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil || len(result.Goals) == 0 {
		fmt.Fprintln(w, "no goals")
		return
	}
	fmt.Fprintf(w, "%-12s  %-10s  %s\n", "ID", "STATUS", "DESCRIPTION")
	fmt.Fprintln(w, strings.Repeat("-", 60))
	for _, g := range result.Goals {
		fmt.Fprintf(w, "%-12s  %-10s  %s\n", shortID(g.ID, 12), g.Status, clip(g.Description, 40))
	}
}

func printNotifications(w io.Writer, raw string) {
	var result struct {
		Notifications []struct {
			AgentID   string `json:"agent_id"`
			Message   string `json:"message"`
			Seen      bool   `json:"seen"`
			CreatedAt string `json:"created_at"`
		} `json:"notifications"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil || len(result.Notifications) == 0 {
		fmt.Fprintln(w, "no notifications")
		return
	}
	for _, n := range result.Notifications {
		from := n.AgentID
		if from == "" {
			from = "nine"
		}
		fmt.Fprintf(w, "[%s] %s\n  %s\n", n.CreatedAt, from, n.Message)
	}
}

// printReflections renders a session's reflection turns from its journal.
//
// A reflection is a turn like any other, so the record is `turn_end` and its
// result is the reflection's text. Turns that produced nothing, or ended in an
// error, are skipped — those were never recorded under the old table either.
func printReflections(w io.Writer, agentID string, events []memory.SessionEvent) {
	type turnEnd struct {
		Result string `json:"result"`
		Error  string `json:"error"`
	}
	shown := 0
	for _, e := range events {
		if e.Type != "turn_end" {
			continue
		}
		var p turnEnd
		if err := json.Unmarshal(e.Payload, &p); err != nil || p.Error != "" || p.Result == "" {
			continue
		}
		if shown > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "[%s]\n", e.TS.Format(time.RFC3339))
		fmt.Fprintln(w, p.Result)
		shown++
	}
	if shown == 0 {
		fmt.Fprintf(w, "no reflections for %s\n", agentID)
	}
}

func printWorkflows(w io.Writer, raw string) {
	var result struct {
		Workflows []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			Status    string `json:"status"`
			CreatedAt string `json:"created_at"`
			Steps     []struct {
				ID     string `json:"id"`
				Label  string `json:"label"`
				Status string `json:"status"`
			} `json:"steps"`
		} `json:"workflows"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil || len(result.Workflows) == 0 {
		fmt.Fprintln(w, "no workflows")
		return
	}
	for i, wf := range result.Workflows {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%-12s  [%s]  %s\n", shortID(wf.ID, 12), wf.Status, clip(wf.Name, 40))
		for _, s := range wf.Steps {
			fmt.Fprintf(w, "  [%-7s] %s: %s\n", s.Status, s.ID, clip(s.Label, 52))
		}
	}
}

func printStatus(w io.Writer, info *protocol.StatusInfo) {
	fmt.Fprintf(w, "Uptime:   %s\n", info.Uptime)
	if q := info.LLMQueue; q != nil {
		fmt.Fprintf(w, "LLM queue: %d inflight, %d waiting  (max %d concurrent)\n",
			q.Inflight, q.Pending, q.MaxConcurrent)
	}
	fmt.Fprintf(w, "Agents:   %d active\n", len(info.Agents))
	for _, a := range info.Agents {
		short := shortID(a.ID, 8)
		role := a.Role
		if role == "" {
			role = "-"
		}
		if a.Name != "" {
			fmt.Fprintf(w, "          %-32s  %-8s  %s\n", a.Name, short, role)
		} else {
			fmt.Fprintf(w, "          %-32s  %-8s  %s\n", "", short, role)
		}
	}
	if len(info.SubAgents) > 0 {
		fmt.Fprintf(w, "Sub-agents: %d running\n", len(info.SubAgents))
		for _, sa := range info.SubAgents {
			role := sa.Role
			if role == "" {
				role = "-"
			}
			fmt.Fprintf(w, "          %-8s  %-12s  %s\n",
				shortID(sa.ID, 8), role, clip(sa.Description, 40))
		}
	}
	sort.Strings(info.Plugins)
	if len(info.Plugins) == 0 {
		fmt.Fprintln(w, "Plugins:  none")
	} else {
		fmt.Fprintf(w, "Plugins:  %s\n", strings.Join(info.Plugins, ", "))
	}
}

// printContextReport renders a session's assembled-context breakdown: a
// per-priority token table, budget totals, and (in verbose mode) the full
// assembled system prompt and message list.
func printContextReport(w io.Writer, rep *ninectx.Report, verbose bool) {
	pct := 0
	if rep.Budget > 0 {
		pct = rep.Used * 100 / rep.Budget
	}
	fmt.Fprintf(w, "Context: %d / %d tokens used (%d%%)\n\n", rep.Used, rep.Budget, pct)
	fmt.Fprintf(w, "  %-13s %-4s %8s  %-5s  %s\n", "SECTION", "PRI", "TOKENS", "IN", "DETAIL")
	for _, s := range rep.Sections {
		in := "no"
		if s.Included {
			in = "yes"
		}
		fmt.Fprintf(w, "  %-13s %-4s %8d  %-5s  %s\n", s.Name, s.Priority, s.Tokens, in, s.Detail)
	}
	fmt.Fprintf(w, "\n  %-13s %-4s %8d\n", "TOTAL used", "", rep.Used)

	if !verbose {
		fmt.Fprintf(w, "\n(%d messages assembled; run with --verbose to dump the full prompt)\n", len(rep.Messages))
		return
	}

	fmt.Fprintln(w, "\n─── system prompt ───")
	fmt.Fprintln(w, rep.System)
	fmt.Fprintf(w, "\n─── messages (%d) ───\n", len(rep.Messages))
	for i, m := range rep.Messages {
		fmt.Fprintf(w, "[%d] %s", i, m.Role)
		if len(m.ToolCalls) > 0 {
			names := make([]string, len(m.ToolCalls))
			for j, tc := range m.ToolCalls {
				names[j] = tc.Name
			}
			fmt.Fprintf(w, " (tool_calls: %s)", strings.Join(names, ", "))
		}
		if len(m.ToolResults) > 0 {
			fmt.Fprintf(w, " (tool_results: %d)", len(m.ToolResults))
		}
		fmt.Fprintln(w)
		if m.Text != "" {
			fmt.Fprintf(w, "    %s\n", strings.ReplaceAll(m.Text, "\n", "\n    "))
		}
	}
}
