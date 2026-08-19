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
	"sort"
	"strings"
	"sync"
	"time"
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
	// client is the concrete HTTP-over-Unix-socket client, not an interface.
	// The interface existed only because MCP servers spoke stdio from inside the
	// manager; now that MCP is reached through a plugin like anything else
	// (R-PLUG.15), there is one transport and nothing to abstract over.
	client *httpClient
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

// Start spawns a native Nine plugin binary with optional extra env vars over the
// HTTP-on-a-Unix-socket transport, calls plugin.describe, and returns the running
// Plugin. The plugin listens on the socket named by NINE_PLUGIN_SOCKET; the daemon
// dials it.
func (m *Manager) Start(binaryPath string, extraEnv ...string) (*Plugin, error) {
	return m.start(binaryLaunch(binaryPath), extraEnv...)
}

// StartBuiltin spawns a built-in plugin — one whose handlers are linked into the
// nine binary (internal/builtins) — by re-executing that binary as
// `nine plugin serve <name>`. It is the same mechanism the client already uses
// to auto-start the daemon (spec/contracts/wire-protocol.md R-PROTO.7), applied
// to plugins.
//
// Everything past the spawn is identical to Start: a separate process, its own
// socket, the sanitized environment, a cache dir, describe, and the
// protocol-version check. Only the artifact differs — there is no per-plugin
// binary to ship or to fall out of sync with the daemon.
func (m *Manager) StartBuiltin(name string, extraEnv ...string) (*Plugin, error) {
	bin, err := m.builtinBinary()
	if err != nil {
		return nil, err
	}
	return m.start(builtinLaunch(bin, name), extraEnv...)
}

// StartBuiltinInstance starts the built-in `builtin` under the wire name
// `instance`. It is how one built-in backs several plugins: the `mcp` bridge
// runs once per configured server (R-PLUG.15), each instance a separate process
// with its own tools, roster row, and failure domain. Everything else — the
// sanitized environment, cache dir, describe, disable check — is identical to
// StartBuiltin, which is the same call with instance == builtin.
func (m *Manager) StartBuiltinInstance(builtin, instance string, extraEnv ...string) (*Plugin, error) {
	bin, err := m.builtinBinary()
	if err != nil {
		return nil, err
	}
	return m.start(builtinInstanceLaunch(bin, builtin, instance), extraEnv...)
}

// TryStartBuiltinInstance is StartBuiltinInstance with TryStart's tolerance: it
// logs and returns nil rather than failing the daemon's boot, so one unreachable
// MCP server does not stop Nine from starting.
func (m *Manager) TryStartBuiltinInstance(builtin, instance string, extraEnv ...string) *Plugin {
	if m.IsDisabled(instance) {
		m.noteDisabledSkip(instance)
		slog.Info("plugin disabled by config, not starting", "name", instance)
		return nil
	}
	p, err := m.StartBuiltinInstance(builtin, instance, extraEnv...)
	if err != nil {
		slog.Warn("plugin start failed", "name", instance, "builtin", builtin, "err", err)
		return nil
	}
	return p
}

func (m *Manager) start(l launch, extraEnv ...string) (*Plugin, error) {
	name := l.name

	// Every start path funnels through here, so a disabled plugin cannot be
	// spawned by any caller — including a future one that forgets to ask. No
	// process, no socket, no cache dir. MCP servers are covered too: each is a
	// bridge plugin started through StartBuiltinInstance (R-PLUG.15), not a
	// separate spawn path of its own.
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

	cmd, watch, socketPath, desc, err := spawnAndDescribe(l, env)
	if err != nil {
		if ephemeral && cacheDir != "" {
			os.RemoveAll(cacheDir) //nolint:errcheck // spawn failed; reclaim the dir we just made
		}
		return nil, err
	}

	c := newHTTPClient(name, socketPath, cmd, watch, desc.MaxConcurrent)
	p := &Plugin{Name: name, client: c, Tools: desc.Tools, AsyncJobs: desc.AsyncJobs,
		MaxConcurrent: desc.MaxConcurrent, cacheDir: cacheDir, cacheEphemeral: ephemeral}
	m.track(p)
	slog.Info("plugin started", "name", name, "tools", len(desc.Tools), "max_concurrent", desc.MaxConcurrent)
	return p, nil
}

// launch is how to run one plugin process: the executable plus any leading
// arguments, and the wire name the resulting plugin is known by. A standalone
// plugin binary is named after its file and takes no arguments; a built-in is
// the nine binary run as `nine plugin serve <name>`, so its name cannot be
// derived from the path (that would name every built-in "nine").
type launch struct {
	name string
	path string
	args []string
}

// binaryLaunch runs a standalone plugin binary — a user plugin, or the browser
// plugin's launcher script.
func binaryLaunch(binaryPath string) launch {
	return launch{name: filepath.Base(binaryPath), path: binaryPath}
}

// builtinLaunch runs a built-in plugin out of the nine binary at nineBin, under
// its own name.
func builtinLaunch(nineBin, name string) launch {
	return builtinInstanceLaunch(nineBin, name, name)
}

// builtinInstanceLaunch runs the built-in `builtin` under a different wire name.
// The two come apart when one built-in serves several plugins: the `mcp` bridge
// runs once per configured MCP server, so four processes all execute
// `nine plugin serve mcp` while presenting as `mcp:github`, `mcp:slack`, and so
// on (R-PLUG.15). Keeping them distinct is what lets each server hold its own
// row in the roster, its own crash isolation, and its own disable switch.
func builtinInstanceLaunch(nineBin, builtin, instance string) launch {
	return launch{name: instance, path: nineBin, args: []string{"plugin", "serve", builtin}}
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

// spawnAndDescribe spawns l on a fresh Unix socket, waits for it to
// listen, calls plugin.describe, and checks the reported protocol version. It is
// the shared front half of both Start (which keeps the process running) and
// Probe (which stops it). On any failure it tears the process and socket down and
// returns the error; on success the caller owns cmd and must eventually stop it
// and remove socketPath. env is the full extra environment (NINE_PLUGIN_SOCKET is
// appended here).
func spawnAndDescribe(l launch, env []string) (*exec.Cmd, *procWatch, string, DescribeResult, error) {
	name := l.name

	if err := os.MkdirAll(socketDir, 0o700); err != nil {
		return nil, nil, "", DescribeResult{}, fmt.Errorf("create socket dir: %w", err)
	}
	socketPath, err := allocSocketPath(name)
	if err != nil {
		return nil, nil, "", DescribeResult{}, err
	}

	env = append([]string{}, env...)
	env = append(env, "NINE_PLUGIN_SOCKET="+socketPath)

	cmd := exec.Command(l.path, l.args...) //nolint:gosec // path is a Nine-resolved plugin binary or the nine binary itself
	cmd.Env = append(sanitizedHostEnv(), env...)
	cmd.Stderr = os.Stderr // surface plugin startup/listen errors
	if err := cmd.Start(); err != nil {
		return nil, nil, "", DescribeResult{}, fmt.Errorf("start plugin: %w", err)
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
		return nil, nil, "", DescribeResult{}, err
	}

	// Read max_concurrent on a throwaway client first: net/http forbids mutating
	// a Transport after a request is issued on it, so the real client's transport
	// is built afterwards with MaxConnsPerHost already set.
	desc, err := describeOverSocket(context.Background(), socketPath)
	if err != nil {
		cleanup()
		return nil, nil, "", DescribeResult{}, fmt.Errorf("plugin.describe: %w", err)
	}

	if err := checkProtocolVersion(name, desc.ProtocolVersion); err != nil {
		cleanup()
		return nil, nil, "", DescribeResult{}, err
	}

	return cmd, watch, socketPath, desc, nil
}

// Probe runs binaryPath through the same handshake Start uses — spawn, wait for
// the socket, plugin.describe, protocol-version check — then stops the process.
// It reports whether the binary is a valid Nine plugin and, when it is, the tools
// it advertises, without tracking anything or leaving a process running. It backs
// `nine plugin validate` and the pre-load vetting of user plugins. env is passed
// straight through as the process environment (e.g. the manager's NINE_BIN).
func Probe(binaryPath string, env ...string) (DescribeResult, error) {
	// Validation must never create or touch persistent state, so Probe always
	// hands over a throwaway ephemeral cache dir removed with the process, whatever
	// the plugin's persist_cache setting (docs/plugin-capabilities.md §4).
	if cacheDir, err := os.MkdirTemp("", "nine-probe-cache-"); err == nil {
		defer os.RemoveAll(cacheDir) //nolint:errcheck // best-effort
		env = append(append([]string{}, env...),
			"NINE_PLUGIN_CACHE_DIR="+cacheDir,
			"NINE_PLUGIN_CACHE_PERSISTENT=0")
	}
	cmd, watch, socketPath, desc, err := spawnAndDescribe(binaryLaunch(binaryPath), env)
	if err != nil {
		return DescribeResult{}, err
	}
	cmd.Process.Kill() //nolint:errcheck // best-effort
	watch.wait()       //nolint:errcheck // reaped by watchProcess
	os.Remove(socketPath)
	return desc, nil
}

// checkProtocolVersion rejects a plugin whose wire-contract version the daemon
// does not support. Support is a set, not a single version: v2 is additive over
// v1 (it only adds jobs), so a v1 plugin is accepted and simply treated as
// lacking async jobs (docs/plugin-capabilities.md §6). An absent version (0)
// means the plugin predates protocol versioning and is rejected.
func checkProtocolVersion(name string, got int) error {
	if supportedProtocolVersion(got) {
		return nil
	}
	if got == 0 {
		return fmt.Errorf("plugin %q reports no protocol version; it predates plugin protocol v%d — rebuild it against the current Nine", name, ProtocolVersion)
	}
	return fmt.Errorf("plugin %q speaks protocol v%d but this daemon speaks v%d — rebuild the plugin (or upgrade Nine)", name, got, ProtocolVersion)
}

// supportedProtocolVersion reports whether the daemon can talk to a plugin
// advertising version v. v1 and the current v2 are both accepted.
func supportedProtocolVersion(v int) bool {
	return v == 1 || v == ProtocolVersion
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

// TryStartBuiltin is TryStart for a built-in plugin: it re-execs the nine
// binary rather than resolving a path under [plugins].bin, and likewise logs
// and returns nil instead of failing boot.
func (m *Manager) TryStartBuiltin(name string, extraEnv ...string) *Plugin {
	if m.IsDisabled(name) {
		m.noteDisabledSkip(name)
		slog.Info("built-in plugin disabled by config, not starting", "name", name)
		return nil
	}
	p, err := m.StartBuiltin(name, extraEnv...)
	if err != nil {
		slog.Warn("built-in plugin start failed", "name", name, "err", err)
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
