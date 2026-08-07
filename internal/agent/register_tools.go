package agent

import (
	"context"
	"encoding/json"
	"fmt"

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
				"source":{"type":"string","description":"ES2023 JavaScript with a default-exported function: export default ({arg}) => result. No imports, no Node APIs (no fs/http/path/Buffer/process/crypto), no fetch unless you declare the http capability."},
				"capabilities":{"type":"object","description":"What the tool needs. Omit entirely unless it genuinely needs reach — a pure transform needs nothing and runs anywhere. Shape: {\"fs\":[\"read\"],\"net\":[\"http\"],\"env\":[\"TZ\"]}.","properties":{
					"fs":{"type":"array","items":{"type":"string","enum":["read","write"]}},
					"net":{"type":"array","items":{"type":"string","enum":["http"]}},
					"env":{"type":"array","items":{"type":"string"}}
				}}
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
	Write(ctx context.Context, t GeneratedToolSpec) (evicted []string, err error)
	Delete(ctx context.Context, name string) error
	Eval(ctx context.Context, source string, caps json.RawMessage, args json.RawMessage) (string, error)
}

// GeneratedToolSpec is one proposed tool, as the model described it.
type GeneratedToolSpec struct {
	Name         string
	Description  string
	InputSchema  json.RawMessage
	Source       string
	Capabilities json.RawMessage
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
			Name         string          `json:"name"`
			Description  string          `json:"description"`
			InputSchema  json.RawMessage `json:"input_schema"`
			Source       string          `json:"source"`
			Capabilities json.RawMessage `json:"capabilities"`
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

		evicted, err := s.Write(ctx, GeneratedToolSpec{
			Name:         req.Name,
			Description:  req.Description,
			InputSchema:  req.InputSchema,
			Source:       req.Source,
			Capabilities: req.Capabilities,
		})
		if err != nil {
			return "", err
		}

		// Saying "next turn" explicitly heads off the obvious failure: the model
		// writes a tool and immediately tries to call it, which cannot work — the
		// tool set is fixed when a loop is built (§9.1).
		msg := fmt.Sprintf("Wrote tool %q. It is callable from your next turn.", req.Name)
		if len(evicted) > 0 {
			msg += fmt.Sprintf(" Evicted %d least-recently-used tool(s) to stay under the cap: %v.", len(evicted), evicted)
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
			Args         json.RawMessage `json:"args"`
			Capabilities json.RawMessage `json:"capabilities"`
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
