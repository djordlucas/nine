package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"nine/internal/config"
	"nine/internal/memory"
)

// Trace prints the session event journal for agentID as a compact one-line-per-
// event timeline (docs/event-log.md §10). With turn > 0 only that turn is shown.
// It reads session_events directly from the store, so it works after a restart
// and with the daemon down — the post-hoc debugging case.
//
// When subAgents is true, each sub_agent_start event expands into the spawned
// sub-agent's own journal, nested inline and indented beneath the marker, and
// recursively for sub-agents of sub-agents.
func (c *CLI) Trace(cfg *config.Config, agentID string, turn int, subAgents bool) error {
	store, err := memory.Open(cfg.DatabaseURL())
	if err != nil {
		return fmt.Errorf("open memory store: %w", err)
	}
	defer store.Close() //nolint:errcheck
	events, err := store.SessionEventsByAgent(agentID)
	if err != nil {
		return fmt.Errorf("read session events: %w", err)
	}
	if subAgents {
		formatTraceTree(c.Out, agentID, events, turn, store.SessionEventsByAgent)
	} else {
		formatTrace(c.Out, agentID, events, turn)
	}
	return nil
}

// Replay reconstructs a single turn of agentID from the journal: each inner LLM
// request → response → tool call I/O, with token usage and timings
// (docs/event-log.md §10). turn must be > 0.
func (c *CLI) Replay(cfg *config.Config, agentID string, turn int) error {
	if turn <= 0 {
		return fmt.Errorf("usage: nine replay <agent-id> --turn <N>")
	}
	events, err := c.readEvents(cfg, agentID)
	if err != nil {
		return err
	}
	formatReplay(c.Out, agentID, events, turn)
	return nil
}

// readEvents opens the memory store directly and returns agentID's journal.
func (c *CLI) readEvents(cfg *config.Config, agentID string) ([]memory.SessionEvent, error) {
	store, err := memory.Open(cfg.DatabaseURL())
	if err != nil {
		return nil, fmt.Errorf("open memory store: %w", err)
	}
	defer store.Close() //nolint:errcheck
	events, err := store.SessionEventsByAgent(agentID)
	if err != nil {
		return nil, fmt.Errorf("read session events: %w", err)
	}
	return events, nil
}

// --- payload parse structs (mirror the JSON written in internal/runtime/journal.go) ---
//
// llm.Message / llm.ToolCall have no json tags, so their fields serialize under
// their Go names (Role, Text, Name, Input) — matched here.

type turnStartPayload struct {
	Input   string `json:"input"`
	Trigger string `json:"trigger"`
}

type turnEndPayload struct {
	Result     string `json:"result"`
	Error      string `json:"error"`
	ToolCount  int    `json:"tool_count"`
	DurationMs int64  `json:"duration_ms"`
}

type traceMessage struct {
	Role string `json:"Role"`
	Text string `json:"Text"`
}

type traceToolCall struct {
	Name  string          `json:"Name"`
	Input json.RawMessage `json:"Input"`
}

type llmRequestPayload struct {
	System     string         `json:"system"`
	Messages   []traceMessage `json:"messages"`
	ToolNames  []string       `json:"tool_names"`
	TokensUsed int            `json:"tokens_used"`
	Budget     int            `json:"budget"`
	LLMCallN   int            `json:"llm_call_n"`
}

type llmResponsePayload struct {
	Text       string          `json:"text"`
	ToolCalls  []traceToolCall `json:"tool_calls"`
	StopReason string          `json:"stop_reason"`
	LLMCallN   int             `json:"llm_call_n"`
}

type toolEndPayload struct {
	Name        string `json:"name"`
	Output      string `json:"output"`
	Truncated   bool   `json:"truncated"`
	SpillPath   string `json:"spill_path"`
	OutputChars int    `json:"output_chars"`
	DurationMs  int64  `json:"duration_ms"`
	Attempts    int    `json:"attempts"`
	Error       string `json:"error"`
}

type toolStartPayload struct {
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type contextPayload struct {
	Used   int `json:"used"`
	Budget int `json:"budget"`
}

type subAgentPayload struct {
	SubID  string `json:"sub_id"`
	Task   string `json:"task"`
	Status string `json:"status"`
}

// unpack unmarshals e.Payload into v, ignoring errors (a malformed payload just
// yields a sparse summary rather than aborting the whole trace).
func unpack(e memory.SessionEvent, v any) {
	_ = json.Unmarshal(e.Payload, v)
}

func trunc(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func formatTrace(w io.Writer, agentID string, events []memory.SessionEvent, turnFilter int) {
	if turnFilter > 0 {
		var kept []memory.SessionEvent
		for _, e := range events {
			if e.Turn == turnFilter {
				kept = append(kept, e)
			}
		}
		events = kept
	}
	if len(events) == 0 {
		if turnFilter > 0 {
			fmt.Fprintf(w, "no events for turn %d of %s\n", turnFilter, agentID)
		} else {
			fmt.Fprintf(w, "no events recorded for %s\n", agentID)
		}
		return
	}

	turns := map[int]bool{}
	for _, e := range events {
		turns[e.Turn] = true
	}
	fmt.Fprintf(w, "session %s — %d events across %d turn(s)\n\n", agentID, len(events), len(turns))

	lastTurn := -1
	for _, e := range events {
		if e.Turn != lastTurn {
			if lastTurn != -1 {
				fmt.Fprintln(w)
			}
			lastTurn = e.Turn
		}
		fmt.Fprintf(w, "#%-5d t%-2d  %-16s %s\n", e.Seq, e.Turn, e.Type, traceSummary(e))
	}
}

// subAgentFetch reads a sub-agent's journal by ID, matching
// memory.Store.SessionEventsByAgent.
type subAgentFetch func(agentID string) ([]memory.SessionEvent, error)

// formatTraceTree renders agentID's trace like formatTrace, but expands each
// sub_agent_start event into the spawned sub-agent's own journal, nested and
// indented beneath the marker, recursively. The turn filter applies only to the
// top-level (parent) trace; an expanded sub-agent is always shown in full.
func formatTraceTree(w io.Writer, agentID string, events []memory.SessionEvent, turnFilter int, fetch subAgentFetch) {
	if turnFilter > 0 {
		events = filterTurn(events, turnFilter)
	}
	if len(events) == 0 {
		if turnFilter > 0 {
			fmt.Fprintf(w, "no events for turn %d of %s\n", turnFilter, agentID)
		} else {
			fmt.Fprintf(w, "no events recorded for %s\n", agentID)
		}
		return
	}
	renderTraceNode(w, agentID, events, "", true, fetch, map[string]bool{agentID: true})
}

// renderTraceNode prints one agent's events with the given indent, recursing
// into sub-agents on each sub_agent_start. seen guards against re-expanding an
// ID (cycles or a sub-agent referenced twice).
func renderTraceNode(w io.Writer, agentID string, events []memory.SessionEvent, indent string, root bool, fetch subAgentFetch, seen map[string]bool) {
	turns := map[int]bool{}
	for _, e := range events {
		turns[e.Turn] = true
	}
	if root {
		fmt.Fprintf(w, "session %s — %d events across %d turn(s)\n\n", agentID, len(events), len(turns))
	} else {
		fmt.Fprintf(w, "%s└─ sub-agent %s — %d events across %d turn(s)\n", indent, agentID, len(events), len(turns))
	}

	lastTurn := -1
	for _, e := range events {
		if e.Turn != lastTurn {
			if lastTurn != -1 {
				fmt.Fprintf(w, "%s\n", indent)
			}
			lastTurn = e.Turn
		}
		fmt.Fprintf(w, "%s#%-5d t%-2d  %-16s %s\n", indent, e.Seq, e.Turn, e.Type, traceSummary(e))
		if e.Type != "sub_agent_start" {
			continue
		}
		var p subAgentPayload
		unpack(e, &p)
		if p.SubID == "" || seen[p.SubID] {
			continue
		}
		seen[p.SubID] = true
		child, err := fetch(p.SubID)
		if err != nil {
			fmt.Fprintf(w, "%s    └─ sub-agent %s — trace unavailable: %v\n", indent, p.SubID, err)
			continue
		}
		if len(child) == 0 {
			fmt.Fprintf(w, "%s    └─ sub-agent %s — no events recorded\n", indent, p.SubID)
			continue
		}
		renderTraceNode(w, p.SubID, child, indent+"    ", false, fetch, seen)
	}
}

// filterTurn keeps only events belonging to the given turn.
func filterTurn(events []memory.SessionEvent, turn int) []memory.SessionEvent {
	var kept []memory.SessionEvent
	for _, e := range events {
		if e.Turn == turn {
			kept = append(kept, e)
		}
	}
	return kept
}

// traceSummary renders a one-line, type-specific summary of an event's payload.
func traceSummary(e memory.SessionEvent) string {
	switch e.Type {
	case "turn_start":
		var p turnStartPayload
		unpack(e, &p)
		return fmt.Sprintf("%s: %q", p.Trigger, trunc(p.Input, 60))
	case "turn_end":
		var p turnEndPayload
		unpack(e, &p)
		return fmt.Sprintf("%s · %d tools · %dms", okOrErr(p.Error), p.ToolCount, p.DurationMs)
	case "llm_request":
		var p llmRequestPayload
		unpack(e, &p)
		return fmt.Sprintf("call %d · %d/%d tok · tools: %s", p.LLMCallN, p.TokensUsed, p.Budget, trunc(strings.Join(p.ToolNames, ","), 40))
	case "llm_response":
		var p llmResponsePayload
		unpack(e, &p)
		s := fmt.Sprintf("call %d · %s · %d tool call(s)", p.LLMCallN, p.StopReason, len(p.ToolCalls))
		if p.Text != "" {
			s += " · " + fmt.Sprintf("%q", trunc(p.Text, 40))
		}
		return s
	case "tool_start":
		var p toolStartPayload
		unpack(e, &p)
		return fmt.Sprintf("%s %s", p.Name, trunc(string(p.Input), 50))
	case "tool_end":
		var p toolEndPayload
		unpack(e, &p)
		return fmt.Sprintf("%s · %dms · %s%s", p.Name, p.DurationMs, okOrErr(p.Error), attemptsSuffix(p.Attempts))
	case "context_update":
		var p contextPayload
		unpack(e, &p)
		return fmt.Sprintf("%d/%d tok", p.Used, p.Budget)
	case "thinking":
		var p struct {
			LLMCallN int `json:"llm_call_n"`
		}
		unpack(e, &p)
		return fmt.Sprintf("call %d", p.LLMCallN)
	case "sub_agent_start":
		var p subAgentPayload
		unpack(e, &p)
		return fmt.Sprintf("%s %q", trunc(p.SubID, 12), trunc(p.Task, 50))
	case "sub_agent_end":
		var p subAgentPayload
		unpack(e, &p)
		return fmt.Sprintf("%s %s", trunc(p.SubID, 12), p.Status)
	default:
		return ""
	}
}

func formatReplay(w io.Writer, agentID string, all []memory.SessionEvent, turn int) {
	var events []memory.SessionEvent
	for _, e := range all {
		if e.Turn == turn {
			events = append(events, e)
		}
	}
	if len(events) == 0 {
		fmt.Fprintf(w, "no events for turn %d of %s\n", turn, agentID)
		return
	}

	fmt.Fprintf(w, "session %s · turn %d\n", agentID, turn)
	for _, e := range events {
		switch e.Type {
		case "turn_start":
			var p turnStartPayload
			unpack(e, &p)
			fmt.Fprintf(w, "trigger: %s\ninput:   %s\n", p.Trigger, trunc(p.Input, 200))
		case "llm_request":
			var p llmRequestPayload
			unpack(e, &p)
			fmt.Fprintf(w, "\n── LLM call %d ─────────────────────\n", p.LLMCallN)
			fmt.Fprintf(w, "request:  %d/%d tokens · %d msg window · tools: %s\n",
				p.TokensUsed, p.Budget, len(p.Messages), trunc(strings.Join(p.ToolNames, ", "), 60))
			if p.System != "" {
				fmt.Fprintf(w, "system:   %s\n", trunc(p.System, 100))
			}
		case "llm_response":
			var p llmResponsePayload
			unpack(e, &p)
			fmt.Fprintf(w, "response: stop=%s\n", p.StopReason)
			if p.Text != "" {
				fmt.Fprintf(w, "  text: %s\n", trunc(p.Text, 200))
			}
			for _, tc := range p.ToolCalls {
				fmt.Fprintf(w, "  → call %s %s\n", tc.Name, trunc(string(tc.Input), 80))
			}
		case "tool_end":
			var p toolEndPayload
			unpack(e, &p)
			status := okOrErr(p.Error)
			fmt.Fprintf(w, "\ntool %s  (%dms, %d attempt(s), %s)\n", p.Name, p.DurationMs, p.Attempts, status)
			if p.Error != "" {
				fmt.Fprintf(w, "  error:  %s\n", trunc(p.Error, 200))
			}
			fmt.Fprintf(w, "  output: %s\n", trunc(p.Output, 200))
			// An over-cap result: say how big it really was and where the full
			// text is, so the trace can be followed to the actual bytes.
			switch {
			case p.SpillPath != "":
				fmt.Fprintf(w, "  spilled: %d chars → %s\n", p.OutputChars, p.SpillPath)
			case p.Truncated:
				fmt.Fprintf(w, "  truncated: %d chars, full output not retained\n", p.OutputChars)
			}
		case "sub_agent_start":
			var p subAgentPayload
			unpack(e, &p)
			fmt.Fprintf(w, "\nsub-agent %s spawned: %s\n", trunc(p.SubID, 16), trunc(p.Task, 80))
		case "sub_agent_end":
			var p subAgentPayload
			unpack(e, &p)
			fmt.Fprintf(w, "sub-agent %s %s\n", trunc(p.SubID, 16), p.Status)
		case "turn_end":
			var p turnEndPayload
			unpack(e, &p)
			fmt.Fprintf(w, "\nresult (%s · %d tools · %dms): %s\n",
				okOrErr(p.Error), p.ToolCount, p.DurationMs, trunc(p.Result, 200))
			if p.Error != "" {
				fmt.Fprintf(w, "error: %s\n", trunc(p.Error, 200))
			}
		}
	}
}

func okOrErr(errStr string) string {
	if errStr == "" {
		return "ok"
	}
	return "error"
}

func attemptsSuffix(attempts int) string {
	if attempts > 1 {
		return " · " + strconv.Itoa(attempts) + "x"
	}
	return ""
}
