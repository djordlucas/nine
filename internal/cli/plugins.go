package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
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
	// A bare binary has no manifest to say which transport it speaks, so try
	// both: socket HTTP (the default) first, stdio as the fallback.
	desc, err := plugin.ProbeAny(binPath)
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
	desc, err := plugin.ProbeTransport(bin, m.Transport)
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

// PluginInstall installs a user plugin into the running daemon
// (adr/agents-concept.md A7, Q19/Q20): the source is a directory of plugin
// files, a single manifest/binary, or a URL (a .tar.gz is extracted; anything
// else is fetched as one file). The files go to the daemon, which vets them —
// disabled list, MCP handshake, tool collision — lands them in
// [plugins].user_dir and reloads, so the tools reach running sessions from the
// next turn (Q32).
func (c *CLI) PluginInstall(cfg *config.Config, src string) error {
	files, err := pluginInstallFiles(src)
	if err != nil {
		return err
	}
	cl, done, err := c.dialDaemon(cfg)
	if err != nil || cl == nil {
		return err
	}
	defer done()

	plugins, err := cl.InstallPlugin(files)
	if err != nil {
		return fmt.Errorf("install plugin: %w", err)
	}
	fmt.Fprintln(c.Out, "Installed plugin; running sessions get its tools from the next turn.")
	printPlugins(c.Out, plugins)
	return nil
}

// pluginInstallFiles gathers the files to upload from a local path or a URL.
// Keys are paths relative to the plugin root (the manifest sits at the top).
func pluginInstallFiles(src string) (map[string][]byte, error) {
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		return fetchPluginFiles(src)
	}
	info, err := os.Stat(src)
	if err != nil {
		return nil, fmt.Errorf("plugin install: %w", err)
	}
	out := map[string][]byte{}
	if !info.IsDir() {
		raw, err := os.ReadFile(src)
		if err != nil {
			return nil, err
		}
		out[filepath.Base(src)] = raw
		return out, nil
	}
	// Walk with ReadDir (never follows symlinks: a symlinked entry is skipped,
	// not traversed) so the uploaded set is exactly what the operator sees.
	var walk func(dir, rel string) error
	walk = func(dir, rel string) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			r := e.Name()
			if rel != "" {
				r = rel + "/" + e.Name()
			}
			if e.IsDir() {
				if err := walk(p, r); err != nil {
					return err
				}
				continue
			}
			if !e.Type().IsRegular() {
				continue // symlink, socket, …: not uploaded
			}
			raw, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[r] = raw
		}
		return nil
	}
	if err := walk(src, ""); err != nil {
		return nil, err
	}
	return out, nil
}

// fetchPluginFiles downloads src: a .tar.gz/.tgz is extracted entry by entry;
// anything else lands as a single file named after the URL's last path segment.
func fetchPluginFiles(url string) (map[string][]byte, error) {
	resp, err := http.Get(url) //nolint:gosec // the operator named the URL
	if err != nil {
		return nil, fmt.Errorf("plugin install: fetch %s: %w", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("plugin install: fetch %s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	lower := strings.ToLower(url)
	if strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz") {
		gz, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("plugin install: %s is not gzip: %w", url, err)
		}
		defer gz.Close() //nolint:errcheck
		out := map[string][]byte{}
		tr := tar.NewReader(gz)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				return out, nil
			}
			if err != nil {
				return nil, fmt.Errorf("plugin install: read tar: %w", err)
			}
			if hdr.Typeflag != tar.TypeReg {
				continue
			}
			name := filepath.Clean(hdr.Name)
			if strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
				return nil, fmt.Errorf("plugin install: refusing unsafe archive path %q", hdr.Name)
			}
			raw, err := io.ReadAll(tr)
			if err != nil {
				return nil, err
			}
			out[name] = raw
		}
	}
	name := path.Base(strings.Split(url, "?")[0])
	if name == "/" || name == "." || name == "" {
		name = "plugin.toml"
	}
	return map[string][]byte{name: body}, nil
}
