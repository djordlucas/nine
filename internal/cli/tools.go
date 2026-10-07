package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"nine/internal/config"
	"nine/internal/protocol"
	"nine/internal/toolvm"
)

// Tools prints the daemon's sandboxed-tool roster: every tool loaded from
// [tools].user_dir with the capabilities it actually runs with, plus any that
// were skipped at load with the reason. It requires a running daemon — the
// roster is live state, and the resolved grant depends on config the daemon read.
func (c *CLI) Tools(cfg *config.Config) error {
	tools, err := c.sandboxedRoster(cfg, false)
	if err != nil || tools == nil {
		return err
	}
	printSandboxedTools(c.Out, tools)
	return nil
}

// ToolsReload asks the daemon to re-scan the sandboxed-tool directory and reload
// it, then prints the resulting roster. Tools that appear are picked up by
// subsequently-built agent loops; turns already in flight keep the tool set they
// started with.
func (c *CLI) ToolsReload(cfg *config.Config) error {
	tools, err := c.sandboxedRoster(cfg, true)
	if err != nil || tools == nil {
		return err
	}
	fmt.Fprintln(c.Out, "Reloaded sandboxed tools.")
	printSandboxedTools(c.Out, tools)
	return nil
}

// ToolsDelete deletes a tool Nine wrote. The daemon refuses a shipped,
// operator-installed or unknown tool and says why.
func (c *CLI) ToolsDelete(cfg *config.Config, name string) error {
	cl, done, err := c.dialDaemon(cfg)
	if err != nil || cl == nil {
		return err
	}
	defer done()

	msg, err := cl.DeleteTool(name)
	if err != nil {
		return err
	}
	fmt.Fprintln(c.Out, msg)
	return nil
}

// ToolsShow prints one tool in full: its provenance, its resolved grant, and its
// source. "What code ran, with what reach, on whose authority" should have an
// exact answer, and this is where it is read.
func (c *CLI) ToolsShow(cfg *config.Config, name string) error {
	tools, err := c.sandboxedRoster(cfg, false)
	if err != nil || tools == nil {
		return err
	}
	for _, t := range tools {
		if t.Name != name {
			continue
		}
		fmt.Fprintf(c.Out, "%s\n", t.Name)
		fmt.Fprintf(c.Out, "  kind          %s\n", provenance(t))
		fmt.Fprintf(c.Out, "  status        %s\n", loadedLabel(t))
		fmt.Fprintf(c.Out, "  capabilities  %s\n", orNone(t.Capabilities))
		if t.Timeout != "" {
			// Shown only when overridden: a tool on the global deadline should not
			// imply it has one of its own.
			fmt.Fprintf(c.Out, "  timeout       %s (overrides [tools] timeout)\n", t.Timeout)
		}
		if t.Generated {
			fmt.Fprintf(c.Out, "  source        generated (in the store)\n")
			fmt.Fprintf(c.Out, "  dependencies  %s\n", orNone(strings.Join(t.Deps, ", ")))
		} else {
			fmt.Fprintf(c.Out, "  manifest      %s\n", t.ManifestPath)
		}
		if t.Description != "" {
			fmt.Fprintf(c.Out, "  description   %s\n", t.Description)
		}
		if t.Error != "" {
			fmt.Fprintf(c.Out, "  error         %s\n", t.Error)
		}
		return nil
	}
	return fmt.Errorf("no sandboxed tool named %q; run `nine tools` for the roster", name)
}

// sandboxedRoster fetches the roster, printing and returning nil when there is
// no daemon or the subsystem is off — both are ordinary states, not errors.
func (c *CLI) sandboxedRoster(cfg *config.Config, reload bool) ([]protocol.SandboxedToolStatus, error) {
	sock := cfg.SocketPath()
	if !protocol.CanConnect(sock) {
		fmt.Fprintln(c.Out, "no daemon running")
		return nil, nil
	}
	cl, err := protocol.Connect(sock)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer cl.Close() //nolint:errcheck

	var tools []protocol.SandboxedToolStatus
	if reload {
		tools, err = cl.ReloadSandboxedTools()
	} else {
		tools, err = cl.ListSandboxedTools()
	}
	if err != nil {
		return nil, err
	}
	if len(tools) == 0 && cfg.Tools.UserDir == "" {
		fmt.Fprintln(c.Out, "Sandboxed tools are not enabled. Set [tools] enabled = true and user_dir in nine.toml.")
		return nil, nil
	}
	return tools, nil
}

// printSandboxedTools renders the roster, loaded tools first, with a trailing
// note for any that were skipped. A skipped tool is the whole reason an operator
// runs this, so its reason is printed inline rather than behind a flag.
func printSandboxedTools(out io.Writer, tools []protocol.SandboxedToolStatus) {
	if len(tools) == 0 {
		fmt.Fprintln(out, "No sandboxed tools loaded.")
		return
	}
	var skipped int
	for _, t := range tools {
		kind := t.Kind
		if t.Generated {
			// Provenance, not a wasm kind: a generated tool is always `js`, so the
			// column is free to carry the more useful distinction.
			kind = "gen"
		}
		if t.Loaded {
			fmt.Fprintf(out, "  ok    %-18s %-6s %s\n", t.Name, kind, orNone(t.Capabilities))
			continue
		}
		skipped++
		fmt.Fprintf(out, "  SKIP  %-18s %-6s %s\n", t.Name, kind, t.Error)
	}
	if skipped > 0 {
		fmt.Fprintf(out, "\n%d sandboxed tool(s) skipped; the daemon is running without them.\n", skipped)
	}
}

// ToolsDeps lists the external npm packages each generated tool carries, so
// "what third-party code is in this daemon, and which tool pulled it in" has one
// answer (docs/sandboxed-tools.md §4.4).
func (c *CLI) ToolsDeps(cfg *config.Config) error {
	tools, err := c.sandboxedRoster(cfg, false)
	if err != nil || tools == nil {
		return err
	}
	var found bool
	for _, t := range tools {
		if len(t.Deps) == 0 {
			continue
		}
		found = true
		fmt.Fprintf(c.Out, "%-18s %s\n", t.Name, strings.Join(t.Deps, ", "))
	}
	if !found {
		fmt.Fprintln(c.Out, "No generated tool has external dependencies.")
	}
	return nil
}

// provenance labels a tool's origin for `nine tools show`. A generated tool is
// always js, so the column carries the more useful distinction.
func provenance(t protocol.SandboxedToolStatus) string {
	if t.Generated {
		return "generated (js)"
	}
	return t.Kind
}

func loadedLabel(t protocol.SandboxedToolStatus) string {
	if t.Loaded {
		return "loaded"
	}
	return "skipped"
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// ToolValidate runs the same manifest checks the daemon uses at load time,
// without a running daemon and without instantiating anything.
//
// It deliberately does not resolve capabilities: a grant lives in nine.toml on
// the host and the answer is "it depends where you install this", so validating
// against the local config would report a confident result that does not
// transfer. What it does check is everything the developer owns — the manifest's
// shape, the entrypoint's presence, the schema's validity, and, for a `wasm`
// tool, that the module exports the ABI.
//
// With no path it validates the configured [tools].user_dir. A path may be a
// directory or a single `.toml` manifest.
func (c *CLI) ToolValidate(cfg *config.Config, path string) error {
	if path == "" {
		path = cfg.Tools.UserDir
		if path == "" {
			return fmt.Errorf("no path given and [tools].user_dir is not set in nine.toml")
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return c.validateToolManifest(path)
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	var manifests []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".toml" {
			manifests = append(manifests, filepath.Join(path, e.Name()))
		}
	}
	sort.Strings(manifests)
	if len(manifests) == 0 {
		fmt.Fprintf(c.Out, "No tool manifests (*.toml) in %s\n", path)
		return nil
	}

	var bad int
	for _, m := range manifests {
		if err := c.validateToolManifest(m); err != nil {
			bad++
		}
	}
	if bad > 0 {
		return fmt.Errorf("%d of %d manifest(s) invalid", bad, len(manifests))
	}
	return nil
}

// validateToolManifest checks one manifest and prints the verdict. It returns
// the error as well as printing it, so a directory sweep can count failures
// without stopping at the first.
func (c *CLI) validateToolManifest(path string) error {
	m, err := toolvm.LoadManifest(path)
	if err != nil {
		fmt.Fprintf(c.Out, "  FAIL  %s: %v\n", filepath.Base(path), err)
		return err
	}

	entry := m.Entrypoint
	if !filepath.IsAbs(entry) {
		entry = filepath.Join(filepath.Dir(path), entry)
	}
	if _, err := os.Stat(entry); err != nil {
		fmt.Fprintf(c.Out, "  FAIL  %s: entrypoint %q not found\n", filepath.Base(path), m.Entrypoint)
		return err
	}

	declared := "none"
	if d := declaredSummary(m.Capabilities); d != "" {
		declared = d + " (must be granted in nine.toml before this tool will load)"
	}
	fmt.Fprintf(c.Out, "  ok    %-18s %-6s declares: %s\n", m.Name, m.Kind, declared)
	return nil
}

// declaredSummary renders a manifest's declared capabilities for the validate
// output. Phrased as a declaration, never a grant — the distinction is the whole
// capability model and the CLI should not blur it.
func declaredSummary(d toolvm.Declaration) string {
	var parts []string
	for _, v := range d.FS {
		parts = append(parts, "fs."+v)
	}
	for _, v := range d.Net {
		parts = append(parts, "net."+v)
	}
	if len(d.Env) > 0 {
		parts = append(parts, "env "+strings.Join(d.Env, ","))
	}
	if d.State {
		// No parameters: a declaration says the tool remembers, and the scope and
		// quotas are the operator's to confer. `nine tool validate` runs without a
		// daemon and deliberately resolves no grant, so there is nothing more to
		// show here than the need itself.
		parts = append(parts, toolvm.CapState)
	}
	return strings.Join(parts, ", ")
}
