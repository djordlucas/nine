// Package agent contains the tool dispatcher and agent loop.
package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"nine/internal/plugin"
)

// maxOutputTokens is the per-result token cap. Results exceeding this are
// truncated before being appended to the scratchpad.
const maxOutputTokens = 2048

// Hook is called after a tool call succeeds.
type Hook func(toolName string, args json.RawMessage, output string)

// CallResult is the output of a dispatched tool call.
type CallResult struct {
	Output    string
	Truncated bool // true when the output was capped at maxOutputTokens
}

// ApprovalFn is consulted before a gated tool runs. Returning a non-nil error
// blocks the call; the error is surfaced to the model as a normal tool failure.
type ApprovalFn func(ctx context.Context, toolName string, args json.RawMessage) error

// Dispatcher routes tool calls to registered plugins or to core-intercepted
// handlers, applies post-call hooks, and enforces the output token cap.
type Dispatcher struct {
	handlers map[string]func(context.Context, json.RawMessage) (string, error)
	hooks    map[string][]Hook
	gated    map[string]bool
	approve  ApprovalFn
}

// New returns an empty Dispatcher. Register handlers with the Register*
// functions before dispatching.
func New() *Dispatcher {
	return &Dispatcher{
		handlers: make(map[string]func(context.Context, json.RawMessage) (string, error)),
		hooks:    make(map[string][]Hook),
	}
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

// Dispatch routes the call, runs post-call hooks, and caps the output.
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

	output, err := fn(ctx, args)
	if err != nil {
		return CallResult{}, err
	}

	for _, h := range d.hooks[toolName] {
		h(toolName, args, output)
	}

	return capOutput(output), nil
}

func capOutput(output string) CallResult {
	maxChars := maxOutputTokens * 4
	if len(output) <= maxChars {
		return CallResult{Output: output}
	}
	return CallResult{
		Output:    output[:maxChars] + "\n[output truncated]",
		Truncated: true,
	}
}
