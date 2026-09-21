package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"nine/internal/llm"
)

// RefMarker is the JSON-Schema extension keyword a tool uses to declare that a
// string property carries a **file-store path**, not a literal value. Before
// dispatching, the daemon reads that path out of the store and substitutes the
// content, so the tool receives the bytes while the model only ever handles a
// short handle:
//
//	"content": {"type": "string", "x-nine-ref": true,
//	            "description": "File-store path whose content to parse."}
//
// This is deliberately **declared, not inferred**. Expanding any argument that
// merely looks like a path would corrupt tools whose arguments are genuinely
// paths — read_file(path) would have its path replaced by the file's contents.
// Only properties a tool marks are ever touched.
const RefMarker = "x-nine-ref"

// MaxRefBytes caps how much content one ref expansion may inject into a call's
// arguments. Refs remove data from the model's context, but the bytes still
// cross the plugin socket into a subprocess that may buffer them whole, so the
// expansion needs its own ceiling.
const MaxRefBytes = 8 << 20 // 8 MiB

// RefResolver returns the content stored at a file-store path. A non-nil error
// fails the tool call — an unresolvable ref is a real error the model must see
// and correct, not something to paper over by passing the raw path through.
type RefResolver func(ctx context.Context, path string) (string, error)

// SetRefResolver registers fn as the reader for x-nine-ref arguments. With no
// resolver registered, ref arguments are passed through untouched.
func (d *Dispatcher) SetRefResolver(fn RefResolver) { d.resolveRef = fn }

// declareRefParams indexes the ref-marked properties of defs so Dispatch knows
// which arguments to expand. Called for core tool definitions at New and for
// each plugin's advertised tools at RegisterPlugin.
func (d *Dispatcher) declareRefParams(name string, schema json.RawMessage) {
	if params := refParams(schema); len(params) > 0 {
		d.refParams[name] = params
	}
}

func (d *Dispatcher) declareToolDefRefParams(defs []llm.ToolDef) {
	for _, def := range defs {
		d.declareRefParams(def.Name, def.InputSchema)
	}
}

// refParams returns the names of the schema's top-level string properties
// marked with RefMarker, sorted for deterministic expansion order. A schema
// that does not parse yields none — an unreadable schema must not make the
// dispatcher guess.
func refParams(schema json.RawMessage) []string {
	if len(schema) == 0 {
		return nil
	}
	var doc struct {
		Properties map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(schema, &doc); err != nil {
		return nil
	}
	var names []string
	for name, prop := range doc.Properties {
		if marked, ok := prop[RefMarker].(bool); ok && marked {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// expandRefs replaces each ref-marked argument's path with the content stored
// at that path. Arguments the tool did not mark are untouched, as is an absent
// or empty ref argument — marking a property makes it ref-capable, not
// mandatory.
//
// Expansion happens after the approval gate and does not affect what the
// journal or post-call hooks record, both of which see the model's original
// arguments. That is intentional: the trace should show the handle the model
// actually chose, not a megabyte the daemon substituted.
func (d *Dispatcher) expandRefs(ctx context.Context, toolName string, args json.RawMessage) (json.RawMessage, error) {
	names := d.refParams[toolName]
	if len(names) == 0 || d.resolveRef == nil {
		return args, nil
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(args, &obj); err != nil {
		return args, nil // not a JSON object; nothing to expand
	}

	expanded := false
	for _, name := range names {
		raw, ok := obj[name]
		if !ok {
			continue
		}
		var path string
		if err := json.Unmarshal(raw, &path); err != nil || path == "" {
			continue
		}
		content, err := d.resolveRef(ctx, path)
		if err != nil {
			return nil, fmt.Errorf("%s: resolving %s reference %q: %w", toolName, name, path, err)
		}
		if len(content) > MaxRefBytes {
			return nil, fmt.Errorf("%s: %s reference %q is %d bytes, over the %d-byte limit; read it in slices with read_file(path, offset, limit) instead",
				toolName, name, path, len(content), MaxRefBytes)
		}
		enc, err := json.Marshal(content)
		if err != nil {
			return nil, fmt.Errorf("%s: encoding %s reference %q: %w", toolName, name, path, err)
		}
		obj[name] = enc
		expanded = true
	}
	if !expanded {
		return args, nil
	}
	return json.Marshal(obj)
}
