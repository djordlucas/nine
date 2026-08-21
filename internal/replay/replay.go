// Package replay reconstructs a Nine session from its durable event journal and
// re-executes it deterministically, feeding the recorded llm.Responses and tool
// outputs back into a real agent loop instead of hitting a live LLM or running
// real tools (adr/event-log.md §3(2), §10, §11 v3).
//
// This is "re-execution from recorded responses", not "press play against
// production": a RecordedProvider returns the logged response for each inner LLM
// call and a recorded dispatcher returns the logged tool observation, so a
// recorded real session replays with no side effects — the automatic form of
// the hand-written scriptedProvider used across the test suite. Recording once
// and asserting the replay reproduces the same answers gives golden-transcript
// regression tests.
//
// Recorded is not safe for concurrent use; replay runs a session's turns
// sequentially on one loop (recorded sub-agent results are returned inline, so
// nothing is re-spawned).
package replay

import (
	"context"
	"encoding/json"
	"fmt"

	"nine/internal/agent"
	ninectx "nine/internal/context"
	"nine/internal/llm"
	"nine/internal/memory"
)

// RecordedTurn is one turn's replay input and its recorded final answer.
type RecordedTurn struct {
	Input   string // the exact text handed to loop.Run (post-notification prepend)
	Trigger string // "user" | "idle"
	Result  string // the recorded final answer, used as the golden expectation
}

// Recorded is a session's journal grouped for re-execution: the per-turn inputs
// and, flattened in execution (seq) order across the whole session, the LLM
// responses and tool observations to feed back.
type Recorded struct {
	Turns     []RecordedTurn
	responses []llm.Response
	toolOut   []string
	toolNames []string
}

// FromEvents groups a flat, seq-ordered journal (as returned by
// memory.SessionEventsByAgent) into a Recorded ready to replay. It reconstructs
// llm.Responses from llm_response payloads and tool observations from tool_end
// payloads, in journal order.
func FromEvents(events []memory.SessionEvent) (*Recorded, error) {
	rec := &Recorded{}
	inputs := map[int]string{}
	triggers := map[int]string{}
	results := map[int]string{}
	var order []int
	seen := map[int]bool{}

	for _, e := range events {
		if !seen[e.Turn] {
			seen[e.Turn] = true
			order = append(order, e.Turn)
		}
		switch e.Type {
		case "turn_start":
			var p struct {
				Input   string `json:"input"`
				Trigger string `json:"trigger"`
			}
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, fmt.Errorf("turn_start seq %d: %w", e.Seq, err)
			}
			inputs[e.Turn] = p.Input
			triggers[e.Turn] = p.Trigger
		case "turn_end":
			var p struct {
				Result string `json:"result"`
			}
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, fmt.Errorf("turn_end seq %d: %w", e.Seq, err)
			}
			results[e.Turn] = p.Result
		case "llm_response":
			// llm.ToolCall has no json tags, so its recorded keys are ID/Name/
			// Input — matched by unmarshaling straight back into llm.ToolCall.
			var p struct {
				Text       string         `json:"text"`
				ToolCalls  []llm.ToolCall `json:"tool_calls"`
				StopReason string         `json:"stop_reason"`
			}
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, fmt.Errorf("llm_response seq %d: %w", e.Seq, err)
			}
			rec.responses = append(rec.responses, llm.Response{
				Text:       p.Text,
				ToolCalls:  p.ToolCalls,
				StopReason: p.StopReason,
			})
		case "tool_end":
			var p struct {
				Name   string `json:"name"`
				Output string `json:"output"`
			}
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, fmt.Errorf("tool_end seq %d: %w", e.Seq, err)
			}
			rec.toolOut = append(rec.toolOut, p.Output)
			rec.toolNames = append(rec.toolNames, p.Name)
		}
	}

	for _, t := range order {
		rec.Turns = append(rec.Turns, RecordedTurn{
			Input:   inputs[t],
			Trigger: triggers[t],
			Result:  results[t],
		})
	}
	return rec, nil
}

// Provider returns an llm.Provider that ignores each request and returns the
// recorded responses in order. It errors if the loop asks for more responses
// than were recorded (a sign replay diverged).
func (r *Recorded) Provider() llm.Provider {
	i := 0
	return llm.ProviderFunc(func(_ context.Context, _ llm.Request) (llm.Response, error) {
		if i >= len(r.responses) {
			return llm.Response{}, fmt.Errorf("replay: exhausted %d recorded LLM responses", len(r.responses))
		}
		resp := r.responses[i]
		i++
		return resp, nil
	})
}

// Dispatcher returns an agent.Dispatcher whose every recorded tool name resolves
// to a handler that returns the recorded observations in order, sharing one
// cursor so outputs are consumed in dispatch order regardless of tool name. No
// real tool runs.
func (r *Recorded) Dispatcher() *agent.Dispatcher {
	d := agent.New()
	i := 0
	handler := func(_ context.Context, _ json.RawMessage) (string, error) {
		if i >= len(r.toolOut) {
			return "", fmt.Errorf("replay: exhausted %d recorded tool outputs", len(r.toolOut))
		}
		out := r.toolOut[i]
		i++
		return out, nil
	}
	registered := map[string]bool{}
	for _, name := range r.toolNames {
		if !registered[name] {
			registered[name] = true
			d.InjectHandler(name, handler)
		}
	}
	return d
}

// Session re-executes every recorded turn on a fresh loop wired to the recorded
// provider and dispatcher, returning the answers produced. No live LLM or tool
// call occurs; a divergence (more calls than recorded) surfaces as an error.
func Session(ctx context.Context, rec *Recorded) ([]string, error) {
	builder := ninectx.New(ninectx.Config{Budget: 1_000_000})
	queue := llm.NewQueue(rec.Provider(), 1)
	loop := agent.NewLoop(agent.Config{SystemCore: "replay"}, builder, queue, rec.Dispatcher())

	answers := make([]string, 0, len(rec.Turns))
	for i, t := range rec.Turns {
		ans, err := loop.Run(ctx, t.Input)
		if err != nil {
			return answers, fmt.Errorf("replay turn %d: %w", i+1, err)
		}
		answers = append(answers, ans)
	}
	return answers, nil
}
