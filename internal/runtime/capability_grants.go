package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"nine/internal/agent"
	"nine/internal/config"
	"nine/internal/memory"
	"nine/internal/protocol"
	"nine/internal/toolvm"
)

// The generated tier's ceiling lives in the store, and `nine.toml` is reconciled
// into it at every boot.
//
// The store is the source of truth for what is *in force*; the file declares the
// operator's baseline. Every grant row carries a source: `default` is derived by
// the daemon from [workspace].root, `config` comes from
// [tools.agent.capabilities], and `approved` was conferred by an operator
// answering a capability request. The first two are rewritten on every boot and
// the third is durable, so the file behaves as both a first-boot seed and a change
// feed with no sentinel, no snapshot and no three-way merge — and a grant the file
// stops declaring stops applying.
//
// The invariant this preserves is the one that matters: *the agent writes the
// code, the operator writes the grants, never the same actor.* An approval is an
// operator action, and no agent-facing tool can reach either table. What changed
// is where a grant lives, not who writes it.

// grantParams is the JSON payload on a capability_grants row. One shape serves
// every capability so that a row is self-describing and a new capability needs no
// schema change — the fields a given capability uses are the ones it sets.
type grantParams struct {
	Mounts     []toolvm.Mount `json:"mounts,omitempty"`      // fs.read, fs.write
	Env        []string       `json:"env,omitempty"`         // env
	AllowHosts []string       `json:"allow_hosts,omitempty"` // net.http
	Methods    []string       `json:"methods,omitempty"`     // net.http
	MaxBytes   int64          `json:"max_bytes,omitempty"`   // net.http
	Scope      string         `json:"scope,omitempty"`       // state
	MaxKeys    int            `json:"max_keys,omitempty"`    // state
	MaxValueKB int            `json:"max_value_kb,omitempty"`
	MaxTotalKB int            `json:"max_total_kb,omitempty"`
	TTL        string         `json:"ttl,omitempty"`
}

// DerivedCapabilityGrants is the ceiling `nine.toml` and the workspace imply: the
// `config` and `default` rows a boot writes.
//
// The fs fallback is the same one agentConfig applied before grants were stored:
// with no operator fs grant, the ceiling is [workspace].root read and write, at
// the guest path the shipped tools use. An explicit grant replaces it rather than
// adding to it, which is how an operator narrows the ceiling.
func DerivedCapabilityGrants(cfg *config.Config) []memory.CapabilityGrant {
	caps := cfg.Tools.Agent.Capabilities
	var out []memory.CapabilityGrant

	add := func(source, capability string, p grantParams) {
		b, err := json.Marshal(p)
		if err != nil {
			// grantParams is plain data; a marshal failure is not reachable.
			slog.Error("capability grant params could not be encoded", "capability", capability, "err", err)
			return
		}
		out = append(out, memory.CapabilityGrant{
			ID:         memory.NewID(),
			Source:     source,
			Capability: capability,
			Params:     string(b),
		})
	}

	fsRead, fsWrite := mounts(caps.FS.Read), mounts(caps.FS.Write)
	if len(fsRead) == 0 && len(fsWrite) == 0 {
		if root := cfg.WorkspaceRoot(); root != "" {
			m := []toolvm.Mount{{Host: root, Guest: toolvm.ShippedWorkspaceGuest}}
			add(memory.GrantSourceDefault, toolvm.CapFSRead, grantParams{Mounts: m})
			add(memory.GrantSourceDefault, toolvm.CapFSWrite, grantParams{Mounts: m})
		}
	} else {
		if len(fsRead) > 0 {
			add(memory.GrantSourceConfig, toolvm.CapFSRead, grantParams{Mounts: fsRead})
		}
		if len(fsWrite) > 0 {
			add(memory.GrantSourceConfig, toolvm.CapFSWrite, grantParams{Mounts: fsWrite})
		}
	}

	if len(caps.Env) > 0 {
		add(memory.GrantSourceConfig, toolvm.CapEnvRead, grantParams{Env: caps.Env})
	}
	if h := httpGrant(caps.Net.HTTP); h != nil {
		add(memory.GrantSourceConfig, toolvm.CapNetHTTP, grantParams{
			AllowHosts: h.AllowHosts, Methods: h.Methods, MaxBytes: h.MaxBytes,
		})
	}
	if st := stateGrant(caps.State); st != nil {
		p := grantParams{
			Scope: st.Scope, MaxKeys: st.MaxKeys,
			MaxValueKB: st.MaxValueKB, MaxTotalKB: st.MaxTotalKB,
		}
		if st.TTL > 0 {
			p.TTL = st.TTL.String()
		}
		add(memory.GrantSourceConfig, toolvm.CapState, p)
	}

	return out
}

// CeilingFromGrants unions grant rows into the ceiling the host enforces.
//
// A capability granted twice — once by the file, once by an approval — contributes
// both times: mounts and env keys accumulate, and for net.http the allowed hosts
// and methods do. That is a union rather than a precedence rule because a grant
// row is an operator's statement that this reach is permitted, and two such
// statements do not cancel.
func CeilingFromGrants(grants []memory.CapabilityGrant) toolvm.Ceiling {
	var g toolvm.Grant
	for _, row := range grants {
		var p grantParams
		if row.Params != "" {
			if err := json.Unmarshal([]byte(row.Params), &p); err != nil {
				// A row nobody can parse is a row that grants nothing. Skipping it
				// fails closed, which is the only safe direction for a ceiling.
				slog.Error("capability grant ignored: unreadable params",
					"id", row.ID, "capability", row.Capability, "err", err)
				continue
			}
		}
		switch row.Capability {
		case toolvm.CapFSRead:
			g.FSRead = append(g.FSRead, p.Mounts...)
		case toolvm.CapFSWrite:
			g.FSWrite = append(g.FSWrite, p.Mounts...)
		case toolvm.CapEnvRead:
			g.Env = append(g.Env, p.Env...)
		case toolvm.CapNetHTTP:
			if g.HTTP == nil {
				g.HTTP = &toolvm.HTTPGrant{MaxBytes: p.MaxBytes}
			}
			g.HTTP.AllowHosts = append(g.HTTP.AllowHosts, p.AllowHosts...)
			g.HTTP.Methods = append(g.HTTP.Methods, p.Methods...)
			if p.MaxBytes > g.HTTP.MaxBytes {
				g.HTTP.MaxBytes = p.MaxBytes
			}
		case toolvm.CapState:
			if g.State == nil {
				g.State = &toolvm.StateGrant{
					Scope: p.Scope, MaxKeys: p.MaxKeys,
					MaxValueKB: p.MaxValueKB, MaxTotalKB: p.MaxTotalKB,
				}
				if p.TTL != "" {
					if d, err := time.ParseDuration(p.TTL); err == nil {
						g.State.TTL = d
					}
				}
			}
		default:
			slog.Warn("capability grant names an unknown capability",
				"id", row.ID, "capability", row.Capability)
		}
	}
	return toolvm.Ceiling{Grant: g}
}

// ReconcileCapabilityGrants writes the ceiling `nine.toml` and the workspace imply
// into the store, replacing the previous derived rows and leaving approved ones
// alone. It returns every grant now in force.
//
// Called at boot, before the host's agent policy is installed, so that one code
// path reads the ceiling whether it came from the file or from an approval.
func ReconcileCapabilityGrants(store *memory.Store, cfg *config.Config) ([]memory.CapabilityGrant, error) {
	if store == nil {
		return nil, nil
	}
	if err := store.CapabilityGrantsReconcile(DerivedCapabilityGrants(cfg)); err != nil {
		return nil, fmt.Errorf("reconcile capability grants: %w", err)
	}
	return store.CapabilityGrantList()
}

// ApplyCapabilityGrants installs the store's ceiling on a running host and
// re-projects the generated catalog against it, with no restart.
//
// Both halves are needed and in this order. SetAgentConfig replaces the policy;
// LoadGeneratedTools re-resolves every stored tool's declaration against the new
// ceiling, which is what registers a tool that previously skipped for want of a
// capability — and unregisters one that no longer fits after a revocation. The
// loop's per-turn catalog re-sync then puts the change in front of the model on
// its next turn.
func ApplyCapabilityGrants(ctx context.Context, store *memory.Store, cfg *config.Config, host *toolvm.Host, mgr toolOwner) error {
	if host == nil || store == nil {
		return nil
	}
	grants, err := store.CapabilityGrantList()
	if err != nil {
		return fmt.Errorf("read capability grants: %w", err)
	}
	ac := agentConfig(cfg)
	ac.Ceiling = CeilingFromGrants(grants)
	host.SetAgentConfig(ac)
	LoadGeneratedTools(ctx, store, host, mgr)
	return nil
}

// NewCapabilityRequester builds the capability_request handler factory: one
// requester per asking agent, backed by store, surfacing each request to the
// operator's notification feed.
//
// The returned closure can insert a pending row and nothing else. It has no path
// to a grant, which is the property that lets the tool be advertised to the model
// at all.
func NewCapabilityRequester(store *memory.Store, notify func(agentID, text string)) func(string) agent.CapabilityRequester {
	if store == nil {
		return nil
	}
	return func(agentID string) agent.CapabilityRequester {
		return func(_ context.Context, capability, reason, toolName string, params json.RawMessage) (string, error) {
			if !slices.Contains(grantableCapabilities, capability) {
				return "", fmt.Errorf("capability %q is not one Nine can grant; it must be one of %v",
					capability, grantableCapabilities)
			}
			// Validated here so the store never holds a request an operator could
			// approve into a ceiling nine.toml could not express (validateGrant).
			if err := validateGrantParams(capability, params); err != nil {
				return "", err
			}

			id := memory.NewID()
			if err := store.CapabilityRequestCreate(id, agentID, toolName, capability, string(params), reason); err != nil {
				return "", fmt.Errorf("record capability request: %w", err)
			}
			if notify != nil {
				what := capability
				if toolName != "" {
					what = capability + " for " + toolName
				}
				notify(agentID, "capability requested: "+what+" — "+reason+
					"\nreview with `nine grants list`, then `nine grants approve "+id+"`")
			}
			slog.Info("capability requested", "id", id, "agent_id", agentID,
				"capability", capability, "tool", toolName)

			return "requested " + capability + " (id " + id + "). " +
				"An operator decides; nothing is granted yet. If it is approved the capability " +
				"becomes available without a restart, so retry the write then. Continue with " +
				"whatever the task allows without it.", nil
		}
	}
}

// grantableCapabilities are the capabilities a request may name — the same set
// toolvm resolves against a ceiling. `clock`, `random` and `log` are absent
// because they are granted unconditionally and have no knob to widen.
var grantableCapabilities = []string{
	toolvm.CapFSRead, toolvm.CapFSWrite, toolvm.CapNetHTTP, toolvm.CapEnvRead, toolvm.CapState,
}

// validateGrantParams holds a requested scope to the same rules nine.toml is held
// to, so that approving a request can never produce a ceiling the file could not
// express — an empty allow_hosts, a relative mount, a reserved env key.
//
// It runs at request time rather than approval time so the refusal reaches the
// model, which can act on it, instead of an operator, who cannot.
func validateGrantParams(capability string, params json.RawMessage) error {
	if len(params) == 0 {
		// A request may name a capability without a scope; the operator then decides
		// the scope. The file's validators run on what they approve.
		return nil
	}
	var p grantParams
	if err := json.Unmarshal(params, &p); err != nil {
		return fmt.Errorf("capability params are not readable: %w", err)
	}
	return config.ValidateCapabilityGrant(capability, config.CapabilityGrantParams{
		Mounts:     configMounts(p.Mounts),
		Env:        p.Env,
		AllowHosts: p.AllowHosts,
		Methods:    p.Methods,
		MaxBytes:   int(p.MaxBytes),
		Scope:      p.Scope,
		MaxKeys:    p.MaxKeys,
		MaxValueKB: p.MaxValueKB,
		MaxTotalKB: p.MaxTotalKB,
		TTL:        p.TTL,
	})
}

func configMounts(in []toolvm.Mount) []config.ToolMount {
	if len(in) == 0 {
		return nil
	}
	out := make([]config.ToolMount, len(in))
	for i, m := range in {
		out[i] = config.ToolMount{Host: m.Host, Guest: m.Guest}
	}
	return out
}

// CapabilityService is the daemon's capability surface, shared by the CLI, the TUI
// and the API so that one decision path serves all three.
//
// It holds the config and the host because a decision is not only a store write: an
// approval widens the live ceiling and a revocation narrows it, and both re-project
// the generated catalog so the change reaches the model on its next turn.
type CapabilityService struct {
	store *memory.Store
	cfg   *config.Config
	host  *toolvm.Host
	mgr   toolOwner
}

// NewCapabilityService wires the capability surface. A nil store yields nil: a
// daemon with no store has no ceiling to report or change.
func NewCapabilityService(store *memory.Store, cfg *config.Config, host *toolvm.Host, mgr toolOwner) *CapabilityService {
	if store == nil || cfg == nil {
		return nil
	}
	return &CapabilityService{store: store, cfg: cfg, host: host, mgr: mgr}
}

// State reports every grant in force and every request the agent has made.
func (s *CapabilityService) State() (protocol.CapabilityState, error) {
	grants, err := s.store.CapabilityGrantList()
	if err != nil {
		return protocol.CapabilityState{}, fmt.Errorf("read capability grants: %w", err)
	}
	requests, err := s.store.CapabilityRequestList(false)
	if err != nil {
		return protocol.CapabilityState{}, fmt.Errorf("read capability requests: %w", err)
	}

	out := protocol.CapabilityState{
		Grants:   make([]protocol.CapabilityGrantInfo, 0, len(grants)),
		Requests: make([]protocol.CapabilityRequestInfo, 0, len(requests)),
	}
	for _, g := range grants {
		out.Grants = append(out.Grants, protocol.CapabilityGrantInfo{
			ID: g.ID, Source: g.Source, Capability: g.Capability,
			Params: g.Params, RequestID: g.RequestID, CreatedAt: g.CreatedAt,
		})
	}
	for _, r := range requests {
		out.Requests = append(out.Requests, protocol.CapabilityRequestInfo{
			ID: r.ID, AgentID: r.AgentID, ToolName: r.ToolName,
			Capability: r.Capability, Params: r.Params, Reason: r.Reason,
			Status: r.Status, CreatedAt: r.CreatedAt, DecidedAt: r.DecidedAt,
		})
	}
	return out, nil
}

// Decide settles a request ("approve"/"deny") or revokes a grant ("revoke"), and
// applies the result to the running host. It returns a sentence describing what
// happened, which every front end shows verbatim.
func (s *CapabilityService) Decide(ctx context.Context, id, action string) (string, error) {
	switch action {
	case "approve":
		ok, err := s.store.CapabilityRequestDecide(id, memory.CapabilityRequestApproved, memory.NewID())
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("no pending capability request %q — it may already be decided", id)
		}
		if err := s.apply(ctx); err != nil {
			// The grant is recorded, so a restart would pick it up. Say so rather
			// than reporting a failure that looks like the approval not landing.
			return "", fmt.Errorf("approved, but the running daemon did not take it up (a restart will): %w", err)
		}
		return "approved " + id + "; the capability is in force now, no restart needed", nil

	case "deny":
		ok, err := s.store.CapabilityRequestDecide(id, memory.CapabilityRequestDenied, "")
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("no pending capability request %q — it may already be decided", id)
		}
		return "denied " + id + "; nothing was granted", nil

	case "revoke":
		ok, err := s.store.CapabilityGrantRevoke(id)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("no revocable grant %q; only an approved grant can be revoked here, "+
				"because a config or default grant is rewritten from nine.toml at every boot", id)
		}
		if err := s.apply(ctx); err != nil {
			return "", fmt.Errorf("revoked, but the running daemon did not take it up (a restart will): %w", err)
		}
		return "revoked " + id + "; any generated tool that needed it stops loading", nil

	default:
		return "", fmt.Errorf("capability action %q is not \"approve\", \"deny\" or \"revoke\"", action)
	}
}

// PendingCount is how many requests await a decision. Used for the TUI's badge and
// the API's summary, where a full listing would be more than the caller wants.
func (s *CapabilityService) PendingCount() (int, error) {
	pending, err := s.store.CapabilityRequestList(true)
	return len(pending), err
}

func (s *CapabilityService) apply(ctx context.Context) error {
	return ApplyCapabilityGrants(ctx, s.store, s.cfg, s.host, s.mgr)
}
