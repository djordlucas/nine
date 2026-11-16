package plugin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// LoadUserPlugins discovers and starts operator plugins from dir and remembers
// dir for later ReloadUserPlugins calls. It is the boot entry point; an empty or
// absent dir loads nothing. See loadUserPlugins for the per-plugin contract.
func (m *Manager) LoadUserPlugins(dir string) {
	m.mu.Lock()
	m.userDir = dir
	m.mu.Unlock()
	m.loadUserPlugins(dir)
}

// ReloadUserPlugins re-runs discovery against the directory last passed to
// LoadUserPlugins: it stops every currently-running user plugin, then loads the
// directory afresh. Built-in plugins are untouched. Newly-started plugins are
// picked up by subsequently-built agent loops (the builder reads Running() at
// build time); turns already in flight keep the tool set they started with.
func (m *Manager) ReloadUserPlugins() {
	m.mu.Lock()
	dir := m.userDir
	m.mu.Unlock()
	m.loadUserPlugins(dir)
}

// loadUserPlugins reconciles running user plugins to what dir declares. It first
// stops any running user plugins (a no-op on first boot), then for each
// discovered candidate, in deterministic name order:
//
//  1. A malformed manifest or missing binary is recorded as skipped — the binary
//     is never executed.
//  2. The binary is probed through the MCP handshake; a non-MCP binary is
//     recorded as skipped.
//  3. Its tools are checked against every already-running plugin (built-in first,
//     then earlier-accepted user plugins). Any collision skips the whole plugin —
//     no override, ever.
//  4. Otherwise it is started and tracked as a user plugin.
//
// Any single failure is surfaced (logged at ERROR and kept in UserStatus) but
// never aborts the others, so one bad drop-in cannot take the daemon down.
func (m *Manager) loadUserPlugins(dir string) {
	for _, p := range m.Running() {
		if p.User {
			m.Stop(p) //nolint:errcheck // best-effort; we are replacing it
		}
	}

	var status []UserPluginStatus
	for _, d := range discoverPlugins(dir) {
		st := UserPluginStatus{Name: d.Name, Manifest: d.ManifestPath}

		if d.Err != nil {
			st.Err = d.Err.Error()
			slog.Error("skipping user plugin", "name", d.Name, "manifest", d.ManifestPath, "err", d.Err)
			status = append(status, st)
			continue
		}

		// Checked before Probe, which would otherwise spawn the binary just to
		// refuse it a moment later (R-PLUG.14).
		if m.IsDisabled(d.Name) {
			st.Err = ErrPluginDisabled.Error()
			// Recorded as matched so UnmatchedDisabled does not report this name
			// as a typo; the roster entry itself comes from st, not from there.
			m.noteDisabledSkip(d.Name)
			slog.Info("skipping user plugin: disabled by config", "name", d.Name)
			status = append(status, st)
			continue
		}

		// Operator settings (docs/plugin-capabilities.md §3) apply to user plugins,
		// which otherwise receive no environment. Probe gets the manager env plus
		// the settings; Start prepends the manager env itself, so it gets settings
		// alone. The manifest's transport selects the wire: socket HTTP by
		// default, stdio when declared (R-PLUG.1, adr/modular_nine.md §9.6).
		extra := m.envFor(d.Name)
		probeEnv := append(append([]string{}, m.env...), extra...)
		desc, err := ProbeTransport(d.BinaryPath, d.Transport, probeEnv...)
		if err != nil {
			st.Err = err.Error()
			slog.Error("skipping user plugin: not a valid MCP plugin", "name", d.Name, "path", d.BinaryPath, "err", err)
			status = append(status, st)
			continue
		}

		if owner, tool := m.firstToolCollision(desc.Tools); tool != "" {
			st.Err = fmt.Sprintf("tool %q already provided by %q", tool, owner)
			slog.Error("skipping user plugin: tool collision", "name", d.Name, "tool", tool, "owner", owner)
			status = append(status, st)
			continue
		}

		var p *Plugin
		if d.Transport == TransportStdio {
			p, err = m.StartStdio(d.BinaryPath, extra...)
		} else {
			p, err = m.Start(d.BinaryPath, extra...)
		}
		if err != nil {
			st.Err = err.Error()
			slog.Error("skipping user plugin: start failed", "name", d.Name, "path", d.BinaryPath, "err", err)
			status = append(status, st)
			continue
		}
		p.User = true

		// Health gate (adr/agents-concept.md A7): the plugin answered the
		// spawn handshake, but its tools are exposed only once it answers on
		// its live connection too — a plugin that died between the two is
		// stopped here and recorded skipped, never half-exposed.
		if err := m.HealthCheck(context.Background(), p); err != nil {
			st.Err = err.Error()
			m.Stop(p) //nolint:errcheck
			slog.Error("skipping user plugin: health check failed", "name", d.Name, "err", err)
			status = append(status, st)
			continue
		}

		st.Loaded = true
		st.Tools = toolNames(desc.Tools)
		status = append(status, st)
		slog.Info("user plugin loaded", "name", d.Name, "tools", len(desc.Tools))
	}

	m.mu.Lock()
	m.userStatus = status
	m.mu.Unlock()

	loaded := 0
	for _, s := range status {
		if s.Loaded {
			loaded++
		}
	}
	slog.Info("seeded user plugins", "dir", dir, "loaded", loaded, "skipped", len(status)-loaded)
}

// firstToolCollision returns the owning plugin and tool name of the first tool in
// tools whose name is already advertised by a running plugin, or ("","") if none
// collide. Because accepted user plugins are running by the time the next is
// checked, this also settles user-vs-user collisions in favor of the first loaded.
func (m *Manager) firstToolCollision(tools []ToolDefinition) (owner, tool string) {
	owners := make(map[string]string)
	for _, p := range m.Running() {
		for _, t := range p.Tools {
			owners[t.Name] = p.Name
		}
	}
	for _, t := range tools {
		if o, ok := owners[t.Name]; ok {
			return o, t.Name
		}
	}
	return "", ""
}

// UserStatus returns a snapshot of the outcome of the last user-plugin load, for
// `nine plugins` reporting.
func (m *Manager) UserStatus() []UserPluginStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]UserPluginStatus, len(m.userStatus))
	copy(out, m.userStatus)
	return out
}

// toolNames extracts the tool names from a set of definitions.
func toolNames(tools []ToolDefinition) []string {
	if len(tools) == 0 {
		return nil
	}
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	return names
}

// InstallUserPlugin installs a user plugin into a running Nine
// (adr/agents-concept.md A7, Q19/Q20): files (manifest + entrypoint + any
// sidecars, keyed by relative path) are staged beside the user dir, vetted —
// disabled-list, MCP handshake probe, tool collision against every running
// plugin — then moved into place and the existing reload runs, so the plugin's
// tools reach sessions from their next turn (Q32). Nothing is overwritten: an
// existing file at a target path is a refusal. It returns the plugin name.
//
// files values are raw bytes; the wire carries them base64 (see
// protocol.NewPluginInstallMsg). The caller is the operator surface — the
// daemon never exposes this to a model tool.
func (m *Manager) InstallUserPlugin(files map[string][]byte) (string, error) {
	m.mu.Lock()
	dir := m.userDir
	m.mu.Unlock()
	if dir == "" {
		return "", fmt.Errorf("plugin install: no user plugin directory is configured ([plugins].user_dir)")
	}
	if len(files) == 0 {
		return "", fmt.Errorf("plugin install: no files")
	}

	// Exactly one top-level manifest; every path relative and inside the dir.
	var manifestName string
	names := make([]string, 0, len(files))
	for name := range files {
		clean := filepath.Clean(name)
		if name == "" || filepath.IsAbs(name) || clean == "." || clean == ".." ||
			strings.HasPrefix(clean, ".."+string(filepath.Separator)) || strings.HasPrefix(clean, "/") {
			return "", fmt.Errorf("plugin install: refusing unsafe path %q", name)
		}
		if filepath.Ext(clean) == ".toml" && !strings.Contains(clean, string(filepath.Separator)) {
			if manifestName != "" {
				return "", fmt.Errorf("plugin install: one manifest per install, got %q and %q", manifestName, clean)
			}
			manifestName = clean
		}
		names = append(names, clean)
	}
	if manifestName == "" {
		return "", fmt.Errorf("plugin install: a <name>.toml manifest is required")
	}
	name := strings.TrimSuffix(manifestName, ".toml")
	if name == "" || strings.HasPrefix(name, ".") {
		return "", fmt.Errorf("plugin install: bad manifest name %q", manifestName)
	}

	m.mu.Lock()
	disabled := m.disabled[name]
	m.mu.Unlock()
	if disabled {
		return "", fmt.Errorf("plugin install: %q is disabled by config ([plugins] disabled)", name)
	}

	// An existing file at any target path means the plugin (or something else)
	// is already installed under it — refuse rather than overwrite.
	for _, n := range names {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			return "", fmt.Errorf("plugin install: %s already exists in the user dir — remove or reload it first", n)
		}
	}

	// Stage, vet, then move into place: a rejected install leaves no trace.
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("plugin install: %w", err)
	}
	staging := filepath.Join(dir, ".install-"+hex.EncodeToString(b[:]))
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return "", err
	}
	cleanup := func() { os.RemoveAll(staging) } //nolint:errcheck
	defer cleanup()

	manifestPath := filepath.Join(staging, manifestName)
	if err := os.WriteFile(manifestPath, files[manifestName], 0o600); err != nil {
		return "", err
	}
	mf, err := LoadManifest(manifestPath)
	if err != nil {
		return "", fmt.Errorf("plugin install: %w", err)
	}
	for _, n := range names {
		if n == manifestName {
			continue
		}
		dst := filepath.Join(staging, n)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return "", err
		}
		mode := os.FileMode(0o600)
		if filepath.Clean(mf.Entrypoint) == n || filepath.Clean("./"+mf.Entrypoint) == n {
			mode = 0o755 // the entrypoint must be executable
		}
		if err := os.WriteFile(dst, files[n], mode); err != nil {
			return "", err
		}
	}

	// Vet like the boot load does: the binary must pass the MCP handshake
	// (the probe) and advertise no tool a running plugin already owns.
	bin := mf.Entrypoint
	if !filepath.IsAbs(bin) {
		bin = filepath.Join(staging, bin)
	}
	desc, err := ProbeTransport(bin, mf.Transport, m.envFor(name)...)
	if err != nil {
		return "", fmt.Errorf("plugin install: %q is not a valid MCP plugin: %w", name, err)
	}
	if owner, tool := m.firstToolCollision(desc.Tools); tool != "" {
		return "", fmt.Errorf("plugin install: tool %q already provided by %q", tool, owner)
	}

	// Move into place, then reload so the plugin starts and its tools are
	// granted to running sessions from their next turn (Q32).
	for _, n := range names {
		if err := os.Rename(filepath.Join(staging, n), filepath.Join(dir, n)); err != nil {
			return "", fmt.Errorf("plugin install: move %s: %w", n, err)
		}
	}
	os.Remove(staging) //nolint:errcheck // now empty
	m.loadUserPlugins(dir)

	st, ok := func() (UserPluginStatus, bool) {
		for _, s := range m.UserStatus() {
			if s.Name == name || s.Manifest == filepath.Join(dir, manifestName) {
				return s, true
			}
		}
		return UserPluginStatus{}, false
	}()
	if !ok {
		return name, fmt.Errorf("plugin install: %q was written but did not load (see `nine plugins`)", name)
	}
	if !st.Loaded {
		return name, fmt.Errorf("plugin install: %q did not load: %s", name, st.Err)
	}
	slog.Info("user plugin installed", "name", name, "tools", len(st.Tools))
	return name, nil
}
