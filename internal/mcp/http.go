package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The streamable-HTTP MCP transport (MCP revision 2025-03-26), for servers
// reached at a URL and for first-party servers on a Unix socket. Hosted MCP
// services never run locally, so stdio cannot reach them at all.
//
// The shape is a single endpoint that takes a JSON-RPC request by POST and
// answers in one of two ways: a plain JSON body, or an SSE stream carrying the
// response among other events. A server picks per request, so a client has to
// accept both — that dual response is the whole of what makes this transport
// "streamable".
//
// Two headers carry state across requests: the server may issue an
// Mcp-Session-Id on initialize which every later request must echo, and the
// negotiated protocol version rides along the same way.

const (
	// EndpointPath is the path streamable-HTTP servers answer on. mcp-go's
	// default; the socket client posts here.
	EndpointPath = "/mcp"

	sessionHeader  = "Mcp-Session-Id"
	versionHeader  = "MCP-Protocol-Version"
	httpTimeout    = 120 * time.Second
	sseContentType = "text/event-stream"
)

// httpConn speaks streamable HTTP to one server: either a hosted URL or a
// Unix socket (DialSocket), which is how the manager reaches a first-party
// subprocess plugin that speaks MCP.
type httpConn struct {
	url      string
	headers  map[string]string
	hc       *http.Client
	spillDir string

	mu        sync.Mutex
	nextID    int
	sessionID string
	closed    bool

	// done closes on Stop. An HTTP server has no process to outlive us, so this
	// is the only way it "exits" — which keeps the connection interface uniform
	// with stdio without pretending there is a child to watch.
	done     chan struct{}
	stopOnce sync.Once
}

// DialHTTP reaches a hosted MCP server over streamable HTTP.
func DialHTTP(url string, headers map[string]string, opts ...ConnOption) Conn {
	var cfg connConfig
	for _, o := range opts {
		o(&cfg)
	}
	return &httpConn{
		url:      url,
		headers:  headers,
		hc:       &http.Client{Timeout: httpTimeout},
		spillDir: cfg.spillDir,
		done:     make(chan struct{}),
	}
}

// DialSocket speaks streamable HTTP to a server listening on a Unix socket,
// the default transport for first-party subprocess plugins (adr/modular_nine.md
// §9.6). The pseudo-host is ignored by the dialer, which always dials
// socketPath.
func DialSocket(socketPath string, opts ...ConnOption) Conn {
	var cfg connConfig
	for _, o := range opts {
		o(&cfg)
	}
	var d net.Dialer
	return &httpConn{
		url:      "http://unix" + EndpointPath,
		headers:  nil,
		spillDir: cfg.spillDir,
		hc: &http.Client{
			Timeout: httpTimeout,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return d.DialContext(ctx, "unix", socketPath)
				},
			},
		},
		done: make(chan struct{}),
	}
}

// Exited implements Conn.
func (c *httpConn) Exited() <-chan struct{} { return c.done }

// SpillDir implements Conn.
func (c *httpConn) SpillDir() string { return c.spillDir }

// StoppedDeliberately implements Conn.
func (c *httpConn) StoppedDeliberately() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// Stop implements Conn.
func (c *httpConn) Stop() error {
	c.stopOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		c.hc.CloseIdleConnections()
		close(c.done)
	})
	return nil
}

// Call sends one JSON-RPC request and returns its result.
func (c *httpConn) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("MCP connection stopped")
	}
	c.nextID++
	id := c.nextID
	c.mu.Unlock()

	body, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	resp, err := c.post(ctx, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // response body close

	// initialize is where a server hands out the session it wants echoed back.
	if sid := resp.Header.Get(sessionHeader); sid != "" {
		c.mu.Lock()
		c.sessionID = sid
		c.mu.Unlock()
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("HTTP %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}

	if strings.Contains(resp.Header.Get("Content-Type"), sseContentType) {
		return readJSONRPCFromSSE(resp.Body, id)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	return resultFromJSONRPC(raw)
}

// Notify sends a JSON-RPC notification. A server acknowledges with 202 and no
// body, so there is nothing to read back.
func (c *httpConn) Notify(method string, params any) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("MCP connection stopped")
	}
	c.mu.Unlock()

	body, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	resp, err := c.post(context.Background(), body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()                               //nolint:errcheck // response body close
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20)) //nolint:errcheck // drain so the connection can be reused

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	return nil
}

// post issues one request with the headers this transport carries: the
// operator's (typically an Authorization bearer), the negotiated protocol
// version, and the session id once the server has issued one.
func (c *httpConn) post(ctx context.Context, body []byte) (*http.Response, error) {
	// Unlike stdio, HTTP can cancel one request without disturbing the next, so
	// the caller's ctx rides the request directly.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Both are accepted because the server chooses which to send per request.
	req.Header.Set("Accept", "application/json, "+sseContentType)
	req.Header.Set(versionHeader, ProtocolVersionHTTP)
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}

	c.mu.Lock()
	sid := c.sessionID
	c.mu.Unlock()
	if sid != "" {
		req.Header.Set(sessionHeader, sid)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", c.url, err)
	}
	return resp, nil
}

// readJSONRPCFromSSE pulls our response out of an SSE stream, skipping events
// that are not it — a server may interleave notifications and progress on the
// same stream, and only the frame carrying our id is the answer.
func readJSONRPCFromSSE(body io.Reader, wantID int) (json.RawMessage, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, scannerMaxBytes), scannerMaxBytes)

	for scanner.Scan() {
		line := scanner.Text()
		// SSE frames are `field: value`, separated by blank lines. Only data
		// carries the payload; event/id/retry are framing we do not need.
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		payload := strings.TrimSpace(data)
		if payload == "" {
			continue
		}

		var probe struct {
			ID     *int            `json:"id"`
			Result json.RawMessage `json:"result,omitempty"`
			Error  *rpcError       `json:"error,omitempty"`
			Method string          `json:"method,omitempty"`
		}
		if err := json.Unmarshal([]byte(payload), &probe); err != nil {
			continue // not JSON-RPC; some servers send comments or keep-alives
		}
		// A frame with a method is a request or notification from the server, not
		// our reply — even when it carries an id.
		if probe.Method != "" || probe.ID == nil || *probe.ID != wantID {
			continue
		}
		if probe.Error != nil {
			return nil, fmt.Errorf("MCP error %d: %s", probe.Error.Code, probe.Error.Message)
		}
		return probe.Result, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read SSE stream: %w", err)
	}
	return nil, fmt.Errorf("SSE stream ended without a response to request %d", wantID)
}

// resultFromJSONRPC unwraps a plain (non-SSE) JSON-RPC response body.
func resultFromJSONRPC(raw []byte) (json.RawMessage, error) {
	var resp struct {
		Result json.RawMessage `json:"result,omitempty"`
		Error  *rpcError       `json:"error,omitempty"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("MCP error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	return resp.Result, nil
}
