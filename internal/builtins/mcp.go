package builtins

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

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

// The MCP revision this bridge announces, which depends on the transport:
// streamable HTTP was introduced in 2025-03-26, so announcing the older revision
// to a hosted server is a contradiction — a strict one may reject it, and a
// lenient one may fall back to the superseded two-endpoint SSE flow we do not
// implement. stdio stays on the older revision, which is what the servers in the
// wild target and what tools/list + tools/call need.
const (
	mcpProtocolVersionStdio = "2024-11-05"
	mcpProtocolVersionHTTP  = "2025-03-26"
)

// mcpProtocolVersion is the revision to announce for a given transport.
func mcpProtocolVersion(spec mcpServerSpec) string {
	if spec.URL != "" {
		return mcpProtocolVersionHTTP
	}
	return mcpProtocolVersionStdio
}

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
	Name string `json:"name"`

	// Command/Args/Env describe a server spawned locally and spoken to over
	// stdio. URL/Headers describe one reached over streamable HTTP. Exactly one
	// pair is set; config validation enforces that before this ever runs.
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`

	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`

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

	// The handshake runs inside ServeDeferred's ready func, not before it — the
	// socket must exist within the daemon's ~3s socket-ready budget (R-PLUG.3),
	// and an `npx`-launched MCP server routinely takes longer than that just to
	// start. Listening first moves the wait into plugin.describe, which has no
	// deadline. Handshaking first is exactly the bug that made every real
	// third-party server fail to load while a compiled test fixture passed.
	// The connection is built inside ready but has to be reachable from the
	// shutdown hook, which is registered before it exists.
	var (
		connMu sync.Mutex
		conn   mcpConn
	)

	plugin.ServeDeferred(
		func(ctx context.Context) ([]plugin.ToolDefinition, map[string]plugin.ToolHandler, error) {
			ctx, cancel := context.WithTimeout(ctx, handshakeTimeout())
			defer cancel()

			c, tools, err := mcpConnect(ctx, spec)
			if err != nil {
				return nil, nil, err
			}
			connMu.Lock()
			conn = c
			connMu.Unlock()
			exitWhenServerDies(spec.Name, c)

			defs, handlers := mcpToolSurface(spec.Name, c, tools)
			return defs, handlers, nil
		},
		// stdio has no per-message framing, so a response can only be matched to
		// its request by owning the stream for the whole round trip. The transport
		// is serial; saying so lets the daemon bound concurrency correctly instead
		// of discovering it as contention (R-PLUG.8).
		plugin.WithMaxConcurrent(1),
		// Registered with Serve rather than as a second signal handler: two
		// handlers race, and Serve's calls os.Exit the moment it wakes, so a
		// competing one may never get to kill the server — leaving the orphaned
		// process tree this is here to prevent.
		plugin.WithOnShutdown(func() {
			connMu.Lock()
			c := conn
			connMu.Unlock()
			if c != nil {
				c.stop() //nolint:errcheck // best-effort teardown on shutdown
			}
		}),
	)
}

// mcpHandshakeTimeout bounds how long a server has to come up and answer
// tools/list. It is finite because plugin.describe has no deadline of its own,
// so a server that never answers would otherwise hold up daemon boot forever.
//
// The value is set from measurement, not taste: `npx -y
// @modelcontextprotocol/server-filesystem` took ~72s to reach tools/list here,
// warm cache included. Anything on the order of the daemon's 3s socket budget
// is hopeless for real servers, and a bound close to the observed time would
// turn a slow day into a failed boot.
const mcpHandshakeTimeout = 3 * time.Minute

// handshakeTimeout is mcpHandshakeTimeout unless NINE_MCP_HANDSHAKE_TIMEOUT
// overrides it. The knob exists because the right value is a property of the
// server, not of Nine: a package resolved over a slow link may need longer, and
// a test asserting that the timeout fires cannot wait out three minutes. An
// unparseable value falls back to the default rather than failing the bridge —
// a malformed duration should not cost an operator their MCP server.
func handshakeTimeout() time.Duration {
	if v := os.Getenv("NINE_MCP_HANDSHAKE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		fmt.Fprintf(os.Stderr, "mcp: ignoring unparseable NINE_MCP_HANDSHAKE_TIMEOUT %q\n", v)
	}
	return mcpHandshakeTimeout
}

// mcpCallTimeout bounds one tool call. stdio cannot cancel an in-flight request
// without losing stream sync, so an unbounded call is how a single hung server
// wedges every later call to it until the daemon restarts. This is the backstop
// when the daemon's own ctx carries no deadline; a shorter task_timeout wins.
const mcpCallTimeout = 5 * time.Minute

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
	if spec.Name == "" {
		return mcpServerSpec{}, fmt.Errorf("NINE_MCP_SERVER needs a name")
	}
	if (spec.Command == "") == (spec.URL == "") {
		return mcpServerSpec{}, fmt.Errorf("NINE_MCP_SERVER needs exactly one of command (stdio) or url (streamable HTTP)")
	}
	return spec, nil
}

// mcpConn is one connection to an MCP server, over either transport. The bridge
// is written against this so the handshake, tool prefixing, and call
// translation are shared: only how bytes move differs between a spawned server
// and a hosted one.
type mcpConn interface {
	call(ctx context.Context, method string, params any) (json.RawMessage, error)
	notify(method string, params any) error
	stop() error
	// Exited closes when the connection is finished — for stdio, when the server
	// process dies; for HTTP, only on stop, since there is no process to lose.
	Exited() <-chan struct{}
	StoppedDeliberately() bool
}

// dialSpec opens the transport the spec asks for. The rest of the bridge is
// transport-agnostic from here on.
func dialSpec(spec mcpServerSpec) (mcpConn, error) {
	if spec.URL != "" {
		return dialMCPHTTP(spec.URL, spec.Headers), nil
	}
	env := make([]string, 0, len(spec.Env))
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}
	return dialMCP(spec.Command, spec.Args, env)
}

// mcpTool is one tool as the MCP server describes it.
type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"` // MCP spells it camelCase
}

// mcpConnect spawns the server and completes the MCP handshake —
// initialize → notifications/initialized → tools/list — returning the live
// connection and what it advertises. On any failure the process is torn down,
// so a half-initialized server is never left running.
func mcpConnect(ctx context.Context, spec mcpServerSpec) (mcpConn, []mcpTool, error) {
	conn, err := dialSpec(spec)
	if err != nil {
		return nil, nil, err
	}

	// A stdio read blocks in Scan and cannot be interrupted by a context, so the
	// deadline is enforced by tearing the connection down: stop() closes stdin,
	// the server exits, and the blocked read returns. Without this a server that
	// accepts input and never replies would hang the handshake forever, and with
	// it plugin.describe and the daemon's boot.
	handshakeDone := make(chan struct{})
	defer close(handshakeDone)
	go func() {
		select {
		case <-ctx.Done():
			conn.stop() //nolint:errcheck // best-effort: unblock the handshake
		case <-handshakeDone:
		}
	}()

	fail := func(err error) (mcpConn, []mcpTool, error) {
		conn.stop() //nolint:errcheck // best-effort teardown on a failed handshake
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, fmt.Errorf("handshake timed out after %s: %w", handshakeTimeout(), ctxErr)
		}
		return nil, nil, err
	}

	version := spec.NineVersion
	if version == "" {
		version = "unknown"
	}
	if _, err := conn.call(ctx, "initialize", map[string]any{
		"protocolVersion": mcpProtocolVersion(spec),
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "nine", "version": version},
	}); err != nil {
		return fail(fmt.Errorf("initialize: %w", err))
	}
	if err := conn.notify("notifications/initialized", nil); err != nil {
		return fail(fmt.Errorf("initialized notification: %w", err))
	}

	raw, err := conn.call(ctx, "tools/list", map[string]any{})
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
func mcpToolSurface(server string, conn mcpConn, tools []mcpTool) ([]plugin.ToolDefinition, map[string]plugin.ToolHandler) {
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
		handlers[prefixed] = func(ctx context.Context, args json.RawMessage) (string, error) {
			// The daemon's ctx carries task_timeout, and a call bounded by nothing
			// is how one hung server wedges every later call to it.
			ctx, cancel := context.WithTimeout(ctx, mcpCallTimeout)
			defer cancel()
			return mcpCall(ctx, conn, upstream, args)
		}
	}
	return defs, handlers
}

// mcpCall performs one tools/call and flattens the MCP content array to the
// single string the plugin contract returns.
//
// Every part survives in some form — see flattenMCPContent. That was not always
// true: text-only flattening meant a screenshot arrived as a summary saying a
// screenshot was taken, minus the image, which is a plausible wrong answer
// rather than a visible failure.
func mcpCall(ctx context.Context, conn mcpConn, tool string, args json.RawMessage) (string, error) {
	raw, err := conn.call(ctx, "tools/call", map[string]any{
		"name":      tool,
		"arguments": args,
	})
	if err != nil {
		return "", err
	}

	var result struct {
		Content []mcpContent `json:"content"`
		IsError bool         `json:"isError"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("parse tools/call response: %w", err)
	}

	text := flattenMCPContent(tool, result.Content)

	// An MCP tool reports failure in-band via isError; the plugin contract wants
	// a Go error, so the model sees a failed tool call rather than prose that
	// happens to describe a failure.
	if result.IsError {
		return "", fmt.Errorf("%s: %s", tool, text)
	}
	return text, nil
}

// mcpContent is one item of a tools/call result. MCP defines several kinds and
// a server may return a mix — Playwright's browser_take_screenshot answers with
// a text summary *and* an image in the same response.
type mcpContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`     // image/audio: base64
	MimeType string `json:"mimeType,omitempty"` // image/audio
	URI      string `json:"uri,omitempty"`      // resource_link
	Name     string `json:"name,omitempty"`     // resource_link
	Resource *struct {
		URI      string `json:"uri,omitempty"`
		MimeType string `json:"mimeType,omitempty"`
		Text     string `json:"text,omitempty"`
		Blob     string `json:"blob,omitempty"` // base64
	} `json:"resource,omitempty"`
}

// flattenMCPContent renders a tools/call result as the single string the plugin
// contract returns.
//
// Everything that is not text used to be dropped on the floor. That is worse
// than it sounds: a screenshot came back as a text summary saying a screenshot
// was taken, plus the image — so the model was handed a confident description
// of a picture it never received, and had no way to tell. Nothing is silently
// discarded now. Binary parts are written to the plugin's cache dir and named
// in the text, so the bytes stay reachable (read_file) without spending a
// context window on base64.
func flattenMCPContent(tool string, content []mcpContent) string {
	var b strings.Builder
	for _, c := range content {
		switch c.Type {
		case "text":
			b.WriteString(c.Text)

		case "image", "audio":
			writeBinaryPart(&b, tool, c.Type, c.MimeType, c.Data)

		case "resource":
			switch {
			case c.Resource == nil:
				fmt.Fprintf(&b, "\n[%s: empty resource]", c.Type)
			case c.Resource.Text != "":
				b.WriteString(c.Resource.Text)
			case c.Resource.Blob != "":
				writeBinaryPart(&b, tool, "resource", c.Resource.MimeType, c.Resource.Blob)
			default:
				fmt.Fprintf(&b, "\n[resource: %s]", c.Resource.URI)
			}

		case "resource_link":
			fmt.Fprintf(&b, "\n[resource: %s %s]", c.Name, c.URI)

		default:
			// An unknown kind is still reported rather than dropped: a future MCP
			// content type should look like something missing, not like nothing.
			fmt.Fprintf(&b, "\n[unsupported MCP content type %q]", c.Type)
		}
	}
	return b.String()
}

// writeBinaryPart saves a base64 part beside the plugin's cache dir and notes
// where it went. With no cache dir configured it says what was received and
// that it was not kept — still better than silence.
func writeBinaryPart(b *strings.Builder, tool, kind, mime, data string) {
	if data == "" {
		fmt.Fprintf(b, "\n[%s: empty]", kind)
		return
	}
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		fmt.Fprintf(b, "\n[%s: undecodable base64 (%d chars)]", kind, len(data))
		return
	}

	dir := os.Getenv("NINE_PLUGIN_CACHE_DIR")
	if dir == "" {
		fmt.Fprintf(b, "\n[%s: %s, %d bytes, not saved — no plugin cache dir configured]", kind, mime, len(decoded))
		return
	}
	// The tool name is server-supplied and lands in a path, so it is sanitized
	// rather than trusted: a server advertising a tool called "../escaped" would
	// otherwise write its image outside the cache dir, anywhere the daemon can
	// write. Verified before fixing — the file really did land in the parent.
	name := fmt.Sprintf("%s-%d%s", safeFileName(tool), time.Now().UnixNano(), extensionForMIME(mime))
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, decoded, 0o600); err != nil {
		fmt.Fprintf(b, "\n[%s: %s, %d bytes, could not be saved: %v]", kind, mime, len(decoded), err)
		return
	}
	fmt.Fprintf(b, "\n[%s: %s, %d bytes, saved to %s]", kind, mime, len(decoded), path)
}

// safeFileName reduces a server-supplied name to something that cannot steer a
// path: separators, traversal, and anything outside a conservative set are
// replaced. Empty or fully-stripped input still yields a usable stem.
func safeFileName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "part"
	}
	return out
}

// extensionForMIME picks a file extension so a saved part is recognisable.
func extensionForMIME(mime string) string {
	// Strip any parameters ("image/png; charset=..."), then take the subtype.
	mime, _, _ = strings.Cut(mime, ";")
	_, sub, ok := strings.Cut(strings.TrimSpace(mime), "/")
	if !ok || sub == "" {
		return ".bin"
	}
	if i := strings.LastIndex(sub, "+"); i >= 0 { // image/svg+xml → xml
		sub = sub[i+1:]
	}
	for _, r := range sub {
		if r < 'a' || r > 'z' || r < '0' || r > '9' {
			return ".bin"
		}
	}
	return "." + sub
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
func exitWhenServerDies(server string, conn mcpConn) {
	go func() {
		<-conn.Exited()
		if conn.StoppedDeliberately() {
			return // ordinary shutdown, not a failure
		}
		fmt.Fprintf(os.Stderr, "mcp %s: server exited; stopping the bridge rather than serving a plugin with nothing behind it\n", server)
		os.Exit(1)
	}()
}
