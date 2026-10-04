package api

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"nine/internal/api/apigen"
	"nine/internal/protocol"
)

// Conversation history, trace and replay: three views of one session's event
// journal, read through the daemon (spec/contracts/api.md API-A-1 keeps the
// API process out of the store). History is the daemon's own transcript
// reconstruction; trace and replay are built here from the raw journal, the
// same rows `nine trace` and `nine replay` read.

// historyTypes maps the daemon's transcript message types to the entry types
// the API declares. history_user is the daemon's name for a replayed prompt;
// the API calls it what it is.
var historyTypes = map[protocol.MsgType]apigen.HistoryEntryType{
	protocol.TypeHistoryUser:   "user_turn",
	protocol.TypeResponse:      "response",
	protocol.TypeToolStart:     "tool_start",
	protocol.TypeToolEnd:       "tool_end",
	protocol.TypeSubAgentStart: "sub_agent_start",
	protocol.TypeSubAgentEnd:   "sub_agent_end",
}

func (s *Server) GetConversationHistory(ctx context.Context, request apigen.GetConversationHistoryRequestObject) (apigen.GetConversationHistoryResponseObject, error) {
	if request.Id == "" {
		return apigen.GetConversationHistory400JSONResponse(
			errorBody("invalid_request", "missing conversation id", nil)), nil
	}
	p, err := page(request.Params.Limit, request.Params.Offset)
	if err != nil {
		return apigen.GetConversationHistory400JSONResponse(
			errorBody("invalid_request", err.Error(), nil)), nil
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.GetConversationHistory503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	msgs, err := cl.SessionHistory(request.Id)
	if err != nil {
		if errNotFound(err) {
			return apigen.GetConversationHistory404JSONResponse(
				errorBody("not_found", "conversation not found", map[string]any{"id": request.Id})), nil
		}
		return apigen.GetConversationHistory500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}

	entries := make([]apigen.HistoryEntry, 0, len(msgs))
	for _, m := range msgs {
		if e, ok := toHistoryEntry(m); ok {
			entries = append(entries, e)
		}
	}
	data, pagination := paginate(entries, p)
	return apigen.GetConversationHistory200JSONResponse{
		Data:       &data,
		Pagination: &pagination,
	}, nil
}

// toHistoryEntry maps one transcript message. A type the API does not declare
// is skipped rather than emitted under a value its enum does not allow.
func toHistoryEntry(m protocol.Msg) (apigen.HistoryEntry, bool) {
	typ, ok := historyTypes[m.Type]
	if !ok {
		return apigen.HistoryEntry{}, false
	}
	e := apigen.HistoryEntry{
		Type:       typ,
		AgentId:    m.AgentID,
		Text:       nonEmpty(m.Text),
		ToolName:   nonEmpty(m.ToolName),
		ToolOutput: nonEmpty(m.ToolOutput),
		SubAgentId: nonEmpty(m.SubAgentID),
		Role:       nonEmpty(m.Role),
		Status:     nonEmpty(m.Status),
	}
	if len(m.ToolInput) > 0 {
		e.ToolInput = rawJSON(m.ToolInput)
	}
	if m.Timestamp > 0 {
		e.Timestamp = ptr(time.UnixMilli(m.Timestamp).UTC())
	}
	if m.Turn > 0 {
		e.TurnNumber = ptr(m.Turn)
	}
	return e, true
}

func (s *Server) GetConversationTrace(ctx context.Context, request apigen.GetConversationTraceRequestObject) (apigen.GetConversationTraceResponseObject, error) {
	if request.Id == "" {
		return apigen.GetConversationTrace400JSONResponse(
			errorBody("invalid_request", "missing conversation id", nil)), nil
	}
	turn := 0
	if request.Params.Turn != nil {
		turn = *request.Params.Turn
	}
	if turn < 0 {
		return apigen.GetConversationTrace400JSONResponse(
			errorBody("invalid_request", fmt.Sprintf("turn must not be negative, got %d", turn), nil)), nil
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.GetConversationTrace503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	events, err := cl.SessionEvents(request.Id, turn)
	if err != nil {
		if errNotFound(err) {
			return apigen.GetConversationTrace404JSONResponse(
				errorBody("not_found", "conversation not found", map[string]any{"id": request.Id})), nil
		}
		return apigen.GetConversationTrace500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}

	out := apigen.GetConversationTrace200JSONResponse{
		AgentId: request.Id,
		Turns:   countTurns(events),
		Events:  toTraceEvents(events),
	}
	if derefBool(request.Params.SubAgents) {
		// Each sub-agent read is its own round trip on its own connection: a
		// client connection carries one request at a time.
		fetch := func(id string) ([]protocol.JournalEvent, error) {
			sub, err := s.getDaemonClient()
			if err != nil {
				return nil, err
			}
			defer sub.Close()
			return sub.SessionEvents(id, 0)
		}
		subs := collectSubAgents(request.Id, events, fetch)
		out.SubAgents = &subs
	}
	return out, nil
}

func toTraceEvents(events []protocol.JournalEvent) []apigen.TraceEvent {
	out := make([]apigen.TraceEvent, 0, len(events))
	for _, e := range events {
		te := apigen.TraceEvent{
			Seq:          e.Seq,
			Turn:         e.Turn,
			Type:         e.Type,
			SpanId:       nonEmpty(e.SpanID),
			ParentSpanId: nonEmpty(e.ParentSpanID),
		}
		if !e.TS.IsZero() {
			te.Timestamp = ptr(e.TS.UTC())
		}
		if len(e.Payload) > 0 {
			te.Payload = rawJSON(e.Payload)
		}
		out = append(out, te)
	}
	return out
}

// collectSubAgents reads the trace of every sub-agent spawned in events, and of
// the sub-agents those spawned, as `nine trace --sub-agents` expands them. The
// tree comes back flat, each entry naming its parent and the spawn event, in
// spawn order (depth first). seen stops a sub-agent being read twice. A
// sub-agent whose trace cannot be read says why in place rather than failing
// the whole trace.
func collectSubAgents(rootID string, events []protocol.JournalEvent, fetch func(string) ([]protocol.JournalEvent, error)) []apigen.TraceSubAgent {
	out := []apigen.TraceSubAgent{}
	seen := map[string]bool{rootID: true}
	var walk func(parent string, events []protocol.JournalEvent)
	walk = func(parent string, events []protocol.JournalEvent) {
		for _, e := range events {
			if e.Type != "sub_agent_start" {
				continue
			}
			var p subAgentPayload
			_ = json.Unmarshal(e.Payload, &p)
			if p.SubID == "" || seen[p.SubID] {
				continue
			}
			seen[p.SubID] = true
			sub := apigen.TraceSubAgent{AgentId: p.SubID, ParentAgentId: parent, SpawnedAtSeq: e.Seq}
			child, err := fetch(p.SubID)
			if err != nil {
				sub.Error = ptr(err.Error())
				out = append(out, sub)
				continue
			}
			sub.Events = ptr(toTraceEvents(child))
			out = append(out, sub)
			walk(p.SubID, child)
		}
	}
	walk(rootID, events)
	return out
}

func countTurns(events []protocol.JournalEvent) int {
	turns := map[int]bool{}
	for _, e := range events {
		turns[e.Turn] = true
	}
	return len(turns)
}

func (s *Server) ReplayTurn(ctx context.Context, request apigen.ReplayTurnRequestObject) (apigen.ReplayTurnResponseObject, error) {
	if request.Id == "" {
		return apigen.ReplayTurn400JSONResponse(
			errorBody("invalid_request", "missing conversation id", nil)), nil
	}
	if request.Body == nil || request.Body.Turn < 1 {
		return apigen.ReplayTurn400JSONResponse(
			errorBody("invalid_request", "turn is required and must be at least 1", nil)), nil
	}
	turn := request.Body.Turn

	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.ReplayTurn503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	events, err := cl.SessionEvents(request.Id, turn)
	if err != nil {
		if errNotFound(err) {
			return apigen.ReplayTurn404JSONResponse(
				errorBody("not_found", "conversation not found", map[string]any{"id": request.Id})), nil
		}
		return apigen.ReplayTurn500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}
	if len(events) == 0 {
		return apigen.ReplayTurn404JSONResponse(
			errorBody("not_found", "turn not found", map[string]any{"id": request.Id, "turn": turn})), nil
	}
	return apigen.ReplayTurn200JSONResponse(buildReplay(request.Id, turn, events)), nil
}

// buildReplay reconstructs one turn from its journal rows. An LLM call's
// request and response share a span, as do a tool call's start and end, so
// each pair is joined on span id; a tool's outbound HTTP is parented to the
// tool call's span.
func buildReplay(agentID string, turn int, events []protocol.JournalEvent) apigen.ReplayTurnResponse {
	out := apigen.ReplayTurnResponse{
		AgentId:   agentID,
		Turn:      turn,
		LlmCalls:  []apigen.ReplayLLMCall{},
		ToolCalls: []apigen.ReplayToolCall{},
		SubAgents: []apigen.ReplaySubAgent{},
	}
	llmBySpan := map[string]int{}
	toolBySpan := map[string]int{}
	subByID := map[string]int{}

	llmCall := func(span string) *apigen.ReplayLLMCall {
		i, ok := llmBySpan[span]
		if !ok {
			i = len(out.LlmCalls)
			llmBySpan[span] = i
			out.LlmCalls = append(out.LlmCalls, apigen.ReplayLLMCall{})
		}
		return &out.LlmCalls[i]
	}
	toolCall := func(span string) *apigen.ReplayToolCall {
		i, ok := toolBySpan[span]
		if !ok {
			i = len(out.ToolCalls)
			toolBySpan[span] = i
			out.ToolCalls = append(out.ToolCalls, apigen.ReplayToolCall{})
		}
		return &out.ToolCalls[i]
	}

	for _, e := range events {
		switch e.Type {
		case "turn_start":
			var p turnStartPayload
			_ = json.Unmarshal(e.Payload, &p)
			out.Trigger = nonEmpty(p.Trigger)
			out.Input = ptr(p.Input)
		case "llm_request":
			var p llmRequestPayload
			_ = json.Unmarshal(e.Payload, &p)
			c := llmCall(e.SpanID)
			c.N = p.LLMCallN
			c.System = nonEmpty(p.System)
			c.MessageCount = ptr(len(p.Messages))
			c.ToolNames = ptr(nonNil(p.ToolNames))
			c.TokensUsed = ptr(p.TokensUsed)
			c.Budget = ptr(p.Budget)
		case "llm_response":
			var p llmResponsePayload
			_ = json.Unmarshal(e.Payload, &p)
			c := llmCall(e.SpanID)
			c.N = p.LLMCallN
			c.StopReason = nonEmpty(p.StopReason)
			c.Text = nonEmpty(p.Text)
			if p.InputTokens > 0 || p.OutputTokens > 0 {
				c.InputTokens = ptr(p.InputTokens)
				c.OutputTokens = ptr(p.OutputTokens)
			}
			calls := make([]apigen.ReplayRequestedTool, 0, len(p.ToolCalls))
			for _, tc := range p.ToolCalls {
				rt := apigen.ReplayRequestedTool{Name: tc.Name}
				if len(tc.Input) > 0 {
					rt.Input = rawJSON(tc.Input)
				}
				calls = append(calls, rt)
			}
			c.ToolCalls = &calls
		case "tool_start":
			var p toolStartPayload
			_ = json.Unmarshal(e.Payload, &p)
			t := toolCall(e.SpanID)
			t.Name = p.Name
			if len(p.Input) > 0 {
				t.Input = rawJSON(p.Input)
			}
		case "tool_end":
			var p toolEndPayload
			_ = json.Unmarshal(e.Payload, &p)
			t := toolCall(e.SpanID)
			t.Name = p.Name
			if t.Input == nil && len(p.Input) > 0 {
				t.Input = rawJSON(p.Input)
			}
			t.Output = ptr(p.Output)
			t.Error = nonEmpty(p.Error)
			t.DurationMs = ptr(p.DurationMs)
			t.Attempts = ptr(p.Attempts)
			if p.Truncated {
				t.Truncated = ptr(true)
			}
			if p.OutputChars > 0 {
				t.OutputChars = ptr(p.OutputChars)
			}
			t.SpillPath = nonEmpty(p.SpillPath)
		case "tool_http":
			var p toolHTTPPayload
			_ = json.Unmarshal(e.Payload, &p)
			t := toolCall(e.ParentSpanID)
			if t.Name == "" {
				t.Name = p.Tool
			}
			req := apigen.ReplayHTTPRequest{
				Method:     nonEmpty(p.Method),
				Url:        nonEmpty(p.URL),
				Host:       nonEmpty(p.Host),
				Bytes:      ptr(p.Bytes),
				DurationMs: ptr(p.DurationMs),
				Error:      nonEmpty(p.Error),
			}
			if p.Status != 0 {
				req.Status = ptr(p.Status)
			}
			if p.Truncated {
				req.Truncated = ptr(true)
			}
			if t.Http == nil {
				t.Http = &[]apigen.ReplayHTTPRequest{}
			}
			*t.Http = append(*t.Http, req)
		case "sub_agent_start", "sub_agent_end":
			var p subAgentPayload
			_ = json.Unmarshal(e.Payload, &p)
			if p.SubID == "" {
				continue
			}
			i, ok := subByID[p.SubID]
			if !ok {
				i = len(out.SubAgents)
				subByID[p.SubID] = i
				out.SubAgents = append(out.SubAgents, apigen.ReplaySubAgent{Id: p.SubID})
			}
			sa := &out.SubAgents[i]
			if p.Task != "" {
				sa.Task = ptr(p.Task)
			}
			if p.Role != "" {
				sa.Role = ptr(p.Role)
			}
			if e.Type == "sub_agent_end" {
				sa.Status = nonEmpty(p.Status)
			}
		case "turn_end":
			var p turnEndPayload
			_ = json.Unmarshal(e.Payload, &p)
			out.Result = &apigen.ReplayResult{
				Text:       ptr(p.Result),
				Error:      nonEmpty(p.Error),
				ToolCount:  ptr(p.ToolCount),
				DurationMs: ptr(p.DurationMs),
			}
		}
	}
	return out
}

// --- journal payloads ---
//
// These mirror the bodies the daemon journals (internal/runtime/journal.go);
// spec/contracts/event-journal.md is their contract. llm.Message and
// llm.ToolCall carry no json tags, so their fields arrive under Go names.

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

type llmRequestPayload struct {
	System     string            `json:"system"`
	Messages   []json.RawMessage `json:"messages"`
	ToolNames  []string          `json:"tool_names"`
	TokensUsed int               `json:"tokens_used"`
	Budget     int               `json:"budget"`
	LLMCallN   int               `json:"llm_call_n"`
}

type llmResponsePayload struct {
	Text      string `json:"text"`
	ToolCalls []struct {
		Name  string          `json:"Name"`
		Input json.RawMessage `json:"Input"`
	} `json:"tool_calls"`
	StopReason   string `json:"stop_reason"`
	LLMCallN     int    `json:"llm_call_n"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
}

type toolStartPayload struct {
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type toolEndPayload struct {
	Name        string          `json:"name"`
	Input       json.RawMessage `json:"input"`
	Output      string          `json:"output"`
	Truncated   bool            `json:"truncated"`
	SpillPath   string          `json:"spill_path"`
	OutputChars int             `json:"output_chars"`
	DurationMs  int64           `json:"duration_ms"`
	Attempts    int             `json:"attempts"`
	Error       string          `json:"error"`
}

type toolHTTPPayload struct {
	Tool       string `json:"tool"`
	Method     string `json:"method"`
	Host       string `json:"host"`
	URL        string `json:"url"`
	Status     int    `json:"status"`
	Bytes      int    `json:"bytes"`
	Truncated  bool   `json:"truncated"`
	DurationMs int64  `json:"duration_ms"`
	Error      string `json:"error"`
}

type subAgentPayload struct {
	SubID  string `json:"sub_id"`
	Task   string `json:"task"`
	Status string `json:"status"`
	Role   string `json:"role"`
}

// rawJSON passes a JSON value through unchanged. A json.RawMessage in an
// interface{} field marshals as the bytes it holds, so tool arguments and
// payloads reach the client exactly as journaled rather than re-encoded.
func rawJSON(b []byte) any { return json.RawMessage(b) }

// nonEmpty returns nil for "", so an absent value is omitted rather than sent
// as an empty string.
func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
