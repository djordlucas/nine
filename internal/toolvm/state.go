package toolvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tetratelabs/wazero/api"
)

// StateQuota is the resolved bound on one (tool, scope) namespace, handed to the
// store with every write. It mirrors StateGrant minus the scope, which the host
// has already turned into a scope key by the time the store sees it.
type StateQuota struct {
	MaxKeys    int
	MaxValueKB int
	MaxTotalKB int
	TTL        time.Duration
}

// StateStore is the durable store behind the `state` capability.
//
// It is an interface rather than a concrete store for the same reason
// Config.TouchGenerated is a function: this package has no database and should
// not grow one. The daemon implements it over the memory store; a test
// implements it over a map.
//
// A QuotaError from any method is handed to the guest as a catchable error
// rather than failing the call — see errIsQuota.
type StateStore interface {
	StateGet(tool, scope, key string) (string, bool, error)
	StateSet(tool, scope, key, value string, q StateQuota) error
	StateDelete(tool, scope, key string) error
	StateList(tool, scope, prefix string) ([]string, error)
	StateSwap(tool, scope, key string, expected *string, value string, q StateQuota) (bool, error)
}

// QuotaError marks a refusal the guest is expected to catch and handle — it has
// filled its store and should evict something. It is deliberately distinct from
// a host failure: a tool that hit its key budget is working correctly and can
// recover, where a store that cannot be reached is not the tool's problem.
type QuotaError struct{ Reason string }

func (e *QuotaError) Error() string { return e.Reason }

// stateGrantKey carries the calling tool's state grant into the host function,
// per call, for the reason httpGrantKey exists: the import is shared by every
// `js` tool because a wasm module's imports are fixed at compile time, and the
// permission must not be.
type stateGrantKey struct{}

// stateScopeKey carries the owning conversation id, for a conversation-scoped
// grant.
type stateScopeKey struct{}

// WithStateScope returns a context whose conversation-scoped state resolves to
// id. Whoever is running the turn installs it, exactly as WithHTTPAudit is
// installed and for the same reason: a Host is daemon-wide and built once at
// boot, and the conversation a call belongs to is a property of the turn.
//
// A conversation-scoped grant with no id on the context is **refused**, not
// quietly served from the shared namespace — see hostState.
func WithStateScope(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, stateScopeKey{}, id)
}

// stateRequest is the guest's op. One envelope for five operations, because the
// ABI crossing is a JSON byte slice and a second shared buffer per verb would
// buy nothing.
type stateRequest struct {
	Op     string `json:"op"`
	Key    string `json:"key"`
	Value  string `json:"value"`
	Prefix string `json:"prefix"`
	// Expected drives swap. Absent or null means "only if the key does not
	// exist"; a string means "only if the current value is exactly this".
	Expected *string `json:"expected"`
}

type stateResponse struct {
	Value string   `json:"value,omitempty"`
	Found bool     `json:"found,omitempty"`
	Keys  []string `json:"keys,omitempty"`
	OK    bool     `json:"ok,omitempty"`
	Error string   `json:"error,omitempty"`
	// Quota marks Error as a quota refusal, so the guest harness can throw a
	// distinguishable error rather than a generic one.
	Quota bool `json:"quota,omitempty"`
}

// hostState is the guest's `nine.state`.
//
// Like hostHTTP it is exported unconditionally — the QuickJS blob is shared by
// every `js` tool and a module's imports are fixed at compile time, so the
// function must exist for the module to instantiate at all. The grant is read
// per call from the context below and a tool without one is refused before the
// request is parsed. What is shared is the import, not the permission.
func (h *Host) hostState(ctx context.Context, mod api.Module, ptr, size uint32) uint64 {
	name, _ := ctx.Value(toolNameKey{}).(string)
	grant, _ := ctx.Value(stateGrantKey{}).(*StateGrant)

	if grant == nil {
		return writeState(ctx, mod, stateResponse{
			Error: "blocked: this tool was not granted the state capability"})
	}
	if h.cfg.StateStore == nil {
		return writeState(ctx, mod, stateResponse{
			Error: "the state capability is granted but this daemon has no state store configured"})
	}

	scope, err := stateScopeFor(ctx, *grant)
	if err != nil {
		return writeState(ctx, mod, stateResponse{Error: err.Error()})
	}

	raw, ok := mod.Memory().Read(ptr, size)
	if !ok {
		return writeState(ctx, mod, stateResponse{Error: "unreadable state request"})
	}
	// Copy before doing anything slow: the read above aliases guest memory.
	buf := make([]byte, len(raw))
	copy(buf, raw)

	return writeState(ctx, mod, h.doState(name, scope, *grant, buf))
}

// stateScopeFor resolves the scope key from the grant and the context.
//
// A conversation-scoped grant with no conversation on the context is an error,
// not a fallback to the shared namespace. Falling back would silently hand a
// tool the very cross-conversation visibility the operator chose "conversation"
// to deny, and it would do so exactly in the paths that lack a turn — which is
// where nobody is watching.
func stateScopeFor(ctx context.Context, g StateGrant) (string, error) {
	switch g.Scope {
	case StateScopeTool:
		return "", nil
	case StateScopeConversation:
		id, _ := ctx.Value(stateScopeKey{}).(string)
		if id == "" {
			return "", fmt.Errorf(
				"state is granted at conversation scope but this call has no conversation " +
					"(a direct invocation, not an agent turn); call it through a session, " +
					"or grant scope = \"tool\" if the store is meant to be shared")
		}
		return id, nil
	default:
		// Unreachable when config validated the grant; a named failure beats
		// serving an unknown scope out of the shared namespace.
		return "", fmt.Errorf("state scope %q is not recognized", g.Scope)
	}
}

func (h *Host) doState(tool, scope string, g StateGrant, raw []byte) stateResponse {
	var req stateRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return stateResponse{Error: "malformed state request: " + err.Error()}
	}

	d := g.withDefaults()
	q := StateQuota{
		MaxKeys:    d.MaxKeys,
		MaxValueKB: d.MaxValueKB,
		MaxTotalKB: d.MaxTotalKB,
		TTL:        d.TTL,
	}
	st := h.cfg.StateStore

	switch req.Op {
	case "get":
		if req.Key == "" {
			return stateResponse{Error: "state get: key is required"}
		}
		v, found, err := st.StateGet(tool, scope, req.Key)
		if err != nil {
			return stateErr(err)
		}
		return stateResponse{Value: v, Found: found}

	case "set":
		if req.Key == "" {
			return stateResponse{Error: "state set: key is required"}
		}
		if err := st.StateSet(tool, scope, req.Key, req.Value, q); err != nil {
			return stateErr(err)
		}
		return stateResponse{OK: true}

	case "delete":
		if req.Key == "" {
			return stateResponse{Error: "state delete: key is required"}
		}
		if err := st.StateDelete(tool, scope, req.Key); err != nil {
			return stateErr(err)
		}
		return stateResponse{OK: true}

	case "list":
		keys, err := st.StateList(tool, scope, req.Prefix)
		if err != nil {
			return stateErr(err)
		}
		// An explicit empty slice, so the guest sees [] rather than null.
		if keys == nil {
			keys = []string{}
		}
		return stateResponse{Keys: keys}

	case "swap":
		if req.Key == "" {
			return stateResponse{Error: "state swap: key is required"}
		}
		took, err := st.StateSwap(tool, scope, req.Key, req.Expected, req.Value, q)
		if err != nil {
			return stateErr(err)
		}
		return stateResponse{OK: took}

	default:
		return stateResponse{Error: fmt.Sprintf(
			"state: unknown op %q (want get, set, delete, list or swap)", req.Op)}
	}
}

// stateErr renders a store error, marking a quota refusal so the guest harness
// can throw something the tool can tell apart from the store being broken.
func stateErr(err error) stateResponse {
	var qe *QuotaError
	if errors.As(err, &qe) {
		return stateResponse{Error: qe.Reason, Quota: true}
	}
	return stateResponse{Error: err.Error()}
}

func writeState(ctx context.Context, mod api.Module, resp stateResponse) uint64 {
	out, err := json.Marshal(resp)
	if err != nil {
		out = []byte(`{"error":"could not encode state response"}`)
	}
	return writeGuestBytes(ctx, mod, out)
}
