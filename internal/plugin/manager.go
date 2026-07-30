package plugin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// socketDir is a short fixed directory for plugin sockets. It must be short to
// stay under the macOS sun_path limit (104 chars); $TMPDIR on darwin overflows
// it. It is a directory and does not collide with the daemon's own socket file.
const socketDir = "/tmp/nine"

// socketReadyTimeout bounds how long Start waits for a spawned plugin to start
// listening before giving up.
const socketReadyTimeout = time.Second

// Plugin is a running plugin process with its advertised tools.
type Plugin struct {
	Name   string
	client pluginClient
	Tools  []ToolDefinition
	// User marks a plugin loaded from the operator's plugins directory rather
	// than a built-in. Reload stops and re-discovers only User plugins.
	User bool
}

// UserPluginStatus records the outcome of trying to load one operator plugin
// from the scan directory, for `nine plugins` reporting. Loaded plugins carry
// their advertised tool names; a skipped one carries the reason in Err.
type UserPluginStatus struct {
	Name     string   `json:"name"`
	Manifest string   `json:"manifest"`
	Loaded   bool     `json:"loaded"`
	Tools    []string `json:"tools,omitempty"`
	Err      string   `json:"error,omitempty"`
}

// Manager spawns and manages plugin processes.
type Manager struct {
	mu      sync.Mutex
	running []*Plugin

	env       []string
	pluginBin string

	// pluginEnv resolves a plugin's extra spawn environment (built-in defaults +
	// operator settings) by name. Set by the daemon via SetPluginEnv so this
	// package stays config-agnostic (docs/plugin-capabilities.md §3). Nil means no
	// extra env — the pre-settings behaviour, used by tests and probes.
	pluginEnv func(name string) []string

	userDir    string
	userStatus []UserPluginStatus
}

// NewManager creates a Manager that passes nineBin to each plugin via the
// NINE_BIN environment variable (appended to the inherited OS environment).
func NewManager(nineBin string) *Manager {
	return &Manager{
		pluginBin: nineBin,
		env:       []string{"NINE_BIN=" + nineBin},
	}
}

// SetPluginEnv installs the resolver used to look up a plugin's extra spawn
// environment by name (typically config.Config.PluginEnvs). It is optional: with
// no resolver installed, plugins start with only the manager's own env, which is
// the behaviour tests and Probe callers rely on.
func (m *Manager) SetPluginEnv(fn func(name string) []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pluginEnv = fn
}

// envFor returns the extra environment for the named plugin via the installed
// resolver, or nil when none is set.
func (m *Manager) envFor(name string) []string {
	m.mu.Lock()
	fn := m.pluginEnv
	m.mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn(name)
}

// PluginBin returns the full path to the named plugin binary.
func (m *Manager) getPluginBinaryPath(name string) string {
	if m.pluginBin != "" {
		return filepath.Join(m.pluginBin, name)
	}
	return filepath.Join("/data/bin", name)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Start spawns a native Nine plugin binary with optional extra env vars over the
// HTTP-on-a-Unix-socket transport, calls plugin.describe, and returns the running
// Plugin. The plugin listens on the socket named by NINE_PLUGIN_SOCKET; the daemon
// dials it.
func (m *Manager) Start(binaryPath string, extraEnv ...string) (*Plugin, error) {
	name := filepath.Base(binaryPath)

	env := append([]string{}, m.env...)
	env = append(env, extraEnv...)

	cmd, socketPath, desc, err := spawnAndDescribe(binaryPath, env)
	if err != nil {
		return nil, err
	}

	c := newHTTPClient(name, socketPath, cmd, desc.MaxConcurrent)
	p := &Plugin{Name: name, client: c, Tools: desc.Tools}
	m.track(p)
	slog.Info("plugin started", "name", name, "tools", len(desc.Tools), "max_concurrent", desc.MaxConcurrent)
	return p, nil
}

// spawnAndDescribe spawns binaryPath on a fresh Unix socket, waits for it to
// listen, calls plugin.describe, and checks the reported protocol version. It is
// the shared front half of both Start (which keeps the process running) and
// Probe (which stops it). On any failure it tears the process and socket down and
// returns the error; on success the caller owns cmd and must eventually stop it
// and remove socketPath. env is the full extra environment (NINE_PLUGIN_SOCKET is
// appended here).
func spawnAndDescribe(binaryPath string, env []string) (*exec.Cmd, string, DescribeResult, error) {
	name := filepath.Base(binaryPath)

	if err := os.MkdirAll(socketDir, 0o700); err != nil {
		return nil, "", DescribeResult{}, fmt.Errorf("create socket dir: %w", err)
	}
	socketPath, err := allocSocketPath(name)
	if err != nil {
		return nil, "", DescribeResult{}, err
	}

	env = append([]string{}, env...)
	env = append(env, "NINE_PLUGIN_SOCKET="+socketPath)

	cmd := exec.Command(binaryPath)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stderr = os.Stderr // surface plugin startup/listen errors
	if err := cmd.Start(); err != nil {
		return nil, "", DescribeResult{}, fmt.Errorf("start plugin: %w", err)
	}

	cleanup := func() {
		cmd.Process.Kill() //nolint:errcheck // best-effort
		cmd.Wait()         //nolint:errcheck
		os.Remove(socketPath)
	}

	if err := waitForSocket(socketPath, socketReadyTimeout); err != nil {
		cleanup()
		return nil, "", DescribeResult{}, err
	}

	// Read max_concurrent on a throwaway client first: net/http forbids mutating
	// a Transport after a request is issued on it, so the real client's transport
	// is built afterwards with MaxConnsPerHost already set.
	desc, err := describeOverSocket(context.Background(), socketPath)
	if err != nil {
		cleanup()
		return nil, "", DescribeResult{}, fmt.Errorf("plugin.describe: %w", err)
	}

	if err := checkProtocolVersion(name, desc.ProtocolVersion); err != nil {
		cleanup()
		return nil, "", DescribeResult{}, err
	}

	return cmd, socketPath, desc, nil
}

// Probe runs binaryPath through the same handshake Start uses — spawn, wait for
// the socket, plugin.describe, protocol-version check — then stops the process.
// It reports whether the binary is a valid Nine plugin and, when it is, the tools
// it advertises, without tracking anything or leaving a process running. It backs
// `nine plugin validate` and the pre-load vetting of user plugins. env is passed
// straight through as the process environment (e.g. the manager's NINE_BIN).
func Probe(binaryPath string, env ...string) (DescribeResult, error) {
	cmd, socketPath, desc, err := spawnAndDescribe(binaryPath, env)
	if err != nil {
		return DescribeResult{}, err
	}
	cmd.Process.Kill() //nolint:errcheck // best-effort
	cmd.Wait()         //nolint:errcheck
	os.Remove(socketPath)
	return desc, nil
}

// checkProtocolVersion rejects a plugin whose wire-contract version the daemon
// does not support. The daemon supports exactly one version today, so any
// mismatch is fatal; widen this if it ever needs to support a range. An absent
// version (0) means the plugin predates protocol versioning.
func checkProtocolVersion(name string, got int) error {
	if got == ProtocolVersion {
		return nil
	}
	if got == 0 {
		return fmt.Errorf("plugin %q reports no protocol version; it predates plugin protocol v%d — rebuild it against the current Nine", name, ProtocolVersion)
	}
	return fmt.Errorf("plugin %q speaks protocol v%d but this daemon speaks v%d — rebuild the plugin (or upgrade Nine)", name, got, ProtocolVersion)
}

// allocSocketPath returns a unique short socket path under socketDir.
func allocSocketPath(name string) (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("alloc socket path: %w", err)
	}
	return filepath.Join(socketDir, fmt.Sprintf("%s.%s.sock", name, hex.EncodeToString(b[:]))), nil
}

// describeOverSocket calls plugin.describe on a one-shot client whose idle
// connections are dropped immediately after.
func describeOverSocket(ctx context.Context, socketPath string) (DescribeResult, error) {
	hc := &http.Client{Transport: &http.Transport{DialContext: dialUnix(socketPath)}}
	defer hc.CloseIdleConnections()

	raw, err := postRPC(ctx, hc, "plugin.describe", struct{}{})
	if err != nil {
		return DescribeResult{}, err
	}
	var desc DescribeResult
	if err := json.Unmarshal(raw, &desc); err != nil {
		return DescribeResult{}, fmt.Errorf("unmarshal describe result: %w", err)
	}
	return desc, nil
}

func (m *Manager) track(p *Plugin) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.running = append(m.running, p)
}

func (m *Manager) untrack(p *Plugin) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, r := range m.running {
		if r == p {
			m.running = append(m.running[:i], m.running[i+1:]...)
			return
		}
	}
}

// Running returns a snapshot of all currently running plugins.
func (m *Manager) Running() []*Plugin {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Plugin, len(m.running))
	copy(out, m.running)
	return out
}

// StopAll stops all tracked plugins, accumulating errors.
func (m *Manager) StopAll() error {
	var errs []error
	for _, p := range m.Running() {
		if err := m.Stop(p); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) TryStart(name string, extraEnv ...string) *Plugin {
	bin := m.getPluginBinaryPath(name)
	if !fileExists(bin) {
		slog.Warn("binary to start does not exist", "name", name, "path", bin)
		return nil
	}
	p, err := m.Start(bin, extraEnv...)
	if err != nil {
		slog.Warn("plugin start failed", "name", name, "err", err)
		return nil
	}

	return p
}

// Call invokes a tool on a running plugin and returns the result. The context is
// carried to the plugin (HTTP request for native plugins) for cancellation and
// deadlines, e.g. task_timeout.
func (m *Manager) Call(ctx context.Context, p *Plugin, toolName string, args json.RawMessage) (CallResult, error) {
	result, err := p.client.call(ctx, "plugin.call", CallRequest{Tool: toolName, Args: args})
	if err != nil {
		return CallResult{}, err
	}

	var cr CallResult
	if err := json.Unmarshal(result, &cr); err != nil {
		return CallResult{}, fmt.Errorf("unmarshal call result: %w", err)
	}
	return cr, nil
}

// Stop gracefully shuts down the plugin process.
func (m *Manager) Stop(p *Plugin) error {
	err := p.client.stop()
	if err == nil {
		slog.Debug("plugin stopped")
	}
	m.untrack(p)
	return err
}

// ListRunning returns the names of all currently running plugins.
func (m *Manager) ListRunning() []string {
	plugins := m.Running()
	names := make([]string, len(plugins))
	for i, p := range plugins {
		names[i] = p.Name
	}
	return names
}
