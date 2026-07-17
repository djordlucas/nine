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
func (c *CLI) Trace(cfg *config.Config, agentID string, turn int) error {
	events, err := c.readEvents(cfg, agentID)
	if err != nil {
		return err
	}
	formatTrace(c.Out, agentID, events, turn)
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
	Name       string `json:"name"`
	Output     string `json:"output"`
	Truncated  bool   `json:"truncated"`
	DurationMs int64  `json:"duration_ms"`
	Attempts   int    `json:"attempts"`
	Error      string `json:"error"`
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
			out := p.Output
			if p.Truncated {
				out += " [truncated]"
			}
			fmt.Fprintf(w, "  output: %s\n", trunc(out, 200))
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
