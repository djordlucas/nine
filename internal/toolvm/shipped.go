package toolvm

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"log/slog"
)

// The shipped tier: first-party tools compiled into the `nine` binary.
//
// This is the third source tier, after developer (a file and manifest on disk,
// R-TVM.10) and generated (a row in `tools`, R-TVM.14). It exists so that the
// capabilities Nine ships with go through the same sandbox as everything else.
//
// The tools here were built-in *plugins*: subprocesses running with the daemon's
// full uid authority. A plugin that reads a clock had, in principle, the reach to
// read the operator's home directory — not because anyone wanted that, but
// because a subprocess inherits it. Moving them under the capability model
// replaces ambient authority with a declared, operator-visible grant, which is
// what the model exists for (F7 in docs/architecture-review.md).
//
// A shipped tool is granted what it declares. That is the one way this tier
// differs from the other two, and it is not a weakening: a developer tool is
// granted by an operator who did not write it, and a generated tool is capped by
// a ceiling because Nine wrote it. A shipped tool is first-party code the
// operator already ran as a plugin with strictly more authority, so the grant is
// a **reduction** made explicit rather than a new trust. It is still visible in
// `nine tools`, and still refused if it declares something the host cannot
// confer.

//go:embed shipped/*.js
var shippedFS embed.FS

// shippedTool is one first-party tool: its manifest, inline, beside the source
// file it names.
type shippedTool struct {
	Name        string
	DisplayName string
	Description string
	Schema      string // JSON Schema for the arguments
	File        string // path within shippedFS
	// Declaration is what this tool needs. An empty Declaration means no
	// filesystem pre-open and no host functions beyond the ABI — the tool can
	// compute and nothing else.
	Declaration Declaration
}

// shippedTools is the catalog. Adding one is this entry plus its .js file.
var shippedTools = []shippedTool{
	{
		Name:        "time",
		DisplayName: "Get Time",
		Description: "Return the current date and time in UTC (ISO-8601 and a readable form).",
		Schema:      `{"type":"object","properties":{}}`,
		File:        "shipped/time.js",
		// Reading a clock needs nothing. This is the whole point of the
		// migration: as a plugin it had the daemon's authority, and as a tool it
		// has none.
		Declaration: Declaration{},
	},
	{
		Name:        "read_file",
		DisplayName: "Read File",
		Description: "Read a file from the workspace. Paths resolve under /work; a relative path is taken as relative to it.",
		Schema:      `{"type":"object","required":["path"],"properties":{"path":{"type":"string","description":"Path under the workspace, e.g. notes.txt or /work/notes.txt"}}}`,
		File:        "shipped/read_file.js",
		Declaration: Declaration{FS: []string{"read"}},
	},
	{
		Name:        "write_file",
		DisplayName: "Write File",
		Description: "Write content to a file in the workspace, creating parent directories as needed. Paths resolve under /work.",
		Schema:      `{"type":"object","required":["path","content"],"properties":{"path":{"type":"string"},"content":{"type":"string"}}}`,
		File:        "shipped/write_file.js",
		Declaration: Declaration{FS: []string{"write"}},
	},
}

// ShippedWorkspace is the host directory a shipped tool's fs grant points at.
//
// The guest path is fixed at /work because that is the alias the `files` plugin
// already accepted, so a model that learned "/work/notes.txt" keeps working. The
// host side comes from the daemon (NINE_WORKSPACE, or the eval harness's
// per-case dir). Empty means no workspace, and a tool declaring fs then fails to
// load rather than registering with a capability that silently does nothing.
type ShippedWorkspace struct{ Host string }

// shippedWorkspaceGuest is where the workspace appears inside the sandbox.
const shippedWorkspaceGuest = "/work"

// SetShippedWorkspace installs the mount used by shipped tools that declare fs.
// Must be called before LoadShipped.
func (h *Host) SetShippedWorkspace(w ShippedWorkspace) {
	h.mu.Lock()
	h.shippedWorkspace = w
	h.mu.Unlock()
}

// LoadShipped registers the first-party tools compiled into the binary.
//
// It runs before Load and LoadGenerated so that a developer or generated tool
// cannot take a shipped tool's name — the namespace rule is first-registered
// wins, and a shipped tool losing its name to a later one would silently replace
// first-party behavior.
func (h *Host) LoadShipped(ctx context.Context, collides Collides) {
	var status []Status
	for _, s := range shippedTools {
		st := Status{Name: s.Name, Kind: string(KindJS), Shipped: true}

		src, err := shippedFS.ReadFile(s.File)
		if err != nil {
			// Unreachable in a correct build — go:embed would have failed — so
			// this is a guard against a catalog entry naming a file nobody added.
			status = append(status, skip(st, fmt.Errorf("shipped source %s: %w", s.File, err), "read"))
			continue
		}

		h.mu.RLock()
		ws := h.shippedWorkspace
		h.mu.RUnlock()

		grant, err := resolveShipped(s.Declaration, ws)
		if err != nil {
			status = append(status, skip(st, err, "grant"))
			continue
		}
		st.Capabilities = grant.Summary()

		if collides != nil {
			if owner, taken := collides(s.Name); taken {
				status = append(status, skip(st,
					fmt.Errorf("tool %q already provided by %q", s.Name, owner), "collision"))
				continue
			}
		}

		t := &Tool{
			Name:        s.Name,
			DisplayName: s.DisplayName,
			Description: s.Description,
			InputSchema: json.RawMessage(s.Schema),
			Kind:        KindJS,
			Grant:       grant,
			Shipped:     true,
			module:      h.qjs,
			source:      string(src),
			imports:     stdlibModules(),
		}

		h.mu.Lock()
		h.tools[s.Name] = t
		h.mu.Unlock()

		st.Loaded = true
		status = append(status, st)
	}

	h.mu.Lock()
	h.shippedStatus = status
	h.mu.Unlock()

	loaded := 0
	for _, s := range status {
		if s.Loaded {
			loaded++
		}
	}
	slog.Info("loaded shipped tools", "loaded", loaded, "skipped", len(status)-loaded)
	_ = ctx
}

// resolveShipped turns a shipped tool's declaration into its grant.
//
// Unlike resolveGrant it has no operator table to consult — the declaration is
// the grant — but it keeps the property that makes the capability model work:
// the host confers only what it can actually enforce, so a declaration naming
// something unsupported is refused rather than silently dropped. A tool that
// declares nothing gets nothing, which is the common case and the reason `time`
// was the right one to migrate first.
func resolveShipped(d Declaration, ws ShippedWorkspace) (Grant, error) {
	var g Grant
	if len(d.FS) > 0 {
		if ws.Host == "" {
			return Grant{}, fmt.Errorf("shipped tool declares fs %v but no workspace is configured", d.FS)
		}
		m := Mount{Host: ws.Host, Guest: shippedWorkspaceGuest}
		for _, verb := range d.FS {
			switch verb {
			case "read":
				g.FSRead = append(g.FSRead, m)
			case "write":
				// A write mount is readable too (nine:fs treats either grant as
				// admitting a read), so a tool that writes can read back what it
				// wrote — which the plugin could, and which write_file needs.
				g.FSWrite = append(g.FSWrite, m)
			default:
				return Grant{}, fmt.Errorf("unknown fs capability %q", verb)
			}
		}
	}
	if len(d.Net) > 0 {
		return Grant{}, fmt.Errorf("shipped tool declares net %v, which needs an allowlist the shipped tier does not yet define", d.Net)
	}
	g.Env = append(g.Env, d.Env...)
	return g, nil
}
