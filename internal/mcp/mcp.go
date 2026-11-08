// Package mcp is Nine's MCP implementation in both directions
// (adr/modular_nine.md): a hand-rolled JSON-RPC client for reaching MCP
// servers, and a server side built on the vendored mcp-go SDK for serving
// Nine's own tools over MCP across a process boundary.
//
// The client moved here from internal/builtins when the plugin manager became
// the single MCP client for everything in another process; the server side
// exists so first-party subprocess tools (shell today) speak the one wire
// protocol that crosses a process boundary. Built-in tools that stay
// in-process never touch this package.
package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// The MCP revision this client announces depends on the transport: streamable
// HTTP was introduced in 2025-03-26, so announcing the older revision to a
// hosted server is a contradiction — a strict one may reject it, and a lenient
// one may fall back to the superseded two-endpoint SSE flow we do not
// implement. stdio stays on the older revision, which is what the servers in
// the wild target and what tools/list + tools/call need.
const (
	ProtocolVersionStdio = "2024-11-05"
	ProtocolVersionHTTP  = "2025-03-26"
)

// ProtocolVersionFor is the revision to announce for a given server spec.
func ProtocolVersionFor(spec ServerSpec) string {
	if spec.URL != "" || spec.Socket != "" {
		return ProtocolVersionHTTP
	}
	return ProtocolVersionStdio
}

// ToolSeparator joins a server name to a tool name. Two MCP servers that both
// advertise `search` would otherwise collide, and a collision means the second
// tool is dropped entirely (R-PLUG.9) — a silent capability loss that depends
// on load order. Prefixing makes that structurally impossible, at the cost of
// longer names in the model's context. First-party servers keep bare names
// (adr/modular_nine.md §9.9); only servers on the MCP roster are prefixed.
const ToolSeparator = "__"

// ServerSpec is one MCP server, handed to a client by the daemon as JSON in
// NINE_MCP_SERVER.
//
// It arrives through the environment rather than being read from nine.toml,
// because a plugin child must not load the operator's config (R-PLUG.13a):
// that file carries the embeddings API key and every other plugin's settings.
// The daemon parses [[mcp.server]] and passes down exactly this server's slice
// of it. Encoding structured data in an env var is normally refused for plugin
// settings — inventing an encoding invites two plugins to disagree about it —
// but the daemon and its children are one build and cannot disagree.
type ServerSpec struct {
	Name string `json:"name"`

	// Command/Args/Env describe a server spawned locally and spoken to over
	// stdio. URL/Headers describe one reached over streamable HTTP. Socket is
	// the local streamable-HTTP-on-a-Unix-socket transport the manager uses for
	// first-party subprocess plugins. Config validation enforces that exactly
	// one is set before a spawned server ever runs.
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`

	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`

	Socket string `json:"socket,omitempty"`

	// NineVersion is reported to the server as clientInfo.version in the
	// handshake. It rides along because a plugin child has no other way to know
	// it — the version is a build-time value in cmd/nine, and this process may
	// not read config. Empty is tolerated: MCP servers do not branch on it, and
	// an absent version beats a stale hardcoded one.
	NineVersion string `json:"nine_version,omitempty"`
}

// SpecFromEnv reads and validates the server spec the daemon passed down.
func SpecFromEnv() (ServerSpec, error) {
	raw := os.Getenv("NINE_MCP_SERVER")
	if raw == "" {
		return ServerSpec{}, fmt.Errorf("NINE_MCP_SERVER is not set — the mcp bridge is started per [[mcp.server]] by the daemon, not on its own")
	}
	var spec ServerSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return ServerSpec{}, fmt.Errorf("parse NINE_MCP_SERVER: %w", err)
	}
	if spec.Name == "" {
		return ServerSpec{}, fmt.Errorf("NINE_MCP_SERVER needs a name")
	}
	if (spec.Command == "") == (spec.URL == "") && spec.Socket == "" {
		return ServerSpec{}, fmt.Errorf("NINE_MCP_SERVER needs exactly one of command (stdio), url (streamable HTTP), or socket (unix-socket HTTP)")
	}
	return spec, nil
}

// DefaultHandshakeTimeout bounds how long a server has to come up and answer
// tools/list. It is finite because the caller (plugin.describe) has no
// deadline of its own, so a server that never answers would otherwise hold up
// daemon boot forever.
//
// The value is set from measurement, not taste: `npx -y
// @modelcontextprotocol/server-filesystem` took ~72s to reach tools/list here,
// warm cache included. Anything on the order of the daemon's socket budget is
// hopeless for real servers, and a bound close to the observed time would turn
// a slow day into a failed boot.
const DefaultHandshakeTimeout = 3 * time.Minute

// HandshakeTimeout is DefaultHandshakeTimeout unless NINE_MCP_HANDSHAKE_TIMEOUT
// overrides it. The knob exists because the right value is a property of the
// server, not of Nine: a package resolved over a slow link may need longer, and
// a test asserting that the timeout fires cannot wait out three minutes. An
// unparseable value falls back to the default rather than failing the client —
// a malformed duration should not cost an operator their MCP server.
func HandshakeTimeout() time.Duration {
	if v := os.Getenv("NINE_MCP_HANDSHAKE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		fmt.Fprintf(os.Stderr, "mcp: ignoring unparseable NINE_MCP_HANDSHAKE_TIMEOUT %q\n", v)
	}
	return DefaultHandshakeTimeout
}

// CallTimeout bounds one tool call. stdio cannot cancel an in-flight request
// without losing stream sync, so an unbounded call is how a single hung server
// wedges every later call to it until the daemon restarts. This is the
// backstop when the caller's own ctx carries no deadline; a shorter
// task_timeout wins.
const CallTimeout = 5 * time.Minute
