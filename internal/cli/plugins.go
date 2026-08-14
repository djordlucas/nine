package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"nine/internal/config"
	"nine/internal/plugin"
	"nine/internal/protocol"
)

// Plugins prints the daemon's plugin roster: built-in and user plugins that are
// loaded, plus any user plugins that were skipped at boot with the reason. It
// requires a running daemon — the roster is live state, not something the CLI can
// derive on its own.
func (c *CLI) Plugins(cfg *config.Config) error {
	sock := cfg.SocketPath()
	if !protocol.CanConnect(sock) {
		fmt.Fprintln(c.Out, "no daemon running")
		return nil
	}
	cl, err := protocol.Connect(sock)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer cl.Close() //nolint:errcheck

	plugins, err := cl.ListPlugins()
	if err != nil {
		return fmt.Errorf("list plugins: %w", err)
	}
	printPlugins(c.Out, plugins)
	return nil
}

// PluginsReload asks the daemon to re-scan the user-plugin directory and reload
// it, then prints the resulting roster. Built-in plugins are untouched.
func (c *CLI) PluginsReload(cfg *config.Config) error {
	sock := cfg.SocketPath()
	if !protocol.CanConnect(sock) {
		fmt.Fprintln(c.Out, "no daemon running; nothing to reload")
		return nil
	}
	cl, err := protocol.Connect(sock)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer cl.Close() //nolint:errcheck

	plugins, err := cl.ReloadPlugins()
	if err != nil {
		return fmt.Errorf("reload plugins: %w", err)
	}
	fmt.Fprintln(c.Out, "Reloaded user plugins.")
	printPlugins(c.Out, plugins)
	return nil
}

// printPlugins renders a plugin roster, built-ins first, then user plugins, with
// a trailing note for any that were skipped.
func printPlugins(out io.Writer, plugins []protocol.PluginStatus) {
	if len(plugins) == 0 {
		fmt.Fprintln(out, "No plugins loaded.")
		return
	}
	var skipped, disabled int
	for _, p := range plugins {
		if p.Loaded {
			tools := ""
			if len(p.Tools) > 0 {
				tools = "  [" + strings.Join(p.Tools, ", ") + "]"
			}
			fmt.Fprintf(out, "  ok    %-14s %-8s%s\n", p.Name, p.Source, tools)
			continue
		}
		// A plugin the operator switched off is not a failure, and reading it as
		// one sends people hunting for a fault that is not there.
		if p.Disabled {
			disabled++
			fmt.Fprintf(out, "  off   %-14s %-8s disabled in [plugins].disabled\n", p.Name, p.Source)
			continue
		}
		skipped++
		fmt.Fprintf(out, "  SKIP  %-14s %-8s %s\n", p.Name, p.Source, p.Error)
	}
	if skipped > 0 {
		fmt.Fprintf(out, "\n%d plugin(s) skipped; the daemon is running without them.\n", skipped)
	}
	if disabled > 0 {
		fmt.Fprintf(out, "\n%d plugin(s) disabled by config.\n", disabled)
	}
}

// PluginValidate runs the same handshake the daemon uses at load time against a
// user plugin, without adding it — a local check that needs no running daemon.
//
// With no path it validates the configured [plugins].user_dir. A path may be a
// directory (validate every manifest in it), a `.toml` manifest, or a plugin
// binary directly.
func (c *CLI) PluginValidate(cfg *config.Config, path string) error {
	if path == "" {
		path = cfg.Plugins.UserDir
		if path == "" {
			return fmt.Errorf("no path given and [plugins].user_dir is not set in nine.toml")
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		return err
	}

	if info.IsDir() {
		return c.validatePluginDir(path)
	}
	if filepath.Ext(path) == ".toml" {
		return c.validateManifest(path)
	}
	return c.validateBinary(path)
}

// validatePluginDir validates every manifest found in dir and reports duplicate
// tool names across the set (the daemon rejects such collisions at load).
func (c *CLI) validatePluginDir(dir string) error {
	manifests, err := filepath.Glob(filepath.Join(dir, "*.toml"))
	if err != nil {
		return err
	}
	if len(manifests) == 0 {
		fmt.Fprintf(c.Out, "No plugin manifests found in %s\n", dir)
		fmt.Fprintf(c.Out, "Expected %s/<name>.toml beside each plugin binary.\n", dir)
		return nil
	}
	sort.Strings(manifests)

	claimed := make(map[string]string) // tool name → manifest that claimed it
	var bad int
	for _, mf := range manifests {
		tools, err := c.probeManifest(mf)
		if err != nil {
			bad++
			fmt.Fprintf(c.Out, "  FAIL  %s\n          %v\n", relPath(mf), err)
			continue
		}
		if dup := firstClaimed(tools, claimed, relPath(mf)); dup != "" {
			bad++
			fmt.Fprintf(c.Out, "  FAIL  %s\n          %s\n", relPath(mf), dup)
			continue
		}
		fmt.Fprintf(c.Out, "  ok    %-24s [%s]\n", relPath(mf), strings.Join(tools, ", "))
	}

	fmt.Fprintf(c.Out, "\n%d manifest(s) checked, %d invalid.\n", len(manifests), bad)
	if bad > 0 {
		fmt.Fprintf(c.Out, "Invalid plugins are skipped at boot; the daemon still starts without them.\n")
		return fmt.Errorf("%d plugin(s) failed validation", bad)
	}
	return nil
}

func (c *CLI) validateManifest(path string) error {
	tools, err := c.probeManifest(path)
	if err != nil {
		fmt.Fprintf(c.Out, "  FAIL  %s\n          %v\n", relPath(path), err)
		return fmt.Errorf("plugin failed validation")
	}
	fmt.Fprintf(c.Out, "  ok    %s  [%s]\n", relPath(path), strings.Join(tools, ", "))
	return nil
}

func (c *CLI) validateBinary(binPath string) error {
	desc, err := plugin.Probe(binPath)
	if err != nil {
		fmt.Fprintf(c.Out, "  FAIL  %s\n          %v\n", relPath(binPath), err)
		return fmt.Errorf("plugin failed validation")
	}
	names := make([]string, len(desc.Tools))
	for i, t := range desc.Tools {
		names[i] = t.Name
	}
	fmt.Fprintf(c.Out, "  ok    %s  [%s]\n", relPath(binPath), strings.Join(names, ", "))
	return nil
}

// probeManifest loads a manifest, resolves its entrypoint, and probes the binary,
// returning the advertised tool names.
func (c *CLI) probeManifest(manifestPath string) ([]string, error) {
	m, err := plugin.LoadManifest(manifestPath)
	if err != nil {
		return nil, err
	}
	bin := m.Entrypoint
	if !filepath.IsAbs(bin) {
		bin = filepath.Join(filepath.Dir(manifestPath), bin)
	}
	if _, err := os.Stat(bin); err != nil {
		return nil, fmt.Errorf("entrypoint %q not found: %w", m.Entrypoint, err)
	}
	desc, err := plugin.Probe(bin)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(desc.Tools))
	for i, t := range desc.Tools {
		names[i] = t.Name
	}
	return names, nil
}

// firstClaimed records each tool under owner and returns a message for the first
// tool already claimed by an earlier manifest, or "" when none collide.
func firstClaimed(tools []string, claimed map[string]string, owner string) string {
	for _, t := range tools {
		if prev, ok := claimed[t]; ok {
			return fmt.Sprintf("tool %q already claimed by %s", t, prev)
		}
	}
	for _, t := range tools {
		claimed[t] = owner
	}
	return ""
}
