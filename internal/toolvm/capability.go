package toolvm

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// Capability names, as they appear in a manifest's [capabilities] table and in
// the errors an operator reads. These are the only capabilities with reach;
// clock, random, and log are granted unconditionally and so are not named here
// (docs/sandboxed-tools.md §6.2).
const (
	CapFSRead   = "fs.read"
	CapFSWrite  = "fs.write"
	CapNetHTTP  = "net.http"
	CapEnvRead  = "env"
	CapState    = "state"
	capFSVerbRd = "read"
	capFSVerbWr = "write"
)

// Declaration is what a tool's manifest says it needs. It is documentation and
// a pre-flight check, never a security control: nothing reads it at call time,
// so a manifest that lies gains nothing (docs/sandboxed-tools.md §6.3).
// The json tags matter for the generated tier: a developer tool's declaration
// arrives as TOML from a manifest, but a generated tool's arrives as the JSON the
// agent handed tool_write (docs/sandboxed-tools.md §5.2), so the same struct has
// to decode from both.
type Declaration struct {
	// FS lists filesystem verbs: "read", "write".
	FS []string `toml:"fs" json:"fs"`
	// Net lists network verbs: "http".
	Net []string `toml:"net" json:"net"`
	// State declares the `state` capability: a durable, host-owned store scoped
	// to this tool. A bool rather than a list because the declaration contributes
	// no parameters — scope and quotas are the operator's, conferred in the
	// grant (R-TVM.6). A manifest saying `state = true` says "this tool
	// remembers"; it does not get to say what it may remember or for how long.
	State bool `toml:"state" json:"state"`
	// Env lists the environment keys the tool reads, by name. Keys rather than a
	// verb, because for env the key *is* the parameter — and naming them in the
	// manifest is what lets an operator diff a declaration against a grant.
	Env []string `toml:"env" json:"env"`
}

// Mount is one host→guest filesystem mapping.
type Mount struct {
	// Tagged because a Mount is serialized into a stored capability grant's params
	// (runtime.grantParams), and an agent's capability_request names the same fields
	// in a tool schema. Untagged, the same mount would be written `{"Host":…}` by
	// the daemon and `{"host":…}` by the model — both readable, since Go matches
	// field names case-insensitively, but two shapes for one thing in a payload the
	// API hands to clients verbatim.
	Host  string `json:"host"`
	Guest string `json:"guest"`
}

// Grant is what the operator confers on one named tool, from `[tool.<name>]`.
// The zero value is the default and the shipped posture: no filesystem, no
// network, no environment.
type Grant struct {
	FSRead  []Mount
	FSWrite []Mount
	Env     []string
	// HTTP is the net.http grant, or nil when the operator conferred none. It is
	// the one capability with no wazero primitive behind it — every check that
	// makes it safe lives in ssrf.go and nethttp.go.
	HTTP *HTTPGrant
	// State is the durable-state grant, or nil when the operator conferred none.
	// Like HTTP it has no wazero primitive behind it: the store is Nine's, and
	// the isolation between two tools' namespaces is a primary key, not a
	// sandbox boundary.
	State *StateGrant
}

// State scope values. Which one an operator writes is the whole security
// argument for this capability, which is why it is required and has no default
// (adr/durable-and-long-running-tools.md §3.3).
const (
	// StateScopeTool shares one namespace across every caller of the tool. It is
	// what a cache wants — and it is a cross-session information channel that
	// needs no other capability, since a tool's arguments come from the model and
	// can carry anything in that session's context. Confer it on a tool whose
	// code you have read.
	StateScopeTool = "tool"
	// StateScopeConversation keys the namespace by the owning conversation, so
	// nothing a tool writes in one thread is readable from another.
	StateScopeConversation = "conversation"
)

// Quota defaults, applied to any field an operator left at zero.
const (
	DefaultStateMaxKeys    = 128
	DefaultStateMaxValueKB = 64
	DefaultStateMaxTotalKB = 1024
)

// StateGrant is what the operator confers under
// `[tool.<name>.capabilities.state]`: a key/value store the host owns, scoped as
// Scope says and bounded by the quotas.
//
// The store is why I-TVM.3 reads "no state survives a call *implicitly*" rather
// than "no state survives a call": the guest's globals, heap, and interpreter
// realm are still destroyed at return, and what persists does so through this
// grant, named by the tool and bounded here.
type StateGrant struct {
	// Scope is StateScopeTool or StateScopeConversation. Required; the config
	// validator refuses an empty or unknown value rather than picking one.
	Scope string
	// MaxKeys, MaxValueKB and MaxTotalKB bound one (tool, scope) namespace.
	// Zero means the default above.
	MaxKeys    int
	MaxValueKB int
	MaxTotalKB int
	// TTL is how long a written value lives. Zero means it never expires.
	TTL time.Duration
}

// withDefaults fills the quota fields an operator left unset. Scope is never
// defaulted — that is the point of it being required.
func (g StateGrant) withDefaults() StateGrant {
	if g.MaxKeys <= 0 {
		g.MaxKeys = DefaultStateMaxKeys
	}
	if g.MaxValueKB <= 0 {
		g.MaxValueKB = DefaultStateMaxValueKB
	}
	if g.MaxTotalKB <= 0 {
		g.MaxTotalKB = DefaultStateMaxTotalKB
	}
	return g
}

// capabilities returns the capability names a Grant actually confers, sorted.
func (g Grant) capabilities() []string {
	var out []string
	if len(g.FSRead) > 0 {
		out = append(out, CapFSRead)
	}
	if len(g.FSWrite) > 0 {
		out = append(out, CapFSWrite)
	}
	if len(g.Env) > 0 {
		out = append(out, CapEnvRead)
	}
	if g.HTTP != nil {
		out = append(out, CapNetHTTP)
	}
	if g.State != nil {
		out = append(out, CapState)
	}
	sort.Strings(out)
	return out
}

// capabilities returns the capability names a Declaration asks for, sorted. An
// unrecognized verb is reported so a typo in a manifest fails loudly instead of
// quietly asking for nothing.
func (d Declaration) capabilities() ([]string, error) {
	var out []string
	for _, v := range d.FS {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case capFSVerbRd:
			out = append(out, CapFSRead)
		case capFSVerbWr:
			out = append(out, CapFSWrite)
		default:
			return nil, fmt.Errorf("unknown fs capability %q (want %q or %q)", v, capFSVerbRd, capFSVerbWr)
		}
	}
	for _, v := range d.Net {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "http":
			out = append(out, CapNetHTTP)
		default:
			return nil, fmt.Errorf("unknown net capability %q (want %q)", v, "http")
		}
	}
	if len(d.Env) > 0 {
		out = append(out, CapEnvRead)
	}
	if d.State {
		out = append(out, CapState)
	}
	sort.Strings(out)
	return out, nil
}

// resolveGrant settles what a tool actually runs with.
//
// The rule is that the declaration and the grant must name the same
// capabilities, and a mismatch in either direction is a named load failure:
//
//   - Declared but not granted is the case docs/sandboxed-tools.md §6.3 spells
//     out. The tool refuses to load rather than starting up crippled, because
//     silent degradation means a tool that half-works in ways neither the
//     developer nor the operator predicted.
//   - Granted but not declared is refused on the same reasoning, read the other
//     way. Conferring reach on a tool that never asked for it is how an
//     over-broad grant survives review: the operator sees a `[tool.x]` table
//     they wrote months ago and the tool's manifest never mentions it. Forcing
//     the two documents to agree keeps the manifest an accurate description of
//     what the tool can do, which is the only reason to write one.
//
// What comes back is built from the *grant* and only the grant — the
// declaration contributes no parameters, just the requirement that it match.
// That is the invariant the whole design rests on: the author writes the code,
// the operator writes the grants, and these are never the same actor.
func resolveGrant(decl Declaration, g Grant) (Grant, error) {
	declared, err := decl.capabilities()
	if err != nil {
		return Grant{}, err
	}
	granted := g.capabilities()

	for _, c := range declared {
		if !slices.Contains(granted, c) {
			return Grant{}, fmt.Errorf(
				"capability %s is declared by the tool but not granted; add it under [tool.<name>.capabilities] or remove the declaration", c)
		}
	}
	for _, c := range granted {
		if !slices.Contains(declared, c) {
			return Grant{}, fmt.Errorf(
				"capability %s is granted but the tool does not declare it; add it to the manifest's [capabilities] or remove the grant", c)
		}
	}

	// env is the one capability whose parameters are named on both sides, so the
	// keys themselves have to agree — matching on "the tool wants some env" would
	// let a grant of the wrong keys pass review as if it were the right ones.
	if err := matchEnvKeys(decl.Env, g.Env); err != nil {
		return Grant{}, err
	}

	return g, nil
}

// matchEnvKeys requires the declared and granted key sets to be identical.
func matchEnvKeys(declared, granted []string) error {
	for _, k := range declared {
		if !slices.Contains(granted, k) {
			return fmt.Errorf("env key %q is declared by the tool but not granted", k)
		}
	}
	for _, k := range granted {
		if !slices.Contains(declared, k) {
			return fmt.Errorf("env key %q is granted but the tool does not declare it", k)
		}
	}
	return nil
}

// Summary renders a resolved grant for `nine tools show` and load logging. An
// empty grant renders as "none", which is the common case and should read as
// reassuring rather than as missing information.
func (g Grant) Summary() string {
	caps := g.capabilities()
	if len(caps) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(caps))
	for _, c := range caps {
		switch c {
		case CapFSRead:
			parts = append(parts, "fs.read "+mountList(g.FSRead))
		case CapFSWrite:
			parts = append(parts, "fs.write "+mountList(g.FSWrite))
		case CapEnvRead:
			parts = append(parts, "env "+strings.Join(g.Env, ","))
		case CapNetHTTP:
			parts = append(parts, "net.http "+strings.Join(g.HTTP.Methods, "/")+
				" "+strings.Join(g.HTTP.AllowHosts, ","))
		case CapState:
			parts = append(parts, "state "+g.State.summary())
		default:
			parts = append(parts, c)
		}
	}
	return strings.Join(parts, "; ")
}

// summary renders a state grant for `nine tools show`: the scope first, because
// it is the part an operator reviewing a grant most needs to see.
func (g StateGrant) summary() string {
	d := g.withDefaults()
	out := fmt.Sprintf("scope=%s keys<=%d value<=%dKB total<=%dKB",
		d.Scope, d.MaxKeys, d.MaxValueKB, d.MaxTotalKB)
	if d.TTL > 0 {
		out += " ttl=" + d.TTL.String()
	}
	return out
}

func mountList(ms []Mount) string {
	parts := make([]string, len(ms))
	for i, m := range ms {
		parts[i] = m.Host + "=>" + m.Guest
	}
	return strings.Join(parts, ",")
}

// Ceiling is the maximum a *generated* tool may be granted
// (`[tools.agent.capabilities]`). It is deliberately the same shape as a Grant,
// because it is expressed in the same terms — but it plays a different role, and
// conflating the two is the mistake this type exists to prevent.
type Ceiling struct{ Grant }

// resolveCeiling settles what a generated tool runs with.
//
// This is where the design's two most easily-confused sentences are made
// mechanical (docs/sandboxed-tools.md §7):
//
//   - **The ceiling is a maximum, not a default.** A tool does not receive
//     workspace read merely by existing. It must declare `fs = ["read"]` to get
//     it, and a tool that declares nothing runs with nothing regardless of how
//     permissive the ceiling is. Least privilege is per tool, not per tier.
//   - **A tool cannot request its way past it.** Declaring a capability the
//     ceiling excludes is a refusal, not a negotiation.
//
// Keeping the two separate is what stops widening the ceiling from retroactively
// widening every tool already in the catalog.
//
// The refusal is a usable signal rather than a dead end: it comes back as a
// message the model can read, so the agent rewrites without the capability or
// calls capability_request for a human to decide. That failure path is a feature,
// and capability_request is what makes it a path rather than a dead end: an approved
// request widens the ceiling on the running daemon, so the same write then succeeds.
func resolveCeiling(decl Declaration, ceiling Ceiling) (Grant, error) {
	declared, err := decl.capabilities()
	if err != nil {
		return Grant{}, err
	}

	available := ceiling.capabilities()
	for _, c := range declared {
		if !slices.Contains(available, c) {
			return Grant{}, fmt.Errorf(
				"capability %s is not available to generated tools on this instance; "+
					"rewrite the tool without it, or call capability_request to ask an operator to grant it", c)
		}
	}

	// Grant only what was declared, drawing the parameters from the ceiling. A
	// tool declaring fs.read gets exactly the ceiling's mounts; one declaring
	// nothing gets an empty Grant, which is the common case and the intended one.
	var g Grant
	for _, c := range declared {
		switch c {
		case CapFSRead:
			g.FSRead = ceiling.FSRead
		case CapFSWrite:
			g.FSWrite = ceiling.FSWrite
		case CapEnvRead:
			// Env keys are named on both sides, so the tool gets the intersection
			// rather than the whole ceiling: declaring one key must not confer the
			// others an operator happened to list.
			for _, k := range decl.Env {
				if slices.Contains(ceiling.Env, k) {
					g.Env = append(g.Env, k)
				}
			}
			if len(g.Env) != len(decl.Env) {
				return Grant{}, fmt.Errorf(
					"one or more declared env keys are not available to generated tools on this instance (available: %v)", ceiling.Env)
			}
		case CapNetHTTP:
			g.HTTP = ceiling.HTTP
		case CapState:
			g.State = ceiling.State
		}
	}
	return g, nil
}

// grantDescription is what `nine.caps` tells a guest about itself. It is
// deliberately a *description* and never a grant: the fields below are already
// enforced elsewhere — the filesystem by wazero's pre-opens, the environment by
// what WithEnv passed, net.http by the per-call lookup in hostHTTP — and nothing
// reads this back to make a decision.
//
// Host paths are not included. A tool sees the guest path it was mounted at, and
// telling it where that lives on the operator's disk would leak the one detail
// the mapping exists to hide.
type grantDescription struct {
	FSRead  []string `json:"fs_read,omitempty"`
	FSWrite []string `json:"fs_write,omitempty"`
	Env     []string `json:"env,omitempty"`
	HTTP    *struct {
		AllowHosts []string `json:"allow_hosts"`
		Methods    []string `json:"methods"`
	} `json:"net_http,omitempty"`
	// State tells a guest its scope and quotas. Both are safe to disclose and
	// useful: a tool that knows its key budget can evict rather than discover the
	// ceiling by hitting it, and nothing reads this back to enforce anything
	// (I-TVM.8).
	State *struct {
		Scope      string `json:"scope"`
		MaxKeys    int    `json:"max_keys"`
		MaxValueKB int    `json:"max_value_kb"`
		MaxTotalKB int    `json:"max_total_kb"`
	} `json:"state,omitempty"`
}

func (g Grant) describe() grantDescription {
	d := grantDescription{Env: g.Env}
	for _, m := range g.FSRead {
		d.FSRead = append(d.FSRead, m.Guest)
	}
	for _, m := range g.FSWrite {
		d.FSWrite = append(d.FSWrite, m.Guest)
	}
	if g.HTTP != nil {
		d.HTTP = &struct {
			AllowHosts []string `json:"allow_hosts"`
			Methods    []string `json:"methods"`
		}{AllowHosts: g.HTTP.AllowHosts, Methods: g.HTTP.Methods}
	}
	if g.State != nil {
		s := g.State.withDefaults()
		d.State = &struct {
			Scope      string `json:"scope"`
			MaxKeys    int    `json:"max_keys"`
			MaxValueKB int    `json:"max_value_kb"`
			MaxTotalKB int    `json:"max_total_kb"`
		}{Scope: s.Scope, MaxKeys: s.MaxKeys, MaxValueKB: s.MaxValueKB, MaxTotalKB: s.MaxTotalKB}
	}
	return d
}
