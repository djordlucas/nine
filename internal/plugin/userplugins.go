package plugin

import (
	"fmt"
	"log/slog"
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
//  2. The binary is probed through the plugin handshake; a non-plugin (or a
//     protocol-version mismatch) is recorded as skipped.
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

		desc, err := Probe(d.BinaryPath, m.env...)
		if err != nil {
			st.Err = err.Error()
			slog.Error("skipping user plugin: not a valid plugin", "name", d.Name, "path", d.BinaryPath, "err", err)
			status = append(status, st)
			continue
		}

		if owner, tool := m.firstToolCollision(desc.Tools); tool != "" {
			st.Err = fmt.Sprintf("tool %q already provided by %q", tool, owner)
			slog.Error("skipping user plugin: tool collision", "name", d.Name, "tool", tool, "owner", owner)
			status = append(status, st)
			continue
		}

		p, err := m.Start(d.BinaryPath)
		if err != nil {
			st.Err = err.Error()
			slog.Error("skipping user plugin: start failed", "name", d.Name, "path", d.BinaryPath, "err", err)
			status = append(status, st)
			continue
		}
		p.User = true

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
