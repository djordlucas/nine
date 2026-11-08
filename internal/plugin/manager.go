package plugin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"nine/internal/mcp"
)

// Plugin transports (the manifest's `transport` key, R-PLUG.1): the default is
// streamable HTTP on the per-plugin Unix socket (concurrent); stdio is the
// opt-in (serial). adr/modular_nine.md §9.6.
const (
	TransportHTTP  = "http"
	TransportStdio = "stdio"
)

// socketDir is a short fixed directory for plugin sockets. It must be short to
// stay under the macOS sun_path limit (104 chars); $TMPDIR on darwin overflows
// it. It is a directory and does not collide with the daemon's own socket file.
const socketDir = "/tmp/nine"

// socketReadyTimeout bounds how long Start waits for a plugin that is alive but
// has not yet listened. A plugin that *dies* during startup no longer waits this
// out — waitForSocket notices the exit and fails immediately with the cause — so
// this governs only the slow-but-healthy case.
//
// Measured on an idle machine: ~13ms for a warm binary, ~205ms for a
// freshly-built one, the difference being the first-execution cost the OS charges
// for a binary it has not seen before (code-signing assessment on darwin). The
// test suite pays that cost every run, since it rebuilds its fixture plugin, and
// does so while every other package is building and running in parallel.
//
// 3s was ~15x the observed worst case and still produced a recurring flake
// (TestRegisterPlugin), which says the tail is much longer than the median under
// load. Raising it is close to free: this is a poll that returns the moment the
// dial succeeds, not a sleep, so a healthy plugin is unaffected and only a
// genuinely stuck one waits longer before being declared dead.
const socketReadyTimeout = 30 * time.Second

// Plugin is a running plugin process with its advertised tools.
type Plugin struct {
	Name string
	// client is the MCP client to the plugin process: streamable HTTP on the
	// plugin's Unix socket, or stdio for a stdio-transport plugin. MCP is the
	// only wire protocol across a process boundary (adr/modular_nine.md).
	client *mcpClient
	Tools  []ToolDefinition
	// User marks a plugin loaded from the operator's plugins directory rather
	// than a built-in. Reload stops and re-discovers only User plugins.
	User bool

	// AsyncJobs mirrors the plugin's describe flag: it can run detached work and
	// answer job_status / job_cancel. The daemon refuses a job_id from a plugin
	// with this false — fail-closed against version skew.
	AsyncJobs bool

	// MaxConcurrent mirrors what the plugin declared (R-PLUG.8): 0 is unbounded,
	// a finite value means its handlers are not safe to run in parallel. The
	// transport is already bounded by it; keeping it here makes the declaration
	// observable rather than only enforced, which is what lets a caller or a test
	// check what a plugin actually promised.
	MaxConcurrent int

	// cacheDir is the plugin's scratch directory (docs/plugin-capabilities.md §4),
	// handed over as NINE_PLUGIN_CACHE_DIR. Empty when no cache root is configured.
	// cacheEphemeral marks it for removal on Stop; a persistent dir is left alone.
	cacheDir       string
	cacheEphemeral bool
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

	// disabled names plugins the operator has switched off ([plugins].disabled,
	// R-PLUG.14). disabledSkipped records the default plugins actually refused at
	// boot, so `nine plugins` can show them as off rather than silently missing.
	// User plugins are not recorded here — loadUserPlugins already keeps its own
	// per-plugin status with the reason.
	disabled        map[string]bool
	disabledSkipped []string

	// builtinBin overrides the executable re-exec'd for built-in plugins
	// (StartBuiltin). Empty means os.Executable(), which is correct in the
	// daemon and wrong under `go test`, where the running binary is the test
	// binary; tests set this to a built nine. See SetBuiltinBinary.
	builtinBin string

	// pluginEnv resolves a plugin's extra spawn environment (built-in defaults +
	// operator settings) by name. Set by the daemon via SetPluginEnv so this
	// package stays config-agnostic (docs/plugin-capabilities.md §3). Nil means no
	// extra env — the pre-settings behaviour, used by tests and probes.
	pluginEnv func(name string) []string

	// cacheRoot is the directory under which per-plugin cache dirs are created
	// (docs/plugin-capabilities.md §4); persistCache reports whether a given
	// plugin's dir survives restarts. Both set by the daemon via SetCacheConfig;
	// an empty cacheRoot disables cache dirs entirely (tests, probes).
	cacheRoot    string
	persistCache func(name string) bool

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

// ErrPluginDisabled is returned by the start path for a plugin the operator has
// switched off. Callers that treat a missing plugin as tolerable (TryStart,
// TryStartBuiltin, loadUserPlugins) report it as a deliberate choice rather than
// a failure.
var ErrPluginDisabled = errors.New("plugin disabled in [plugins].disabled")

// SetDisabled installs the set of plugin names that must never start
// (typically config.Plugins.Disabled). Names are wire names — `shell`,
// `browser`, a user plugin's manifest name — and apply to every start path.
func (m *Manager) SetDisabled(names []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.disabled = make(map[string]bool, len(names))
	for _, n := range names {
		m.disabled[n] = true
	}
}

// IsDisabled reports whether name is switched off by config.
func (m *Manager) IsDisabled(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.disabled[name]
}

// DisabledSkipped returns the default plugins that were refused at boot because
// they are disabled, for the `nine plugins` roster.
func (m *Manager) DisabledSkipped() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.disabledSkipped))
	copy(out, m.disabledSkipped)
	return out
}

// UnmatchedDisabled returns the names in the disabled set that never actually
// refused anything, once loading is done. Every such name is a mistake with a
// silent and dangerous failure mode: `disabled = ["shel"]` withholds nothing and
// leaves `shell` — arbitrary command execution — running, with no error, no
// roster entry, and an operator who believes otherwise. Names cannot be
// validated when config is read, because a user plugin's name is not known
// until its directory is scanned, so this is checked after boot instead.
func (m *Manager) UnmatchedDisabled() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	matched := make(map[string]bool, len(m.disabledSkipped))
	for _, n := range m.disabledSkipped {
		matched[n] = true
	}
	var out []string
	for name := range m.disabled {
		if !matched[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// noteDisabledSkip records a refused default plugin once, for the roster.
func (m *Manager) noteDisabledSkip(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, n := range m.disabledSkipped {
		if n == name {
			return
		}
	}
	m.disabledSkipped = append(m.disabledSkipped, name)
}

// SetBuiltinBinary overrides the nine executable used to spawn built-in plugins.
// The daemon never needs it — os.Executable() is the nine binary there. Tests do:
// their os.Executable() is the test binary, which has no `plugin serve`
// subcommand, so they build a nine and point the manager at it.
func (m *Manager) SetBuiltinBinary(path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.builtinBin = path
}

// SetCacheConfig installs the cache-dir root and the per-plugin persistence
// resolver (typically config.Config.PluginCacheRoot and PluginPersistCache).
// With an empty root, plugins start without a cache dir — the behaviour tests
// and probes rely on.
func (m *Manager) SetCacheConfig(root string, persist func(name string) bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cacheRoot = root
	m.persistCache = persist
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

// Start spawns a native plugin binary that speaks MCP streamable HTTP on a
// per-plugin Unix socket — the default plugin transport (R-PLUG.16) — and
// returns the running Plugin. The plugin listens on the socket named by
// NINE_PLUGIN_SOCKET; the manager handshakes MCP on it (initialize → tools/list).
func (m *Manager) Start(binaryPath string, extraEnv ...string) (*Plugin, error) {
	return m.startSocket(binaryLaunch(binaryPath), extraEnv...)
}

// StartStdio spawns a native plugin binary that speaks MCP over its stdio pipes
// (the stdio opt-in, adr/modular_nine.md §9.6). No socket is allocated; the
// transport is serial, so the plugin is registered with max_concurrent 1.
func (m *Manager) StartStdio(binaryPath string, extraEnv ...string) (*Plugin, error) {
	return m.startStdio(binaryLaunch(binaryPath), extraEnv...)
}

// StartMCPBuiltin spawns a built-in plugin — one whose handlers are linked into the
// nine binary (internal/builtins) — by re-executing that binary as
// `nine mcp serve <name>` (adr/modular_nine.md). Everything past the spawn is
// identical to Start: a separate process, its own socket, the sanitized
// environment, a cache dir, and the MCP handshake in spawnAndDescribe. Only the
// subcommand differs.
func (m *Manager) StartMCPBuiltin(name string, extraEnv ...string) (*Plugin, error) {
	bin, err := m.builtinBinary()
	if err != nil {
		return nil, err
	}
	return m.startSocket(mcpBuiltinLaunch(bin, name), extraEnv...)
}

// TryStartMCPBuiltin is TryStart for an MCP-serving built-in: it logs and
// returns nil rather than failing the daemon's boot.
func (m *Manager) TryStartMCPBuiltin(name string, extraEnv ...string) *Plugin {
	if m.IsDisabled(name) {
		m.noteDisabledSkip(name)
		slog.Info("MCP built-in plugin disabled by config, not starting", "name", name)
		return nil
	}
	p, err := m.StartMCPBuiltin(name, extraEnv...)
	if err != nil {
		slog.Warn("MCP built-in plugin start failed", "name", name, "err", err)
		return nil
	}
	return p
}

// startSocket is the unix-socket-HTTP start path: spawn, wait for the socket,
// MCP-handshake on it, track. Every socket start path (Start, StartMCPBuiltin)
// funnels through here, so a disabled plugin cannot be spawned by any caller —
// no process, no socket, no cache dir (R-PLUG.14). External MCP servers are
// covered too: StartMCPServer checks the same set by instance name (R-PLUG.15).
func (m *Manager) startSocket(l launch, extraEnv ...string) (*Plugin, error) {
	name := l.name

	if m.IsDisabled(name) {
		return nil, fmt.Errorf("%q: %w", name, ErrPluginDisabled)
	}

	env := append([]string{}, m.env...)
	env = append(env, extraEnv...)

	// Allocate the plugin's cache dir and hand it over as Nine-owned env vars.
	// They go last so Nine's values win over anything inherited; an operator
	// cannot set them via [plugin.<name>.settings] (they are reserved).
	cacheDir, ephemeral, err := m.allocCacheDir(name)
	if err != nil {
		return nil, err
	}
	if cacheDir != "" {
		env = append(env, "NINE_PLUGIN_CACHE_DIR="+cacheDir, "NINE_PLUGIN_CACHE_PERSISTENT="+persistentEnv(!ephemeral))
	}

	cmd, watch, socketPath, desc, mconn, err := spawnAndHandshake(l, env)
	if err != nil {
		if ephemeral && cacheDir != "" {
			os.RemoveAll(cacheDir) //nolint:errcheck // spawn failed; reclaim the dir we just made
		}
		return nil, err
	}

	p := &Plugin{Name: name, client: newMCPClient(name, socketPath, cmd, watch, mconn),
		Tools: desc.Tools, MaxConcurrent: desc.MaxConcurrent,
		cacheDir: cacheDir, cacheEphemeral: ephemeral}
	m.track(p)
	slog.Info("plugin started", "name", name, "transport", "unix-socket-http", "tools", len(desc.Tools), "max_concurrent", desc.MaxConcurrent)
	return p, nil
}

// startStdio is the stdio start path: spawn with pipes, MCP-handshake over
// stdin/stdout, track. No socket is allocated; the transport is serial, so the
// plugin is registered with max_concurrent 1 (R-PLUG.8). A plugin that dies
// before answering the handshake fails the start at once ("MCP server closed
// stdout"), which is the stdio form of the dead-before-listening check.
func (m *Manager) startStdio(l launch, extraEnv ...string) (*Plugin, error) {
	name := l.name

	if m.IsDisabled(name) {
		return nil, fmt.Errorf("%q: %w", name, ErrPluginDisabled)
	}

	env := append([]string{}, sanitizedHostEnv()...)
	env = append(env, m.env...)
	env = append(env, extraEnv...)

	cacheDir, ephemeral, err := m.allocCacheDir(name)
	if err != nil {
		return nil, err
	}
	if cacheDir != "" {
		env = append(env, "NINE_PLUGIN_CACHE_DIR="+cacheDir, "NINE_PLUGIN_CACHE_PERSISTENT="+persistentEnv(!ephemeral))
	}

	ctx, cancel := context.WithTimeout(context.Background(), mcp.HandshakeTimeout())
	defer cancel()

	conn, err := mcp.DialStdioEnv(l.path, l.args, env, mcp.WithSpillDir(cacheDir)) //nolint:gosec // path is a Nine-resolved plugin binary or the nine binary itself
	if err != nil {
		if ephemeral && cacheDir != "" {
			os.RemoveAll(cacheDir) //nolint:errcheck
		}
		return nil, fmt.Errorf("start plugin: %w", err)
	}
	tools, err := mcp.Handshake(ctx, conn, mcp.ProtocolVersionStdio, "nine", "unknown")
	if err != nil {
		conn.Stop() //nolint:errcheck // best-effort teardown on a failed handshake
		if ephemeral && cacheDir != "" {
			os.RemoveAll(cacheDir) //nolint:errcheck
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("handshake timed out after %s: %w", mcp.HandshakeTimeout(), ctxErr)
		}
		return nil, err
	}

	p := &Plugin{Name: name, client: newMCPClient(name, "", nil, nil, conn),
		Tools: mcpToolsToDefs(tools), MaxConcurrent: 1,
		cacheDir: cacheDir, cacheEphemeral: ephemeral}
	go func() {
		<-conn.Exited()
		if conn.StoppedDeliberately() {
			return
		}
		slog.Warn("plugin process exited; its tools will fail until the daemon restarts", "name", name)
	}()
	m.track(p)
	slog.Info("plugin started", "name", name, "transport", "stdio", "tools", len(p.Tools), "max_concurrent", 1)
	return p, nil
}

// launch is how to run one plugin process: the executable plus any leading
// arguments, and the wire name the resulting plugin is known by. A standalone
// plugin binary is named after its file and takes no arguments; a built-in is
// the nine binary run as `nine mcp serve <name>`, so its name cannot be
// derived from the path (that would name every built-in "nine").
type launch struct {
	name string
	path string
	args []string
}

// binaryLaunch runs a standalone plugin binary — a user plugin.
func binaryLaunch(binaryPath string) launch {
	return launch{name: filepath.Base(binaryPath), path: binaryPath}
}

// mcpBuiltinLaunch runs a built-in that serves MCP out of the nine binary at
// nineBin: `nine mcp serve <name>` (adr/modular_nine.md). The wire name is the
// built-in's own, exactly as in binaryLaunch.
func mcpBuiltinLaunch(nineBin, name string) launch {
	return launch{name: name, path: nineBin, args: []string{"mcp", "serve", name}}
}

// MCPInstanceName is the plugin name an MCP server runs under. One function so
// the daemon that starts it, the roster that lists it, and the operator who
// disables it all agree on the spelling — `[plugins].disabled = ["mcp:github"]`
// has to match what boot registered, and a convention duplicated across three
// call sites is a convention that drifts.
func MCPInstanceName(server string) string { return "mcp:" + server }

// builtinBinary resolves the nine executable to re-exec for built-in plugins.
// It is the running binary unless a test has overridden it via
// SetBuiltinBinary — under `go test` the running binary is the test binary,
// which knows nothing about serving plugins.
func (m *Manager) builtinBinary() (string, error) {
	m.mu.Lock()
	override := m.builtinBin
	m.mu.Unlock()
	if override != "" {
		return override, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate nine binary for built-in plugin: %w", err)
	}
	return exe, nil
}

// spawnAndHandshake spawns l on a fresh Unix socket, waits for it to
// listen, and completes the MCP handshake on it (initialize →
// notifications/initialized → tools/list). It is the shared front half of both
// Start (which keeps the process running) and Probe (which stops it). On any
// failure it tears the process and socket down and returns the error; on success
// the caller owns cmd and must eventually stop it and remove socketPath, and owns
// mconn. env is the full extra environment (NINE_PLUGIN_SOCKET is appended here).
func spawnAndHandshake(l launch, env []string) (*exec.Cmd, *procWatch, string, DescribeResult, mcp.Conn, error) {
	if err := os.MkdirAll(socketDir, 0o700); err != nil {
		return nil, nil, "", DescribeResult{}, nil, fmt.Errorf("create socket dir: %w", err)
	}
	socketPath, err := allocSocketPath(l.name)
	if err != nil {
		return nil, nil, "", DescribeResult{}, nil, err
	}

	env = append([]string{}, env...)
	env = append(env, "NINE_PLUGIN_SOCKET="+socketPath)

	cmd := exec.Command(l.path, l.args...) //nolint:gosec // path is a Nine-resolved plugin binary or the nine binary itself
	cmd.Env = append(sanitizedHostEnv(), env...)
	cmd.Stderr = os.Stderr // surface plugin startup/listen errors
	if err := cmd.Start(); err != nil {
		return nil, nil, "", DescribeResult{}, nil, fmt.Errorf("start plugin: %w", err)
	}

	// Claim the process's single Wait up front, so startup can tell a plugin that
	// died from one that is merely slow, and so stop has a result to consume.
	watch := watchProcess(cmd)

	cleanup := func() {
		cmd.Process.Kill() //nolint:errcheck // best-effort
		watch.wait()       //nolint:errcheck // reaped by watchProcess
		os.Remove(socketPath)
	}

	if err := waitForSocket(socketPath, socketReadyTimeout, watch); err != nil {
		cleanup()
		return nil, nil, "", DescribeResult{}, nil, err
	}

	// The handshake decides nothing any more — MCP is the only wire a plugin
	// speaks (R-PLUG.1). Concurrency comes from the transport: unix-socket HTTP
	// is concurrent (0 = unbounded); stdio would be serial and is handled by
	// startStdio, which sets max_concurrent 1.
	desc, mconn, err := handshakeSocket(socketPath)
	if err != nil {
		cleanup()
		return nil, nil, "", DescribeResult{}, nil, err
	}

	return cmd, watch, socketPath, desc, mconn, nil
}

// handshakeSocket completes the MCP handshake with the freshly-listening plugin
// on socketPath and returns its advertised tools with bare names — first-party
// tools are never prefixed (adr/modular_nine.md §9.9); only MCP-roster servers
// carry `<server>__` (R-PLUG.15).
func handshakeSocket(socketPath string) (DescribeResult, mcp.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), mcp.HandshakeTimeout())
	defer cancel()
	c, tools, err := mcp.ConnectSocket(ctx, socketPath, "")
	if err != nil {
		return DescribeResult{}, nil, err
	}
	// Unix-socket HTTP is concurrent, so max_concurrent 0 (unbounded) is the
	// honest declaration; async_jobs is false because jobs ride a _meta
	// extension the manager does not consume yet (§2).
	return DescribeResult{Tools: mcpToolsToDefs(tools)}, c, nil
}

// mcpToolsToDefs converts MCP tool listings to the plugin contract's shape. The
// input schema is carried through unchanged; only the envelope key differs.
func mcpToolsToDefs(tools []mcp.Tool) []ToolDefinition {
	defs := make([]ToolDefinition, 0, len(tools))
	for _, t := range tools {
		defs = append(defs, ToolDefinition{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	return defs
}

// ProbeTransport runs binaryPath through the same handshake its Start path uses
// — spawn, then the MCP handshake over the transport named (TransportHTTP: socket
// on NINE_PLUGIN_SOCKET; TransportStdio: stdio pipes) — and stops the process. It
// reports whether the binary is a valid Nine plugin (an MCP server) and, when it
// is, the tools it advertises, without tracking anything or leaving a process
// running. It backs `nine plugin validate` and the pre-load vetting of user
// plugins. env is passed straight through as extra spawn env.
func ProbeTransport(binaryPath, transport string, env ...string) (DescribeResult, error) {
	// Validation must never create or touch persistent state, so Probe always
	// hands over a throwaway ephemeral cache dir removed with the process, whatever
	// the plugin's persist_cache setting (docs/plugin-capabilities.md §4).
	if cacheDir, err := os.MkdirTemp("", "nine-probe-cache-"); err == nil {
		defer os.RemoveAll(cacheDir) //nolint:errcheck // best-effort
		env = append(append([]string{}, env...),
			"NINE_PLUGIN_CACHE_DIR="+cacheDir,
			"NINE_PLUGIN_CACHE_PERSISTENT=0")
	}

	if transport == TransportStdio {
		full := append(append([]string{}, sanitizedHostEnv()...), env...)
		ctx, cancel := context.WithTimeout(context.Background(), mcp.HandshakeTimeout())
		defer cancel()
		conn, err := mcp.DialStdioEnv(binaryPath, nil, full)
		if err != nil {
			return DescribeResult{}, err
		}
		tools, err := mcp.Handshake(ctx, conn, mcp.ProtocolVersionStdio, "nine", "unknown")
		conn.Stop() //nolint:errcheck // Probe is done with the process
		if err != nil {
			return DescribeResult{}, err
		}
		return DescribeResult{Tools: mcpToolsToDefs(tools), MaxConcurrent: 1}, nil
	}

	cmd, watch, socketPath, desc, mconn, err := spawnAndHandshake(binaryLaunch(binaryPath), env)
	if err != nil {
		return DescribeResult{}, err
	}
	mconn.Stop()       //nolint:errcheck // Probe is done with the connection; the process is killed below
	cmd.Process.Kill() //nolint:errcheck // best-effort
	watch.wait()       //nolint:errcheck // reaped by watchProcess
	os.Remove(socketPath)
	return desc, nil
}

// ProbeAny probes a bare binary without a manifest to tell its transport from:
// unix-socket HTTP first (the default), stdio as the fallback.
func ProbeAny(binaryPath string, env ...string) (DescribeResult, error) {
	if desc, err := ProbeTransport(binaryPath, TransportHTTP, env...); err == nil {
		return desc, nil
	}
	return ProbeTransport(binaryPath, TransportStdio, env...)
}

// allocSocketPath returns a unique short socket path under socketDir.
func allocSocketPath(name string) (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("alloc socket path: %w", err)
	}
	return filepath.Join(socketDir, fmt.Sprintf("%s.%s.sock", socketSafe(name), hex.EncodeToString(b[:]))), nil
}

// socketSafe reduces a plugin name to characters safe in a filename. Plugin
// names became more than a bare word when instances arrived (`mcp:github`), and
// a name is not a path — a separator in one would silently place the socket
// somewhere other than socketDir, which the macOS sun_path limit exists to keep
// short and predictable.
func socketSafe(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, name)
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

// TryStart starts a plugin shipped as its own binary under [plugins].bin,
// logging and returning nil rather than failing the daemon's boot. Built-in
// plugins use TryStartBuiltin instead — they have no binary to look up.
func (m *Manager) TryStart(name string, extraEnv ...string) *Plugin {
	// Checked before the binary lookup so a disabled plugin reports as disabled
	// rather than as a missing binary — two very different things for an
	// operator reading the log.
	if m.IsDisabled(name) {
		m.noteDisabledSkip(name)
		slog.Info("plugin disabled by config, not starting", "name", name)
		return nil
	}
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

// TryStartBuiltin is gone with the legacy envelope (adr/modular_nine.md P4):
// every built-in serves MCP now, so boot uses TryStartMCPBuiltin.

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

// JobStatus asks a plugin for the status of one of its jobs
// (docs/plugin-capabilities.md §5). An unknown job id is not an RPC error: the
// plugin answers a failed status, so a daemon that lost track cannot wedge.
func (m *Manager) JobStatus(ctx context.Context, p *Plugin, pluginJobID string) (JobStatus, error) {
	raw, err := p.client.call(ctx, "plugin.job_status", map[string]string{"job_id": pluginJobID})
	if err != nil {
		return JobStatus{}, err
	}
	var st JobStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return JobStatus{}, fmt.Errorf("unmarshal job status: %w", err)
	}
	return st, nil
}

// JobCancel best-effort cancels one of a plugin's jobs.
func (m *Manager) JobCancel(ctx context.Context, p *Plugin, pluginJobID string) error {
	_, err := p.client.call(ctx, "plugin.job_cancel", map[string]string{"job_id": pluginJobID})
	return err
}

// PluginByName returns the running plugin with the given name, or (nil, false).
func (m *Manager) PluginByName(name string) (*Plugin, bool) {
	for _, p := range m.Running() {
		if p.Name == name {
			return p, true
		}
	}
	return nil, false
}

// Stop gracefully shuts down the plugin process.
func (m *Manager) Stop(p *Plugin) error {
	err := p.client.stop()
	if err == nil {
		slog.Debug("plugin stopped")
	}
	// The process is down (client.stop waited on it), so its scratch dir is now
	// safe to reclaim. Persistent dirs are the operator's to keep.
	if p.cacheEphemeral && p.cacheDir != "" {
		os.RemoveAll(p.cacheDir) //nolint:errcheck // best-effort scratch cleanup
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
