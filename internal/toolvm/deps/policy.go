// Package deps resolves and bundles a generated tool's external npm dependencies
// at tool_write time, in the daemon, once (docs/sandboxed-tools.md §4.4). The
// output is one self-contained ESM module with zero imports and zero network at
// call time. This is the single riskiest switch in the sandboxed-tool design;
// every property that makes it defensible — off by default, an operator-named
// allowlist, sha512 integrity, no install scripts ever, and the deps+net.http
// interlock enforced above this package — lives here or in the caller.
package deps

import (
	"fmt"
	"strings"
)

// Mode is the [tools.agent.deps] posture.
const (
	// ModeOff is the default and the shipped posture: no external resolution, the
	// nine:* stdlib only. A tool importing an npm package fails to bundle.
	ModeOff = "off"
	// ModeAllowlist admits only packages an operator named, transitive deps
	// included — the recommended enabled posture.
	ModeAllowlist = "allowlist"
	// ModeOpen admits anything from the registry within the budgets. A development
	// posture, meant to be paired with Frozen once iteration settles.
	ModeOpen = "open"
)

// Allow is one entry of the operator's allowlist: a package name and the semver
// range they are willing to stand behind.
type Allow struct {
	Name    string
	Version string // a semver range, e.g. "^4.17.21"
}

// Policy is [tools.agent.deps], already defaulted by the caller. The zero value
// is ModeOff — no resolution — which is the safe default if a caller forgets to
// set it.
type Policy struct {
	Mode     string
	Registry string // registry base URL, e.g. https://registry.npmjs.org
	Allow    []Allow

	// Budgets bound how far a small allowlist can expand through transitive deps —
	// the mechanism by which a four-package list smuggles in four hundred.
	MaxPackages int // including transitive; 0 → DefaultMaxPackages
	MaxBundleKB int // final bundle size cap; 0 → DefaultMaxBundleKB
	MaxDepth    int // transitive depth; 0 → DefaultMaxDepth

	// Frozen resolves only from the cache/lockfile and never touches the network —
	// the air-gapped / reproducible posture.
	Frozen bool
}

// Defaults for the budgets, applied when a field is 0.
const (
	DefaultRegistry    = "https://registry.npmjs.org"
	DefaultMaxPackages = 24
	DefaultMaxBundleKB = 2048
	DefaultMaxDepth    = 4
)

// Enabled reports whether external resolution is on at all.
func (p Policy) Enabled() bool { return p.Mode == ModeAllowlist || p.Mode == ModeOpen }

func (p Policy) maxPackages() int {
	if p.MaxPackages <= 0 {
		return DefaultMaxPackages
	}
	return p.MaxPackages
}

func (p Policy) maxBundleBytes() int {
	if p.MaxBundleKB <= 0 {
		return DefaultMaxBundleKB * 1024
	}
	return p.MaxBundleKB * 1024
}

func (p Policy) maxDepth() int {
	if p.MaxDepth <= 0 {
		return DefaultMaxDepth
	}
	return p.MaxDepth
}

func (p Policy) registry() string {
	if p.Registry == "" {
		return DefaultRegistry
	}
	return strings.TrimRight(p.Registry, "/")
}

// admit decides whether a package (name) at a requested range may be resolved,
// and with what range. In allowlist mode the name MUST be listed — transitive
// deps included, otherwise the list is decoration (§4.4) — and the operator's
// range overrides whatever a dependent asked for, so the operator's intent wins.
// In open mode any name is admitted at the requested range.
//
// requested is the range the importer declared (a dependent's package.json entry,
// or "" for a top-level tool import, which then resolves to the allowlist range
// or, in open mode, latest).
func (p Policy) admit(name, requested string) (string, error) {
	switch p.Mode {
	case ModeAllowlist:
		for _, a := range p.Allow {
			if a.Name == name {
				return a.Version, nil
			}
		}
		return "", fmt.Errorf("package %q is not on [tools.agent.deps].allow (allowlist mode); "+
			"add it, or the tool cannot use it", name)
	case ModeOpen:
		if requested == "" {
			return "*", nil // latest satisfying
		}
		return requested, nil
	default:
		return "", fmt.Errorf("external dependencies are disabled ([tools.agent.deps].mode = %q); "+
			"import only nine:* modules, or ask an operator to enable deps", p.Mode)
	}
}
