package toolvm

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/tetratelabs/wazero"
)

// Collides reports whether a tool name is already taken elsewhere. The host is
// handed one of these at load so it can enforce "no override, ever" against
// names it cannot see itself — built-in tools and native plugin tools.
type Collides func(toolName string) (owner string, taken bool)

// Load discovers developer tools from the configured directory and registers the
// ones that pass. It replaces the previously-loaded set wholesale, so it doubles
// as reload.
//
// The sequence mirrors user plugins (R-PLUG.9), because their ergonomics are
// already right and an operator should not have to learn a second set of rules:
//
//  1. Candidates are taken in deterministic name order.
//  2. A malformed manifest, a missing entrypoint, or an unreadable schema is
//     recorded as skipped — nothing of that tool is compiled or run.
//  3. Capabilities are resolved against the operator's grant; a mismatch is a
//     named failure rather than a tool that half-works.
//  4. The name is checked against built-ins, plugin tools, and earlier-accepted
//     sandboxed tools. Any collision skips the whole tool — no override, ever.
//  5. Otherwise the module is compiled and the tool registered.
//
// Any single failure is surfaced (logged at ERROR and kept in Status) but never
// aborts the others, so one bad drop-in cannot take the daemon down.
func (h *Host) Load(ctx context.Context, collides Collides) {
	tools := map[string]*Tool{}
	var status []Status

	for _, d := range discoverTools(h.cfg.UserDir) {
		st := Status{Name: d.Name, ManifestPath: d.ManifestPath}

		if d.Err != nil {
			status = append(status, skip(st, d.Err, "manifest"))
			continue
		}
		st.Kind = string(d.Manifest.Kind)

		grant, err := resolveGrant(d.Manifest.Capabilities, h.cfg.Grants[d.Name])
		if err != nil {
			status = append(status, skip(st, err, "capability"))
			continue
		}
		st.Capabilities = grant.Summary()

		if owner, taken := firstCollision(d.Name, tools, collides); taken {
			status = append(status, skip(st, fmt.Errorf("tool %q already provided by %q", d.Name, owner), "collision"))
			continue
		}
		t, err := h.compile(ctx, d, grant)
		if err != nil {
			status = append(status, skip(st, err, "compile"))
			continue
		}

		tools[d.Name] = t
		st.Loaded = true
		status = append(status, st)
		slog.Info("sandboxed tool loaded",
			"name", d.Name, "kind", d.Manifest.Kind, "capabilities", st.Capabilities)
	}

	h.mu.Lock()
	h.tools = tools
	h.status = status
	h.mu.Unlock()

	slog.Info("seeded sandboxed tools",
		"dir", h.cfg.UserDir, "loaded", len(tools), "skipped", len(status)-len(tools))
}

// skip records a rejected candidate and logs it. Every rejection is loud: a tool
// an operator installed that is not running is exactly the thing they need told,
// and a silent skip is how a capability mismatch becomes a mystery.
func skip(st Status, err error, stage string) Status {
	st.Loaded = false
	st.Err = err.Error()
	slog.Error("skipping sandboxed tool",
		"name", st.Name, "manifest", st.ManifestPath, "stage", stage, "err", err)
	return st
}

// firstCollision resolves a name against the sandboxed tools already accepted in
// this pass and then against everything outside the host. Checking the local map
// first is what settles tool-vs-tool collisions in favor of the first loaded,
// which combined with the deterministic order makes the outcome reproducible.
func firstCollision(name string, accepted map[string]*Tool, collides Collides) (string, bool) {
	if _, ok := accepted[name]; ok {
		return "another sandboxed tool", true
	}
	if collides == nil {
		return "", false
	}
	return collides(name)
}

// compile turns an accepted candidate into a callable Tool. A `js` tool shares
// the already-compiled QuickJS blob and carries its source; a `wasm` tool
// compiles its own module, which is also where a module that does not export the
// ABI is caught — at load, not at first call.
func (h *Host) compile(ctx context.Context, d discovered, grant Grant) (*Tool, error) {
	// A resumable `wasm` tool receives its job context under the reserved
	// `nine_job` argument key, since it has no harness to hand a second parameter
	// to. Refuse a schema that declares the same name rather than silently
	// overwriting the author's own argument — the key is only genuinely reserved
	// if something enforces it.
	if d.Manifest.Kind == KindWasm && d.Manifest.Resumable {
		if declaresReservedJobKey(d.SchemaJSON) {
			return nil, fmt.Errorf(
				"input schema declares %q, which is reserved: a resumable wasm tool receives "+
					"its job context under that key; rename the argument", reservedJobKey)
		}
	}

	t := &Tool{
		Name:         d.Manifest.Name,
		DisplayName:  d.Manifest.DisplayName,
		Description:  d.Manifest.Description,
		InputSchema:  d.SchemaJSON,
		Kind:         d.Manifest.Kind,
		Grant:        grant,
		Timeout:      h.cfg.Timeouts[d.Manifest.Name],
		ManifestPath: d.ManifestPath,
		Resumable:    d.Manifest.Resumable,
	}

	if d.Manifest.Kind == KindJS {
		t.module = h.qjs
		t.source = string(d.Source)
		// The `nine:*` standard library, which used to reach only generated tools.
		// That was a leftover rather than a decision: the comment excluding it
		// described the generated tier as not yet existing. It is embedded,
		// pure-ES, dependency-free, and resolved host-side before the call, so
		// admitting a hand-written tool costs nothing and removes the oddity that
		// the author who cannot ask Nine to write them a CSV parser was the one
		// denied the CSV parser.
		t.imports = stdlibModules()
		return t, nil
	}

	mod, err := h.rt.CompileModule(ctx, d.Source)
	if err != nil {
		return nil, fmt.Errorf("compile wasm: %w", err)
	}
	if err := checkABI(mod); err != nil {
		mod.Close(ctx) //nolint:errcheck // rejecting it; the close error is noise
		return nil, err
	}
	t.module = mod
	return t, nil
}

// checkABI refuses a module that does not export the guest contract. Doing this
// at load rather than at first call is the difference between an operator seeing
// "does not export nine_run" in `nine tools` and a model seeing a mystery
// failure three days later.
func checkABI(mod wazero.CompiledModule) error {
	exports := mod.ExportedFunctions()
	for _, name := range []string{exportAlloc, exportRun} {
		if _, ok := exports[name]; !ok {
			return fmt.Errorf("module does not export %q (Nine ABI v%d)", name, ABIVersion)
		}
	}
	return nil
}

// MergeUserTools merges user tools from tempHost into targetHost,
// but does not overwrite existing tools (shipped tools have priority).
// This is used by OpenSandboxedTools to preserve shipped tools while
// still loading user tools.
func MergeUserTools(targetHost, tempHost *Host) {
	targetHost.mu.Lock()
	defer targetHost.mu.Unlock()
	
	for _, tool := range tempHost.Tools() {
		// Only add if the name doesn't already exist (shipped tool takes precedence)
		if _, exists := targetHost.tools[tool.Name]; !exists {
			targetHost.tools[tool.Name] = tool
		}
	}
	// Append user tool statuses to target host status
	targetHost.status = append(targetHost.status, tempHost.Status()...)
}
