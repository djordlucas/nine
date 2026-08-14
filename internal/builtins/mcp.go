package builtins

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"nine/internal/plugin"
)

// The MCP bridge. One instance fronts one MCP server: the daemon starts
// `nine plugin serve mcp` once per [[mcp.server]], each under its own wire name
// (`mcp:github`), so an MCP server is a plugin in every respect — its own
// process, its own crash isolation, its own roster row, its own disable switch
// (spec/contracts/plugin.md R-PLUG.15).
//
// Before this, MCP was a second transport inside the plugin manager: a whole
// stdio JSON-RPC client, an adapter implementing the plugin-client interface,
// and an "except MCP" clause in five separate contract rules. Behind a plugin,
// none of that reaches the core. The limitations did not vanish — stdio still
// cannot be cancelled mid-read and still serializes — but they are now this
// bridge's business, declared honestly through the ordinary plugin contract
// (max_concurrent: 1) instead of being exceptions the daemon had to carry.

// mcpProtocolVersion is the MCP revision this bridge speaks in the initialize
// handshake.
const mcpProtocolVersion = "2024-11-05"

// mcpToolSeparator joins a server name to a tool name. Two MCP servers that
// both advertise `search` would otherwise collide, and a collision means the
// second tool is dropped entirely (R-PLUG.9) — a silent capability loss that
// depends on load order. Prefixing makes that structurally impossible, at the
// cost of longer names in the model's context.
const mcpToolSeparator = "__"

// mcpServerSpec is one MCP server, handed to this bridge by the daemon as JSON
// in NINE_MCP_SERVER.
//
// It arrives through the environment rather than being read from nine.toml,
// because a plugin child must not load the operator's config (R-PLUG.13a): that
// file carries the embeddings API key and every other plugin's settings. The
// daemon parses [[mcp.server]] and passes down exactly this server's slice of
// it. Encoding structured data in an env var is normally refused for plugin
// settings — inventing an encoding invites two plugins to disagree about it —
// but the daemon and this bridge are one build and cannot disagree.
type mcpServerSpec struct {
	Name    string            `json:"name"`
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`

	// NineVersion is reported to the server as clientInfo.version in the
	// handshake. It rides along because a plugin child has no other way to know
	// it — the version is a build-time value in cmd/nine, and this process may
	// not read config. Empty is tolerated: MCP servers do not branch on it, and
	// an absent version beats a stale hardcoded one.
	NineVersion string `json:"nine_version,omitempty"`
}

// serveMCP runs the bridge: read the server spec, spawn it, complete the MCP
// handshake, then serve its tools through the ordinary plugin contract.
func serveMCP() {
	spec, err := mcpSpecFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcp: %v\n", err)
		os.Exit(1)
	}

	conn, tools, err := mcpConnect(spec)
	if err != nil {
		// Exiting here means the daemon's TryStartBuiltinInstance logs the failure
		// and carries on without this server, which is the right blast radius: one
		// unreachable MCP server must not stop Nine from booting.
		fmt.Fprintf(os.Stderr, "mcp %s: %v\n", spec.Name, err)
		os.Exit(1)
	}

	// plugin.Serve blocks and owns SIGTERM for the socket, but it does not know
	// about the child process this bridge spawned. Without this the MCP server
	// would outlive its bridge on shutdown.
	stopOnSignal(conn)
	exitWhenServerDies(spec.Name, conn)

	defs, handlers := mcpToolSurface(spec.Name, conn, tools)
	plugin.Serve(defs, handlers,
		// stdio has no per-message framing, so a response can only be matched to
		// its request by owning the stream for the whole round trip. The transport
		// is serial; saying so lets the daemon bound concurrency correctly instead
		// of discovering it as contention (R-PLUG.8).
		plugin.WithMaxConcurrent(1),
	)
}

// mcpSpecFromEnv reads and validates the server spec the daemon passed down.
func mcpSpecFromEnv() (mcpServerSpec, error) {
	raw := os.Getenv("NINE_MCP_SERVER")
	if raw == "" {
		return mcpServerSpec{}, fmt.Errorf("NINE_MCP_SERVER is not set — the mcp bridge is started per [[mcp.server]] by the daemon, not on its own")
	}
	var spec mcpServerSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return mcpServerSpec{}, fmt.Errorf("parse NINE_MCP_SERVER: %w", err)
	}
	if spec.Name == "" || spec.Command == "" {
		return mcpServerSpec{}, fmt.Errorf("NINE_MCP_SERVER needs both name and command")
	}
	return spec, nil
}

// mcpTool is one tool as the MCP server describes it.
type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"` // MCP spells it camelCase
}

// mcpConnect spawns the server and completes the MCP handshake —
// initialize → notifications/initialized → tools/list — returning the live
// connection and what it advertises. On any failure the process is torn down,
// so a half-initialized server is never left running.
func mcpConnect(spec mcpServerSpec) (*mcpStdio, []mcpTool, error) {
	env := make([]string, 0, len(spec.Env))
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}

	conn, err := dialMCP(spec.Command, spec.Args, env)
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (*mcpStdio, []mcpTool, error) {
		conn.stop() //nolint:errcheck // best-effort teardown on a failed handshake
		return nil, nil, err
	}

	version := spec.NineVersion
	if version == "" {
		version = "unknown"
	}
	if _, err := conn.call("initialize", map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "nine", "version": version},
	}); err != nil {
		return fail(fmt.Errorf("initialize: %w", err))
	}
	if err := conn.notify("notifications/initialized", nil); err != nil {
		return fail(fmt.Errorf("initialized notification: %w", err))
	}

	raw, err := conn.call("tools/list", map[string]any{})
	if err != nil {
		return fail(fmt.Errorf("tools/list: %w", err))
	}
	var listed struct {
		Tools []mcpTool `json:"tools"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		return fail(fmt.Errorf("parse tools/list: %w", err))
	}
	return conn, listed.Tools, nil
}

// mcpToolSurface turns the server's tools into the plugin contract's shape:
// prefixed names for the daemon and the model, each handler translating back to
// the server's own unprefixed name.
func mcpToolSurface(server string, conn *mcpStdio, tools []mcpTool) ([]plugin.ToolDefinition, map[string]plugin.ToolHandler) {
	defs := make([]plugin.ToolDefinition, 0, len(tools))
	handlers := make(map[string]plugin.ToolHandler, len(tools))

	for _, t := range tools {
		prefixed := server + mcpToolSeparator + t.Name
		defs = append(defs, plugin.ToolDefinition{
			Name:        prefixed,
			Description: t.Description,
			InputSchema: t.InputSchema, // the schema itself is identical; only MCP's key name differs
		})
		// upstream is captured per tool: the model calls the prefixed name, the
		// server only ever knows its own.
		upstream := t.Name
		handlers[prefixed] = func(_ context.Context, args json.RawMessage) (string, error) {
			return mcpCall(conn, upstream, args)
		}
	}
	return defs, handlers
}

// mcpCall performs one tools/call and flattens the MCP content array to the
// single string the plugin contract returns.
//
// The flattening is lossy — MCP content can carry images and resource links,
// and only text survives. That was true of the old in-core adapter too, so it
// is not a regression, but it is now a limitation of one plugin rather than of
// Nine's tool contract, and can be revisited here alone.
func mcpCall(conn *mcpStdio, tool string, args json.RawMessage) (string, error) {
	raw, err := conn.call("tools/call", map[string]any{
		"name":      tool,
		"arguments": args,
	})
	if err != nil {
		return "", err
	}

	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("parse tools/call response: %w", err)
	}

	var text strings.Builder
	for _, c := range result.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	// An MCP tool reports failure in-band via isError; the plugin contract wants
	// a Go error, so the model sees a failed tool call rather than prose that
	// happens to describe a failure.
	if result.IsError {
		return "", fmt.Errorf("%s: %s", tool, text.String())
	}
	return text.String(), nil
}

// exitWhenServerDies ends the bridge when its MCP server exits on its own.
//
// Without this the bridge outlives the thing it exists to front: it keeps
// serving, keeps reporting `ok` in the roster, and fails every call with "MCP
// server closed stdout", while the dead server sits unreaped as a zombie. The
// bridge's liveness would mean nothing, because what it fronts is gone.
//
// Exiting makes a dead server present as a dead plugin. Nine's supervision
// watches plugin processes, not the grandchildren one spawns, so this is what
// puts an MCP server on the same footing as every other plugin — whatever
// happens to a plugin that dies happens to this one too, no sooner and no later.
// (Today that means the failure surfaces on the next call, as it does for any
// plugin: EventPluginCrashed exists in the supervisor but nothing emits it yet,
// so there is no automatic restart to rely on for MCP or anything else.)
func exitWhenServerDies(server string, conn *mcpStdio) {
	go func() {
		<-conn.Exited()
		if conn.StoppedDeliberately() {
			return // ordinary shutdown, not a failure
		}
		fmt.Fprintf(os.Stderr, "mcp %s: server exited; stopping the bridge rather than serving a plugin with nothing behind it\n", server)
		os.Exit(1)
	}()
}

// stopOnSignal tears the MCP server down when the daemon stops this bridge.
// plugin.Serve installs its own handler for the socket, but nothing there knows
// about a spawned child, so without this an MCP server would survive the plugin
// that owns it and leak one process per daemon restart.
func stopOnSignal(conn *mcpStdio) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigs
		conn.stop() //nolint:errcheck // best-effort teardown on shutdown
	}()
}
