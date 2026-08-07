package toolvm

import (
	"fmt"
	"slices"
	"sort"
	"strings"
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
	capFSVerbRd = "read"
	capFSVerbWr = "write"
)

// Declaration is what a tool's manifest says it needs. It is documentation and
// a pre-flight check, never a security control: nothing reads it at call time,
// so a manifest that lies gains nothing (docs/sandboxed-tools.md §6.3).
type Declaration struct {
	// FS lists filesystem verbs: "read", "write".
	FS []string `toml:"fs"`
	// Net lists network verbs: "http".
	Net []string `toml:"net"`
	// Env lists the environment keys the tool reads, by name. Keys rather than a
	// verb, because for env the key *is* the parameter — and naming them in the
	// manifest is what lets an operator diff a declaration against a grant.
	Env []string `toml:"env"`
}

// Mount is one host→guest filesystem mapping.
type Mount struct {
	Host  string
	Guest string
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
		default:
			parts = append(parts, c)
		}
	}
	return strings.Join(parts, "; ")
}

func mountList(ms []Mount) string {
	parts := make([]string, len(ms))
	for i, m := range ms {
		parts[i] = m.Host + "=>" + m.Guest
	}
	return strings.Join(parts, ",")
}
