package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

const mcpProtocolVersion = "2024-11-05"

// mcpClient wraps a raw client connected to an MCP server and implements
// pluginClient by translating plugin.call ↔ tools/call.
type mcpClient struct {
	inner *client
}

// call intercepts "plugin.call" and translates it to MCP tools/call.
// All other methods are forwarded directly (used internally during handshake).
func (m *mcpClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if method != "plugin.call" {
		return m.inner.call(ctx, method, params)
	}

	// params is a CallRequest passed by Manager.Call.
	cr, ok := params.(CallRequest)
	if !ok {
		// Fallback: marshal/unmarshal.
		b, _ := json.Marshal(params)
		if err := json.Unmarshal(b, &cr); err != nil {
			return nil, fmt.Errorf("mcp: expected CallRequest: %w", err)
		}
	}

	mcpParams := map[string]any{
		"name":      cr.Tool,
		"arguments": cr.Args,
	}
	raw, err := m.inner.call(ctx, "tools/call", mcpParams)
	if err != nil {
		return nil, err
	}

	var mcpResult struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &mcpResult); err != nil {
		return nil, fmt.Errorf("mcp: parse tools/call response: %w", err)
	}

	var buf strings.Builder
	for _, c := range mcpResult.Content {
		if c.Type == "text" {
			buf.WriteString(c.Text)
		}
	}
	text := buf.String()

	if mcpResult.IsError {
		return nil, fmt.Errorf("tool error: %s", text)
	}
	return json.Marshal(CallResult{Output: text})
}

func (m *mcpClient) stop() error { return m.inner.stop() }

// StartMCP spawns an MCP server, performs the initialize + initialized
// handshake, lists its tools, and returns a Plugin usable identically to a
// native Nine plugin.
func (m *Manager) StartMCP(binaryPath string, args []string, extraEnv ...string) (*Plugin, error) {
	// Checked here rather than inherited: StartMCP builds its own stdio client
	// instead of going through start(), so it is the one spawn path the shared
	// check in start() does not cover (R-PLUG.14).
	name := filepath.Base(binaryPath)
	if m.IsDisabled(name) {
		return nil, fmt.Errorf("%q: %w", name, ErrPluginDisabled)
	}

	env := append(m.env, extraEnv...)
	c, err := newClient(binaryPath, args, env)
	if err != nil {
		return nil, err
	}

	// 1. initialize
	_, err = c.call(context.Background(), "initialize", map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "nine", "version": "0.1.0"},
	})
	if err != nil {
		c.stop() //nolint:errcheck // best-effort subprocess cleanup
		return nil, fmt.Errorf("MCP initialize: %w", err)
	}

	// 2. notifications/initialized (one-way, no response)
	if err := c.sendNotification("notifications/initialized", nil); err != nil {
		c.stop() //nolint:errcheck // best-effort subprocess cleanup
		return nil, fmt.Errorf("MCP initialized notification: %w", err)
	}

	// 3. tools/list
	raw, err := c.call(context.Background(), "tools/list", map[string]any{})
	if err != nil {
		c.stop() //nolint:errcheck // best-effort subprocess cleanup
		return nil, fmt.Errorf("MCP tools/list: %w", err)
	}

	var listResult struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"` // MCP uses camelCase
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &listResult); err != nil {
		c.stop() //nolint:errcheck // best-effort subprocess cleanup
		return nil, fmt.Errorf("MCP parse tools/list: %w", err)
	}

	tools := make([]ToolDefinition, len(listResult.Tools))
	for i, t := range listResult.Tools {
		tools[i] = ToolDefinition{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema, // schema content is identical; only key name differs
		}
	}

	p := &Plugin{Name: filepath.Base(binaryPath), client: &mcpClient{inner: c}, Tools: tools}
	m.track(p)
	return p, nil
}
