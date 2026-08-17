package toolvm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// Defaults for the net.http capability, applied when a grant leaves them unset.
const (
	// DefaultHTTPMaxBytes caps a response body. A tool's output is capped again
	// downstream by the dispatcher, but that happens after the bytes are already
	// in the daemon's memory — this is the bound that keeps them out.
	DefaultHTTPMaxBytes int64 = 1 << 20 // 1 MiB

	// DefaultHTTPTimeout bounds one request. It is deliberately below the 5s
	// per-call deadline so a slow host surfaces as a legible HTTP timeout rather
	// than as the whole tool being killed.
	DefaultHTTPTimeout = 4 * time.Second

	// maxRedirects bounds redirect chasing. Every hop is fully re-validated, so
	// this is about bounding work, not safety.
	maxRedirects = 5
)

// HTTPGrant is the resolved `net.http` capability for one tool.
type HTTPGrant struct {
	// AllowHosts is the hostname allowlist. Never empty for a granted tool, and
	// never a bare "*" — config validation refuses both.
	AllowHosts []string
	// Methods is the permitted HTTP method allowlist, upper-cased.
	Methods []string
	// MaxBytes caps the response body; 0 uses DefaultHTTPMaxBytes.
	MaxBytes int64
}

// httpRequest is what the guest sends. Deliberately small: a URL, a method,
// headers, and a body. There is no streaming, no redirect control, and no
// transport tuning, because every one of those would be another lever pointed at
// the boundary in this file.
type httpRequest struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`

	// BodyB64 sends bytes rather than text. Everything crossing the ABI is UTF-8
	// JSON, so a string field cannot carry arbitrary bytes — a PNG put in `body`
	// arrives mangled or not at all. When set it wins over Body.
	BodyB64 string `json:"body_b64,omitempty"`
}

// httpResponse is what comes back. An `error` field carries a refusal, so the
// guest sees a normal value it can branch on rather than a trap.
type httpResponse struct {
	Status  int               `json:"status,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	Error   string            `json:"error,omitempty"`

	// BodyB64 carries a response body that is not valid UTF-8, and is set
	// *instead of* Body when so — never both, so a guest branches on which field
	// is present rather than guessing.
	//
	// This decision is made here, on the host, because here is the last place the
	// original bytes exist. Marshalling them into Body would replace every
	// invalid byte with U+FFFD before any guest could see them: a PNG's leading
	// 0x89 becomes 0xEF 0xBF 0xBD, `res.ok` stays true, and nothing reports a
	// problem. Silent corruption is worse than a refusal and much worse than a
	// second field.
	BodyB64 string `json:"body_b64,omitempty"`
}

// forbiddenRequestHeaders are headers the guest may not set. Host and the
// hop-by-hop set would let a tool desynchronize the connection or lie about its
// target — request smuggling by another name — and Content-Length is computed
// from the body we actually send.
var forbiddenRequestHeaders = map[string]bool{
	"host":              true,
	"content-length":    true,
	"connection":        true,
	"proxy-connection":  true,
	"transfer-encoding": true,
	"upgrade":           true,
	"te":                true,
	"trailer":           true,
	"keep-alive":        true,
}

// newHTTPClient builds the client a granted tool's requests go through.
//
// The security-critical line is Control. It runs after DNS resolution and
// immediately before connect, and receives the address actually being dialed —
// so validating there closes the rebinding window completely. Validating a
// resolved IP any earlier leaves a gap in which the name can be re-resolved to
// something else; there is no such gap here, and this is why the check does not
// live next to the URL parsing where it would read more naturally.
//
// It also covers every hop for free: a redirect opens a new connection, and
// Control runs again.
func newHTTPClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout:   timeout,
		KeepAlive: -1, // no pooling across calls; each call is its own world
		Control: func(network, address string, _ syscall.RawConn) error {
			switch network {
			case "tcp", "tcp4", "tcp6":
			default:
				return fmt.Errorf("blocked: network %q is not permitted", network)
			}
			return checkAddr(address)
		},
	}

	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: dialer.DialContext,
			// A proxy would resolve and connect on our behalf, which would route
			// around Control entirely — the one hole that would make everything
			// above decorative. Never take one from the environment.
			Proxy:                 nil,
			DisableKeepAlives:     true,
			DisableCompression:    false,
			MaxIdleConns:          0,
			TLSHandshakeTimeout:   timeout,
			ResponseHeaderTimeout: timeout,
			ExpectContinueTimeout: time.Second,
		},
	}
}

// checkRedirect enforces §8 items 5 and 6 on every hop.
func checkRedirect(grant HTTPGrant) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("blocked: more than %d redirects", maxRedirects)
		}
		// Item 5: the allowlist is re-checked per hop. A redirect is an attacker-
		// controlled URL — treating the first check as covering the chain is how
		// an allowed host becomes an open redirect into anywhere.
		if err := checkRequestURL(req.URL, grant); err != nil {
			return err
		}
		// Item 6: credentials must not follow a hop to a different origin. Go
		// already strips some headers cross-*domain*, but a scheme or port change
		// within one domain is still a different origin and is not covered.
		prev := via[len(via)-1]
		if !sameOrigin(prev.URL, req.URL) {
			req.Header.Del("Authorization")
			req.Header.Del("Cookie")
			req.Header.Del("Proxy-Authorization")
		}
		return nil
	}
}

func sameOrigin(a, b *url.URL) bool {
	return a.Scheme == b.Scheme && a.Hostname() == b.Hostname() && a.Port() == b.Port()
}

// checkRequestURL applies gate 1 (scheme + hostname allowlist) to one URL. Gate
// 2 — the resolved IP — happens in the dialer.
func checkRequestURL(u *url.URL, grant HTTPGrant) error {
	switch u.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("blocked: scheme %q is not permitted (http and https only)", u.Scheme)
	}
	if u.User != nil {
		// user:pass@host is a classic parser-confusion vector: what a human reads
		// as the host and what the client dials can differ.
		return fmt.Errorf("blocked: credentials in the URL are not permitted")
	}
	return allowHost(u.Hostname(), grant.AllowHosts)
}

// doHTTP runs one guest request through the whole §8 checklist and returns the
// response the guest sees. It never returns a Go error: a refusal is a value in
// httpResponse.Error, so a blocked call reaches the tool as something it can
// branch on and report, not as a crash.
func (h *Host) doHTTP(ctx context.Context, toolName string, grant HTTPGrant, raw []byte) httpResponse {
	var req httpRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return httpResponse{Error: "malformed request: " + err.Error()}
	}

	// 1. Method allowlist.
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}
	if !methodAllowed(method, grant.Methods) {
		return httpResponse{Error: fmt.Sprintf(
			"blocked: method %s is not permitted for this tool (allowed: %s)",
			method, strings.Join(grant.Methods, ", "))}
	}

	u, err := url.Parse(strings.TrimSpace(req.URL))
	if err != nil {
		return httpResponse{Error: "invalid url: " + err.Error()}
	}
	// 2–4. Scheme, hostname allowlist here; the resolved-IP rejection happens in
	// the dialer, on the address actually being connected.
	if err := checkRequestURL(u, grant); err != nil {
		return httpResponse{Error: err.Error()}
	}

	timeout := DefaultHTTPTimeout
	if h.timeout > 0 && h.timeout < timeout {
		timeout = h.timeout
	}
	maxBytes := grant.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultHTTPMaxBytes
	}

	var body io.Reader
	switch {
	case req.BodyB64 != "":
		raw, err := base64.StdEncoding.DecodeString(req.BodyB64)
		if err != nil {
			return httpResponse{Error: "invalid request: body_b64 is not valid base64"}
		}
		body = bytes.NewReader(raw)
	case req.Body != "":
		body = strings.NewReader(req.Body)
	}
	hreq, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return httpResponse{Error: "invalid request: " + err.Error()}
	}
	for k, v := range req.Headers {
		if forbiddenRequestHeaders[strings.ToLower(strings.TrimSpace(k))] {
			return httpResponse{Error: fmt.Sprintf("blocked: header %q may not be set by a tool", k)}
		}
		hreq.Header.Set(k, v)
	}
	if hreq.Header.Get("User-Agent") == "" {
		hreq.Header.Set("User-Agent", "nine-sandboxed-tool/"+toolName)
	}

	client := newHTTPClient(timeout)
	// 5–6. Per-hop revalidation and credential stripping.
	client.CheckRedirect = checkRedirect(grant)

	start := time.Now()
	resp, err := client.Do(hreq)
	if err != nil {
		// A refusal from Control or CheckRedirect arrives wrapped in a
		// *url.Error; unwrapping keeps the reason readable to the model rather
		// than burying it under Go's transport prose.
		return httpResponse{Error: unwrapBlocked(err)}
	}
	defer resp.Body.Close() //nolint:errcheck

	// 7. Cap the body. LimitReader with one extra byte so an over-long response
	// can be reported as truncated rather than silently cut.
	buf, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	truncated := int64(len(buf)) > maxBytes
	if truncated {
		buf = buf[:maxBytes]
	}
	if readErr != nil && !truncated {
		return httpResponse{Error: "reading response: " + readErr.Error()}
	}

	// 8. Journal the call: tool, method, host, status, bytes. "What did this tool
	// reach, and what came back" must be answerable after the fact.
	h.auditHTTP(HTTPCall{
		Tool:      toolName,
		Method:    method,
		URL:       resp.Request.URL.Redacted(),
		Host:      resp.Request.URL.Hostname(),
		Status:    resp.StatusCode,
		Bytes:     len(buf),
		Truncated: truncated,
		Duration:  time.Since(start),
	})

	out := httpResponse{
		Status:  resp.StatusCode,
		Headers: flattenHeaders(resp.Header),
	}
	// Text stays text — the overwhelmingly common case, and one no tool should
	// have to decode. Anything else goes back as bytes rather than through a
	// lossy conversion nobody asked for. A truncated body is checked as it
	// stands: cutting a multi-byte rune in half genuinely does make the result
	// not-UTF-8, and handing back the bytes is the honest answer there too.
	if utf8.Valid(buf) {
		out.Body = string(buf)
	} else {
		out.BodyB64 = base64.StdEncoding.EncodeToString(buf)
	}
	if truncated {
		out.Headers["x-nine-truncated"] = fmt.Sprintf("body capped at %d bytes", maxBytes)
	}
	return out
}

// HTTPCall is one audited outbound request (§8 item 8, §9.3).
type HTTPCall struct {
	Tool      string
	Method    string
	URL       string
	Host      string
	Status    int
	Bytes     int
	Truncated bool
	Duration  time.Duration
}

// auditHTTP records an outbound call. The structured log line is unconditional
// and is what ships.
//
// Config.AuditHTTP is the hook for routing the same record into the session
// event journal (docs/event-log.md), which this package cannot do — it has no
// store and should not grow one. It is currently unwired by the daemon, because
// a journal entry wants a session id and the dispatcher does not carry one into
// a tool call. So "what did this tool reach" is answerable from the log today;
// "which turn asked for it" is not.
func (h *Host) auditHTTP(c HTTPCall) {
	slog.Info("sandboxed tool http",
		"tool", c.Tool, "method", c.Method, "host", c.Host, "url", c.URL,
		"status", c.Status, "bytes", c.Bytes, "truncated", c.Truncated,
		"duration_ms", c.Duration.Milliseconds())
	if h.cfg.AuditHTTP != nil {
		h.cfg.AuditHTTP(c)
	}
}

// unwrapBlocked pulls our own refusal text out of the transport's error
// wrapping, so "blocked: 169.254.169.254 is link-local" reaches the model
// instead of a dial-tcp stack trace.
func unwrapBlocked(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, "blocked: "); i >= 0 {
		return msg[i:]
	}
	var ue *url.Error
	if ok := asURLError(err, &ue); ok && ue.Timeout() {
		return "request timed out"
	}
	return "request failed: " + msg
}

func asURLError(err error, target **url.Error) bool {
	for err != nil {
		if ue, ok := err.(*url.Error); ok { //nolint:errorlint // walking the chain by hand below
			*target = ue
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// flattenHeaders reduces the response headers to a flat map. Set-Cookie is
// dropped rather than flattened: a tool has no cookie jar (each call is a fresh
// instance), so the only thing handing it session cookies could achieve is
// leaking them into the model's context.
func flattenHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if strings.EqualFold(k, "Set-Cookie") || len(v) == 0 {
			continue
		}
		out[strings.ToLower(k)] = v[0]
	}
	return out
}

func methodAllowed(method string, allowed []string) bool {
	for _, m := range allowed {
		if strings.EqualFold(strings.TrimSpace(m), method) {
			return true
		}
	}
	return false
}
