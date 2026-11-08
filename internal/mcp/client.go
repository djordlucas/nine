package mcp

import (
	"context"
	"encoding/json"
	"fmt"
)

// Conn is one connection to an MCP server, over any transport. The client is
// written against this so the handshake, tool calls, and call translation are
// shared: only how bytes move differs between a spawned server, a hosted one,
// and a first-party plugin on a Unix socket.
type Conn interface {
	Call(ctx context.Context, method string, params any) (json.RawMessage, error)
	Notify(method string, params any) error
	Stop() error
	// Exited closes when the connection is finished — for stdio, when the server
	// process dies; for HTTP, only on Stop, since there is no process to lose.
	Exited() <-chan struct{}
	StoppedDeliberately() bool
	// SpillDir is where binary content parts are written (R-PLUG.15b). Empty
	// falls back to NINE_PLUGIN_CACHE_DIR, then to reporting the part unsaved.
	SpillDir() string
}

// ConnOption configures one connection.
type ConnOption func(*connConfig)

type connConfig struct {
	spillDir string
}

// WithSpillDir sets the directory binary content parts are written to.
func WithSpillDir(dir string) ConnOption {
	return func(c *connConfig) { c.spillDir = dir }
}

// Tool is one tool as an MCP server describes it.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"` // MCP spells it camelCase
}

// Dial opens the transport the spec asks for. The rest of the client is
// transport-agnostic from here on.
func Dial(spec ServerSpec) (Conn, error) {
	switch {
	case spec.Socket != "":
		return DialSocket(spec.Socket), nil
	case spec.URL != "":
		return DialHTTP(spec.URL, spec.Headers), nil
	default:
		env := make([]string, 0, len(spec.Env))
		for k, v := range spec.Env {
			env = append(env, k+"="+v)
		}
		return DialStdio(spec.Command, spec.Args, env)
	}
}

// Connect spawns or reaches the server and completes the MCP handshake —
// initialize → notifications/initialized → tools/list — returning the live
// connection and what it advertises. On any failure the connection is torn
// down, so a half-initialized server is never left running.
func Connect(ctx context.Context, spec ServerSpec) (Conn, []Tool, error) {
	conn, err := Dial(spec)
	if err != nil {
		return nil, nil, err
	}

	// A stdio read blocks in Scan and cannot be interrupted by a context, so the
	// deadline is enforced by tearing the connection down: Stop() closes stdin,
	// the server exits, and the blocked read returns. Without this a server that
	// accepts input and never replies would hang the handshake forever, and with
	// it the caller's boot.
	handshakeDone := make(chan struct{})
	defer close(handshakeDone)
	go func() {
		select {
		case <-ctx.Done():
			conn.Stop() //nolint:errcheck // best-effort: unblock the handshake
		case <-handshakeDone:
		}
	}()

	fail := func(err error) (Conn, []Tool, error) {
		conn.Stop() //nolint:errcheck // best-effort teardown on a failed handshake
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, fmt.Errorf("handshake timed out after %s: %w", HandshakeTimeout(), ctxErr)
		}
		return nil, nil, err
	}

	version := spec.NineVersion
	if version == "" {
		version = "unknown"
	}
	tools, err := Handshake(ctx, conn, ProtocolVersionFor(spec), "nine", version)
	if err != nil {
		return fail(err)
	}
	return conn, tools, nil
}

// ConnectSocket completes the MCP handshake with a first-party server listening
// on a Unix socket (streamable HTTP), the transport the plugin manager uses
// for subprocess plugins that speak MCP (adr/modular_nine.md §9.6).
func ConnectSocket(ctx context.Context, socketPath string, nineVersion string) (Conn, []Tool, error) {
	conn := DialSocket(socketPath)
	handshakeDone := make(chan struct{})
	defer close(handshakeDone)
	go func() {
		select {
		case <-ctx.Done():
			conn.Stop() //nolint:errcheck
		case <-handshakeDone:
		}
	}()
	if nineVersion == "" {
		nineVersion = "unknown"
	}
	tools, err := Handshake(ctx, conn, ProtocolVersionHTTP, "nine", nineVersion)
	if err != nil {
		conn.Stop() //nolint:errcheck
		return nil, nil, err
	}
	return conn, tools, nil
}

// Handshake runs initialize → notifications/initialized → tools/list on an
// open connection and returns the advertised tools.
func Handshake(ctx context.Context, conn Conn, protocolVersion, clientName, clientVersion string) ([]Tool, error) {
	if _, err := conn.Call(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": clientName, "version": clientVersion},
	}); err != nil {
		return nil, fmt.Errorf("initialize: %w", err)
	}
	if err := conn.Notify("notifications/initialized", nil); err != nil {
		return nil, fmt.Errorf("initialized notification: %w", err)
	}

	raw, err := conn.Call(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	var listed struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		return nil, fmt.Errorf("parse tools/list: %w", err)
	}
	return listed.Tools, nil
}

// CallTool performs one tools/call and flattens the MCP content array to the
// single string Nine's tool contract returns.
//
// Every part survives in some form — see FlattenContent. An MCP tool reports
// failure in-band via isError; the caller wants a Go error, so the model sees a
// failed tool call rather than prose that happens to describe a failure.
func CallTool(ctx context.Context, conn Conn, tool string, args json.RawMessage) (string, error) {
	raw, err := conn.Call(ctx, "tools/call", map[string]any{
		"name":      tool,
		"arguments": args,
	})
	if err != nil {
		return "", err
	}

	var result struct {
		Content []Content `json:"content"`
		IsError bool      `json:"isError"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("parse tools/call response: %w", err)
	}

	text := FlattenContent(tool, result.Content, conn.SpillDir())
	if result.IsError {
		return "", fmt.Errorf("%s: %s", tool, text)
	}
	return text, nil
}
