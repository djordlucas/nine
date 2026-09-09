// Package agent contains the tool dispatcher and agent loop.
package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"nine/internal/plugin"
	"nine/internal/toolvm"
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
	// Backend labels which backend ran the tool ("builtin", "plugin", "tool").
	Backend string
}

// ApprovalFn is consulted before a gated tool runs. Returning a non-nil error
// blocks the call; the error is surfaced to the model as a normal tool failure.
type ApprovalFn func(ctx context.Context, toolName string, args json.RawMessage) error

// ApprovalError wraps a gated call a human declined, or one whose approval
// could not be obtained (the question timed out or the turn was cancelled).
//
// It exists so the retry loop can tell a *decision* from a transient failure:
// re-dispatching a rejected call just asks the same person the same question
// again, and with parallel sub-agent gates (R-HITL.5) that multiplies into a
// barrage. dispatchWithRetry treats it as terminal. The wrapped error carries
// the message the model sees, unchanged.
type ApprovalError struct{ Err error }

func (e *ApprovalError) Error() string { return e.Err.Error() }
func (e *ApprovalError) Unwrap() error { return e.Err }

// Dispatcher routes tool calls to registered plugins or to core-intercepted
// handlers, expands ref arguments, applies post-call hooks, and enforces the
// output token cap.
type Dispatcher struct {
	handlers map[string]func(context.Context, json.RawMessage) (string, error)
	backends map[string]string
	hooks    map[string][]Hook
	gated    map[string]bool
	approve  ApprovalFn

	// Large-output plumbing (adr/tool-output-spill.md): spill writes over-cap
	// results out to the file store, resolveRef reads them back into the
	// arguments of tools that declare a ref parameter, and refParams indexes
	// which arguments those are.
	spill           SpillFn
	resolveRef      RefResolver
	refParams       map[string][]string
	maxOutputTokens int

	// jobs records long-running plugin jobs (docs/plugin-capabilities.md §5), set
	// per worker via SetJobStarter. Nil when jobs are not enabled.
	jobs JobStarter
}

// New returns an empty Dispatcher. Register handlers with the Register*
// functions before dispatching.
func New() *Dispatcher {
	d := &Dispatcher{
		handlers:        make(map[string]func(context.Context, json.RawMessage) (string, error)),
		backends:        make(map[string]string),
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

// JobStarter records a long-running plugin job a tool call returned instead of a
// result (docs/plugin-capabilities.md §5) and returns the observation the model
// sees — a short line naming the job's stable handle. It is set per conversation
// (the owner) so a completion can be notified back; without one, a plugin that
// returns a job id is an error.
type JobStarter interface {
	StartJob(ctx context.Context, pluginName, tool, pluginJobID, ack string) (string, error)

	// StartToolJob records a long-running sandboxed-tool job and returns the
	// observation the model sees. Unlike a plugin job the work has not begun —
	// the daemon decides when to make the first call — so this can be refused
	// outright rather than admitted and cancelled.
	StartToolJob(ctx context.Context, tool string, args json.RawMessage, c *toolvm.Continuation) (string, error)
}

// SetJobStarter installs the sink for plugin jobs. Registered per worker so the
// job is keyed to the conversation that started it.
func (d *Dispatcher) SetJobStarter(js JobStarter) { d.jobs = js }

// RegisterPlugin indexes all tools advertised by p so they can be dispatched.
func (d *Dispatcher) RegisterPlugin(m *plugin.Manager, p *plugin.Plugin) {
	for _, t := range p.Tools {
		toolName := t.Name
		pp := p
		d.declareRefParams(toolName, t.InputSchema)
		d.backends[toolName] = "plugin"
		d.handlers[toolName] = func(ctx context.Context, args json.RawMessage) (string, error) {
			cr, err := m.Call(ctx, pp, toolName, args)
			if err != nil {
				return "", err
			}
			return d.resolveCallResult(ctx, pp, toolName, cr)
		}
	}
}

// SandboxedHost is the sandboxed-tool backend, as the dispatcher needs it
// (spec/contracts/toolvm.md). Narrowed to these two methods so the agent package
// does not depend on the wasm runtime — and so a test can substitute a fake
// without one.
type SandboxedHost interface {
	Tools() []*toolvm.Tool
	CallOutput(ctx context.Context, name string, args json.RawMessage) (toolvm.Output, error)
}

// RegisterSandboxed indexes every tool the sandboxed host holds, so it can be
// dispatched exactly like a plugin tool. A nil host registers nothing — callers
// pass one only when [tools] enabled is set, which is what keeps the whole
// subsystem additive.
//
// Unlike a plugin, a sandboxed tool's schema is authoritative from its manifest
// (there is no process to ask plugin.describe), but it reaches the ref machinery
// through the same declaration path, so a sandboxed tool can take a ref argument
// on the same terms.
func (d *Dispatcher) RegisterSandboxed(h SandboxedHost) {
	if h == nil {
		return
	}
	for _, t := range h.Tools() {
		toolName := t.Name
		d.declareRefParams(toolName, t.InputSchema)
		d.backends[toolName] = "wasm"
		d.handlers[toolName] = func(ctx context.Context, args json.RawMessage) (string, error) {
			out, err := h.CallOutput(ctx, toolName, args)
			if err != nil {
				return "", err
			}
			if out.Continue != nil {
				// The tool did bounded work and asked to be called again, so this
				// turn gets an acknowledgement and the job runs on past it. Same
				// shape as a plugin returning a job id, and the model sees the same
				// vocabulary: a handle it can job_wait or job_check.
				if d.jobs == nil {
					return "", fmt.Errorf(
						"tool %q asked to run as a background job, but background jobs are not enabled here", toolName)
				}
				return d.jobs.StartToolJob(ctx, toolName, args, out.Continue)
			}
			if out.Bytes != nil {
				return d.storeToolBytes(ctx, toolName, out)
			}
			return out.Text, nil
		}
	}
}

// resolveCallResult turns a plugin CallResult into the string the model sees. An
// ordinary result passes through; a job id is recorded via the JobStarter and
// replaced with its handle observation. A job id from a plugin that did not
// advertise async_jobs is rejected (fail-closed against version skew), as is one
// when no JobStarter is wired.
func (d *Dispatcher) resolveCallResult(ctx context.Context, p *plugin.Plugin, tool string, cr plugin.CallResult) (string, error) {
	if cr.JobID == "" {
		return cr.Output, nil
	}
	if !p.AsyncJobs {
		return "", fmt.Errorf("plugin %q returned a job id but did not advertise async_jobs", p.Name)
	}
	if d.jobs == nil {
		return "", fmt.Errorf("plugin %q started a job but background jobs are not enabled here", p.Name)
	}
	return d.jobs.StartJob(ctx, p.Name, tool, cr.JobID, cr.Output)
}

// InjectHandler registers a handler function directly by tool name.
// Intended for testing; overwrites any existing handler.
func (d *Dispatcher) InjectHandler(toolName string, fn func(context.Context, json.RawMessage) (string, error)) {
	d.handlers[toolName] = fn
	d.backends[toolName] = "builtin"
}

// InjectHandlerWithSchema is InjectHandler for a tool whose input schema
// matters — in practice, one declaring an x-nine-ref parameter (refs.go).
// Intended for testing; production tools get their schemas from RegisterPlugin
// (plugins) or InterceptedDefs (core).
func (d *Dispatcher) InjectHandlerWithSchema(toolName string, schema json.RawMessage, fn func(context.Context, json.RawMessage) (string, error)) {
	d.declareRefParams(toolName, schema)
	d.handlers[toolName] = fn
	d.backends[toolName] = "builtin"
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

// Has reports whether toolName has a registered handler — i.e. whether
// Dispatch would route it rather than fail with "unknown tool". Callers that
// hold more than one dispatch surface (the daemon holds this one plus the
// plugin manager) use it to pick the right one before dispatching.
func (d *Dispatcher) Has(toolName string) bool {
	_, ok := d.handlers[toolName]
	return ok
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

	// Normalize nil/empty args to {} so every handler can json.Unmarshal
	// unconditionally. The LLM providers sanitize invalid tool-call input to
	// nil to keep the journal marshal-safe; without this, a handler that
	// receives nil gets "unexpected end of JSON input" instead of a clean
	// zero-value struct it can validate.
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
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

// Backend returns the backend label for toolName. Tools registered directly
// via d.handlers (core builtins) default to "builtin" when not explicitly
// labeled by RegisterPlugin/RegisterSandboxed/InjectHandler.
func (d *Dispatcher) Backend(toolName string) string {
	if b, ok := d.backends[toolName]; ok {
		return b
	}
	return "builtin"
}
