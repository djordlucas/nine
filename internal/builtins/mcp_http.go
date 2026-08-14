package builtins

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The streamable-HTTP MCP transport, for servers that are reached at a URL
// rather than spawned as a subprocess (MCP revision 2025-03-26). Hosted MCP
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
	mcpSessionHeader  = "Mcp-Session-Id"
	mcpVersionHeader  = "MCP-Protocol-Version"
	mcpHTTPTimeout    = 120 * time.Second
	mcpSSEContentType = "text/event-stream"
)

type mcpHTTP struct {
	url     string
	headers map[string]string
	hc      *http.Client

	mu        sync.Mutex
	nextID    int
	sessionID string
	closed    bool

	// done closes on stop. An HTTP server has no process to outlive us, so this
	// is the only way it "exits" — which keeps the connection interface uniform
	// with stdio without pretending there is a child to watch.
	done     chan struct{}
	stopOnce sync.Once
}

func dialMCPHTTP(url string, headers map[string]string) *mcpHTTP {
	return &mcpHTTP{
		url:     url,
		headers: headers,
		hc:      &http.Client{Timeout: mcpHTTPTimeout},
		done:    make(chan struct{}),
	}
}

func (c *mcpHTTP) Exited() <-chan struct{} { return c.done }

func (c *mcpHTTP) StoppedDeliberately() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *mcpHTTP) stop() error {
	c.stopOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		c.hc.CloseIdleConnections()
		close(c.done)
	})
	return nil
}

// call sends one JSON-RPC request and returns its result.
func (c *mcpHTTP) call(method string, params any) (json.RawMessage, error) {
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

	resp, err := c.post(body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // response body close

	// initialize is where a server hands out the session it wants echoed back.
	if sid := resp.Header.Get(mcpSessionHeader); sid != "" {
		c.mu.Lock()
		c.sessionID = sid
		c.mu.Unlock()
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("HTTP %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}

	if strings.Contains(resp.Header.Get("Content-Type"), mcpSSEContentType) {
		return readJSONRPCFromSSE(resp.Body, id)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	return resultFromJSONRPC(raw)
}

// notify sends a JSON-RPC notification. A server acknowledges with 202 and no
// body, so there is nothing to read back.
func (c *mcpHTTP) notify(method string, params any) error {
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
	resp, err := c.post(body)
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
func (c *mcpHTTP) post(body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Both are accepted because the server chooses which to send per request.
	req.Header.Set("Accept", "application/json, "+mcpSSEContentType)
	req.Header.Set(mcpVersionHeader, mcpProtocolVersion)
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}

	c.mu.Lock()
	sid := c.sessionID
	c.mu.Unlock()
	if sid != "" {
		req.Header.Set(mcpSessionHeader, sid)
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
	scanner.Buffer(make([]byte, mcpScannerMaxBytes), mcpScannerMaxBytes)

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
			Error  *mcpRPCError    `json:"error,omitempty"`
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
		Error  *mcpRPCError    `json:"error,omitempty"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("MCP error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	return resp.Result, nil
}
