// Package agent contains the tool dispatcher and agent loop.
package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"nine/internal/plugin"
)

// DefaultMaxOutputTokens is the per-result token cap. Results exceeding it are
// spilled to the file store and replaced with a preview, or — with no spill
// sink registered — truncated, before being appended to the scratchpad.
const DefaultMaxOutputTokens = 2048

// Hook is called after a tool call succeeds.
type Hook func(toolName string, args json.RawMessage, output string)

// CallResult is the output of a dispatched tool call.
type CallResult struct {
	Output    string
	Truncated bool // true when the output exceeded the cap
	// SpillPath is the file-store path holding the full output, set only when
	// an over-cap result was spilled rather than merely truncated.
	SpillPath string
	// OutputChars is the length of the tool's original output, set only when it
	// exceeded the cap. Output alone cannot report it — it is the preview.
	// Characters, not bytes: it is the unit file_fetch's offset/limit address,
	// so the model can do arithmetic between the two.
	OutputChars int
}

// ApprovalFn is consulted before a gated tool runs. Returning a non-nil error
// blocks the call; the error is surfaced to the model as a normal tool failure.
type ApprovalFn func(ctx context.Context, toolName string, args json.RawMessage) error

// Dispatcher routes tool calls to registered plugins or to core-intercepted
// handlers, expands ref arguments, applies post-call hooks, and enforces the
// output token cap.
type Dispatcher struct {
	handlers map[string]func(context.Context, json.RawMessage) (string, error)
	hooks    map[string][]Hook
	gated    map[string]bool
	approve  ApprovalFn

	// Large-output plumbing (docs/tool-output-spill.md): spill writes over-cap
	// results out to the file store, resolveRef reads them back into the
	// arguments of tools that declare a ref parameter, and refParams indexes
	// which arguments those are.
	spill           SpillFn
	resolveRef      RefResolver
	refParams       map[string][]string
	maxOutputTokens int
}

// New returns an empty Dispatcher. Register handlers with the Register*
// functions before dispatching.
func New() *Dispatcher {
	d := &Dispatcher{
		handlers:        make(map[string]func(context.Context, json.RawMessage) (string, error)),
		hooks:           make(map[string][]Hook),
		refParams:       make(map[string][]string),
		maxOutputTokens: DefaultMaxOutputTokens,
	}
	// Core-intercepted tools declare their ref parameters in the same static
	// definitions the model is shown, so index them up front.
	d.declareToolDefRefParams(InterceptedDefs)
	return d
}

// SetApproval registers fn as an approval gate for the named tools. Before any
// gated tool is dispatched, fn is called; a non-nil error blocks the call.
// Intended for interactive sessions only (see internal/runtime/hitl.go).
func (d *Dispatcher) SetApproval(toolNames []string, fn ApprovalFn) {
	if len(toolNames) == 0 || fn == nil {
		return
	}
	d.gated = make(map[string]bool, len(toolNames))
	for _, n := range toolNames {
		d.gated[n] = true
	}
	d.approve = fn
}

// RegisterPlugin indexes all tools advertised by p so they can be dispatched.
func (d *Dispatcher) RegisterPlugin(m *plugin.Manager, p *plugin.Plugin) {
	for _, t := range p.Tools {
		toolName := t.Name
		pp := p
		d.declareRefParams(toolName, t.InputSchema)
		d.handlers[toolName] = func(ctx context.Context, args json.RawMessage) (string, error) {
			cr, err := m.Call(ctx, pp, toolName, args)
			if err != nil {
				return "", err
			}
			return cr.Output, nil
		}
	}
}

// InjectHandler registers a handler function directly by tool name.
// Intended for testing; overwrites any existing handler.
func (d *Dispatcher) InjectHandler(toolName string, fn func(context.Context, json.RawMessage) (string, error)) {
	d.handlers[toolName] = fn
}

// InjectHandlerWithSchema is InjectHandler for a tool whose input schema
// matters — in practice, one declaring an x-nine-ref parameter (refs.go).
// Intended for testing; production tools get their schemas from RegisterPlugin
// (plugins) or InterceptedDefs (core).
func (d *Dispatcher) InjectHandlerWithSchema(toolName string, schema json.RawMessage, fn func(context.Context, json.RawMessage) (string, error)) {
	d.declareRefParams(toolName, schema)
	d.handlers[toolName] = fn
}

// RestrictTo removes every registered handler whose name is not in names.
// It enforces a role's tool allowlist at the dispatch boundary (docs/roles.md
// R-ROLE.4): a tool outside the role's set cannot be called even if the model
// hallucinates it — Dispatch returns "unknown tool". Call after all Register*
// functions; names absent from the registered set are ignored (R-ROLE.5).
func (d *Dispatcher) RestrictTo(names []string) {
	allowed := make(map[string]bool, len(names))
	for _, n := range names {
		allowed[n] = true
	}
	for name := range d.handlers {
		if !allowed[name] {
			delete(d.handlers, name)
		}
	}
}

// AddHook registers h to be called after every successful call to toolName.
func (d *Dispatcher) AddHook(toolName string, h Hook) {
	d.hooks[toolName] = append(d.hooks[toolName], h)
}

// Dispatch routes the call, expands ref arguments, runs post-call hooks, and
// caps the output.
//
// The approval gate and the post-call hooks both see the model's original
// arguments, not the ref-expanded ones — a human approving a call should read
// the handle the model chose, not the payload behind it.
func (d *Dispatcher) Dispatch(ctx context.Context, toolName string, args json.RawMessage) (CallResult, error) {
	fn, ok := d.handlers[toolName]
	if !ok {
		return CallResult{}, fmt.Errorf("unknown tool: %s", toolName)
	}

	if d.approve != nil && d.gated[toolName] {
		if err := d.approve(ctx, toolName, args); err != nil {
			return CallResult{}, err
		}
	}

	callArgs, err := d.expandRefs(ctx, toolName, args)
	if err != nil {
		return CallResult{}, err
	}

	output, err := fn(ctx, callArgs)
	if err != nil {
		return CallResult{}, err
	}

	for _, h := range d.hooks[toolName] {
		h(toolName, args, output)
	}

	return d.capOrSpill(ctx, toolName, output), nil
}
