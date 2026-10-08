package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"nine/internal/llm"
)

// GeneratedToolNames are the core-intercepted tools of the generated tier. They
// are registered only when `[tools.agent]` is on, so an instance that has not
// opted in advertises neither.
var GeneratedToolNames = []string{"tool_write", "tool_delete", "js_eval"}

// The prompt guidance below is doing real work, not decoration.
//
// An agent that can write tools will write tools, and every one competes for the
// context budget in tool selection — so a catalog of 200 half-redundant
// generated tools degrades ranking for the *built-in* tools too, and the agent
// gets worse at everything (docs/sandboxed-tools.md §9.2). The eval that matters
// here is not "can it write a tool" but "does the catalog stay small and get
// used", which makes the bar the description sets the main lever available.
var generatedToolDefs = []llm.ToolDef{
	{
		Name:        "tool_write",
		DisplayName: "Write Tool",
		Description: "Create or replace one of your own tools: a small JavaScript program that becomes a permanent, callable tool. " +
			"Use this ONLY for a reusable deterministic transform you expect to need again — parsing a format, converting units, computing something fiddly. " +
			"Do NOT use it for a one-off computation you could do inline, and do not create a tool you will call once. " +
			"Prefer js_eval to try something out; write it as a tool only once it works and you want to keep it. " +
			"Writing an existing name replaces it. The tool is callable from your NEXT turn, not this one.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"required":["name","description","source"],
			"properties":{
				"name":{"type":"string","description":"Tool name, lowercase with underscores, e.g. iso_week_of."},
				"description":{"type":"string","description":"What the tool does and when to reach for it. This is the whole basis on which you will later decide to call it, so write it for a reader who has forgotten the context."},
				"input_schema":{"type":"object","description":"JSON Schema for the tool's arguments."},
				"source":{"type":"string","description":"ES2023 JavaScript with a default-exported function: export default ({arg}) => result. No Node APIs (no require, no fs/http/path/Buffer/process/crypto modules); the only imports are nine's own modules. nine:csv, nine:date, nine:diff and nine:html need no capability. fs → import { readFileText, writeFile, readDir } from \"nine:fs\", with workspace files under /work (notes/a.txt is /work/notes/a.txt); net → the global fetch; env → import { get } from \"nine:env\"."},
				"capabilities":{"type":"object","description":"What the tool needs. Omit entirely unless it genuinely needs reach — a pure transform needs nothing and runs anywhere. Shape: {\"fs\":[\"read\"],\"net\":[\"http\"],\"env\":[\"TZ\"]}.","properties":{
					"fs":{"type":"array","items":{"type":"string","enum":["read","write"]}},
					"net":{"type":"array","items":{"type":"string","enum":["http"]}},
					"env":{"type":"array","items":{"type":"string"}}
				}},
				"standing":{"type":"object","description":"Ask for this tool to run indefinitely on its own cadence, starting now — not just once. Requires resumable. A human is ALWAYS asked to approve this, whatever the operator's other settings, and it may be disabled entirely. Use it only for work that genuinely needs to keep running; a tool that answers a question should not be standing.","properties":{
					"interval":{"type":"string","description":"How often a new cycle starts, e.g. \"10s\" or \"1h\". Give this or schedule, not both."},
					"schedule":{"type":"string","description":"A 5-field cron expression, as an alternative to interval."},
					"args":{"type":"object","description":"Arguments passed at the start of every cycle."}
				}},
				"resumable":{"type":"boolean","description":"Set only for work too long for one call. A resumable tool does a bounded slice per call and returns again({cursor,progress,afterMs}) from \"nine:job\" to be called again with its cursor; returning a value finishes it. It runs as a background job, so its result reaches you on a later turn via job_check/job_wait. May be disabled by the operator."}
			}}`),
	},
	{
		Name:        "tool_delete",
		DisplayName: "Delete Tool",
		Description: "Delete one of your own tools. Use it to clean up a tool that turned out to be wrong or redundant — keeping the catalog small keeps every tool easier to find, including the built-in ones. Built-in, plugin, and operator-installed tools cannot be deleted.",
		InputSchema: json.RawMessage(`{"type":"object","required":["name"],"properties":{"name":{"type":"string"}}}`),
	},
	{
		Name:        "js_eval",
		DisplayName: "Evaluate JavaScript",
		Description: "Run a JavaScript snippet once in the sandbox and return its result. Nothing is saved. " +
			"This is the right tool for a one-off computation, for checking that an approach works before committing it with tool_write, and for anything you would otherwise be tempted to create a single-use tool for. " +
			"Same rules as tool_write: ES2023 only, default-export a function, no Node APIs.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"required":["source"],
			"properties":{
				"source":{"type":"string","description":"ES2023 JavaScript: export default (args) => result."},
				"args":{"type":"object","description":"Arguments passed to the exported function."},
				"capabilities":{"type":"object","description":"Same shape as tool_write. Omit unless genuinely needed."}
			}}`),
	},
}

// GeneratedToolDefs returns the definitions for the generated tier, filtered to
// what the operator enabled: js_eval has its own switch, and tool_write/
// tool_delete come as a pair.
func GeneratedToolDefs(evalEnabled bool) []llm.ToolDef {
	var out []llm.ToolDef
	for _, d := range generatedToolDefs {
		if d.Name == "js_eval" && !evalEnabled {
			continue
		}
		out = append(out, d)
	}
	return out
}

// GeneratedToolStore is the persistence the generated tier needs, narrowed so
// the agent package does not depend on the memory store's full surface.
type GeneratedToolStore interface {
	Write(ctx context.Context, t GeneratedToolSpec) (WriteResult, error)
	Delete(ctx context.Context, name string) error
	Eval(ctx context.Context, source string, caps json.RawMessage, args json.RawMessage) (string, error)
}

// WriteResult reports what a write did, so the model can be told something more
// useful than "ok" — which tools the cap evicted, and whether the write changed
// anything at all. A model that cannot tell a no-op rewrite from a real one has
// no signal that it is looping, and small models do loop here.
type WriteResult struct {
	// Evicted names the least-recently-called tools dropped to stay under the cap.
	Evicted []string
	// Unchanged reports a write byte-identical to the stored tool: same source,
	// description and schema.
	Unchanged bool
}

// GeneratedToolSpec is one proposed tool, as the model described it.
type GeneratedToolSpec struct {
	Name         string
	Description  string
	InputSchema  json.RawMessage
	Source       string
	Capabilities json.RawMessage
	// Resumable asks for the long-running lifecycle. Gated by the operator
	// separately from the capability ceiling, which bounds reach rather than
	// duration.
	Resumable bool
	// Standing asks for the tool to be run indefinitely, starting now. Nil is
	// the ordinary case.
	Standing *StandingRequest
}

// StandingRequest is a generated tool asking to be run standing.
//
// It is a request and never a grant: the operator's allow_standing decides
// whether it is possible at all, max_standing bounds how many may exist, and a
// human approves each one. Nine can ask; it cannot confer.
type StandingRequest struct {
	Interval string          `json:"interval,omitempty"`
	Schedule string          `json:"schedule,omitempty"`
	Args     json.RawMessage `json:"args,omitempty"`
}

// RegisterGeneratedTools registers tool_write, tool_delete, and js_eval.
//
// Every one of these is a *code* path, never a *capability* path: nothing here
// writes a grant, and the store it talks to has no way to. That asymmetry is the
// whole design (docs/sandboxed-tools.md §2) — the agent writes the code, the
// operator writes the grants, and they are never the same actor.
func RegisterGeneratedTools(d *Dispatcher, s GeneratedToolStore, evalEnabled bool) {
	if s == nil {
		return
	}

	d.handlers["tool_write"] = func(ctx context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Name         string           `json:"name"`
			Description  string           `json:"description"`
			InputSchema  json.RawMessage  `json:"input_schema,omitempty"`
			Source       string           `json:"source"`
			Capabilities json.RawMessage  `json:"capabilities,omitempty"`
			Resumable    bool             `json:"resumable"`
			Standing     *StandingRequest `json:"standing"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("tool_write: %w", err)
		}
		switch {
		case req.Name == "":
			return "", fmt.Errorf("tool_write: name is required")
		case req.Description == "":
			// The description is the entire basis on which this tool will later be
			// selected, so an empty one produces a tool that exists and is never
			// found.
			return "", fmt.Errorf("tool_write: description is required — it is how you will find this tool later")
		case req.Source == "":
			return "", fmt.Errorf("tool_write: source is required")
		}
		// A schema that is not a JSON object is refused here rather than stored:
		// it becomes this tool's `parameters` in every later LLM request, and a
		// provider that rejects the malformed field fails the whole turn — one bad
		// tool would take down every turn of every session that advertises it. The
		// refusal is a message the model can act on (docs/sandboxed-tools.md §7).
		if len(req.InputSchema) > 0 {
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(req.InputSchema, &obj); err != nil {
				return "", fmt.Errorf(
					"tool_write: input_schema must be a JSON Schema object like "+
						`{"type":"object","properties":{...}}, not %s`, firstToken(req.InputSchema))
			}
		}

		// Whether the tool is callable *now* is the dispatcher's to answer: a tool
		// this loop already carries can be called in this very turn, and only a
		// name new to the loop waits for the next one (docs/sandboxed-tools.md §9.1).
		callableNow := d.Has(req.Name)
		res, err := s.Write(ctx, GeneratedToolSpec{
			Name:         req.Name,
			Description:  req.Description,
			InputSchema:  req.InputSchema,
			Source:       req.Source,
			Capabilities: req.Capabilities,
			Resumable:    req.Resumable,
			Standing:     req.Standing,
		})
		if err != nil {
			return "", err
		}

		// Say exactly when the tool can be called, because the obvious failure is a
		// model that writes a tool and then cannot tell whether to call it or write
		// it again. A name new to this loop waits for the next turn, which keeps the
		// tool set it started with (§9.1). A tool the loop already carries is callable
		// in this turn, and a rewrite of it takes effect on the next call, because the
		// handler resolves the source by name at call time.
		// A write that changes nothing is a failure, not a success: nothing was
		// written. Reporting it as done is what lets a model rewrite the same
		// source turn after turn, which is exactly the loop small models fall into
		// once they have a tool they cannot decide to call.
		if res.Unchanged {
			return "", fmt.Errorf(
				"tool %q already exists with this exact source; nothing was written. "+
					"It is in your tools: call %s directly instead of writing it again",
				req.Name, req.Name)
		}
		var msg string
		switch {
		case callableNow:
			msg = fmt.Sprintf("Updated tool %q. The new source takes effect on your next call to it.", req.Name)
		default:
			msg = fmt.Sprintf("Wrote tool %q. It is callable from your next turn.", req.Name)
		}
		if callableNow {
			msg += fmt.Sprintf(" It is in your tools now: call %s directly rather than writing it again.", req.Name)
		}
		if len(res.Evicted) > 0 {
			msg += fmt.Sprintf(" Evicted %d least-recently-used tool(s) to stay under the cap: %v.",
				len(res.Evicted), res.Evicted)
		}
		return msg, nil
	}

	d.handlers["tool_delete"] = func(ctx context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("tool_delete: %w", err)
		}
		if req.Name == "" {
			return "", fmt.Errorf("tool_delete: name is required")
		}
		if err := s.Delete(ctx, req.Name); err != nil {
			return "", err
		}
		return fmt.Sprintf("Deleted tool %q.", req.Name), nil
	}

	if !evalEnabled {
		return
	}
	d.handlers["js_eval"] = func(ctx context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Source       string          `json:"source"`
			Args         json.RawMessage `json:"args,omitempty"`
			Capabilities json.RawMessage `json:"capabilities,omitempty"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("js_eval: %w", err)
		}
		if req.Source == "" {
			return "", fmt.Errorf("js_eval: source is required")
		}
		return s.Eval(ctx, req.Source, req.Capabilities, req.Args)
	}
}

// firstToken describes the shape of a malformed JSON value for an error message,
// without echoing the whole value back at the model.
func firstToken(raw json.RawMessage) string {
	t := strings.TrimSpace(string(raw))
	if t == "" {
		return "an empty value"
	}
	switch t[0] {
	case '"':
		return "a string"
	case '[':
		return "an array"
	}
	if len(t) > 20 {
		t = t[:20] + "…"
	}
	return t
}
