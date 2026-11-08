package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// The network half of the shipped tier in Go: http_get/http_post/web_page_read/
// web_search, with the SSRF boundary ported from the sandbox host
// (internal/toolvm/ssrf.go). Two gates, both of which must pass — the URL
// hostname against the tool's allowlist, and the resolved IP at dial time
// (after DNS, per connection, including redirect hops), because a hostname-only
// check is defeated by DNS rebinding and a URL-only check by redirects.

// blockedRanges are the CIDR blocks a shipped tool may never reach, beyond what
// netip's own predicates cover. See internal/toolvm/ssrf.go for the per-block
// rationale (169.254.0.0/16 is cloud instance metadata, and it is spelled out so
// a future edit cannot quietly drop it).
var blockedRanges = []netip.Prefix{
	netip.MustParsePrefix("169.254.0.0/16"),  // link-local (cloud instance metadata)
	netip.MustParsePrefix("fe80::/10"),       // IPv6 link-local
	netip.MustParsePrefix("10.0.0.0/8"),      // private
	netip.MustParsePrefix("172.16.0.0/12"),   // private
	netip.MustParsePrefix("192.168.0.0/16"),  // private
	netip.MustParsePrefix("fc00::/7"),        // IPv6 unique local
	netip.MustParsePrefix("127.0.0.0/8"),     // loopback
	netip.MustParsePrefix("::1/128"),         // IPv6 loopback
	netip.MustParsePrefix("0.0.0.0/8"),       // this-network
	netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmark
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved
	netip.MustParsePrefix("255.255.255.255/32"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64
	netip.MustParsePrefix("100::/64"),     // IPv6 discard-only
}

// checkIP reports whether a shipped tool may connect to addr.
func checkIP(addr netip.Addr) error {
	if !addr.IsValid() {
		return fmt.Errorf("blocked: invalid IP address")
	}
	addr = addr.Unmap() // ::ffff:127.0.0.1 is the classic filter bypass
	switch {
	case addr.IsLoopback():
		return fmt.Errorf("blocked: %s is loopback", addr)
	case addr.IsUnspecified():
		return fmt.Errorf("blocked: %s is unspecified", addr)
	case addr.IsLinkLocalUnicast():
		return fmt.Errorf("blocked: %s is link-local (cloud instance metadata lives here)", addr)
	case addr.IsLinkLocalMulticast(), addr.IsInterfaceLocalMulticast(), addr.IsMulticast():
		return fmt.Errorf("blocked: %s is multicast", addr)
	case addr.IsPrivate():
		return fmt.Errorf("blocked: %s is a private address", addr)
	}
	for _, p := range blockedRanges {
		if p.Contains(addr) {
			return fmt.Errorf("blocked: %s is in %s", addr, p)
		}
	}
	if addr.Zone() != "" {
		return fmt.Errorf("blocked: %s is interface-scoped", addr)
	}
	return nil
}

// checkDialAddr validates a "host:port" handed to the dialer's Control hook —
// always a resolved literal at that point.
func checkDialAddr(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("blocked: %q is not a resolved IP address", host)
	}
	return checkIP(addr)
}

// allowHost reports whether hostname matches the allowlist — gate 1. "*"
// permits any hostname (the dial-time address checks still apply); "*.x" covers
// subdomains but not the apex; otherwise exact, case-insensitive.
func allowHost(hostname string, allow []string) error {
	h := strings.ToLower(strings.TrimSuffix(hostname, "."))
	if h == "" {
		return fmt.Errorf("blocked: empty hostname")
	}
	for _, pattern := range allow {
		p := strings.ToLower(strings.TrimSpace(pattern))
		switch {
		case p == "":
		case p == "*":
			return nil
		case strings.HasPrefix(p, "*."):
			if strings.HasSuffix(h, p[1:]) && len(h) > len(p)-1 {
				return nil
			}
		case p == h:
			return nil
		}
	}
	return fmt.Errorf("blocked: host %q is not in this tool's allow_hosts", hostname)
}

// shippedHTTPClient is an http.Client whose every connection passes the SSRF
// dial-time gate, and whose redirects re-run gate 1 per hop.
func shippedHTTPClient(allowHosts []string, methods []string, timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(network, address string, c syscall.RawConn) error {
			return checkDialAddr(address)
		},
	}
	tr := &http.Transport{
		DialContext:     dialer.DialContext,
		MaxIdleConns:    16,
		IdleConnTimeout: 30 * time.Second,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			return allowHost(req.URL.Hostname(), allowHosts)
		},
	}
}

// fetchURL performs one gated request: gate 1 on the URL host, the method
// allowlist, then the dial-time gate per connection.
func fetchURL(ctx context.Context, client *http.Client, allowHosts, methods []string, method, rawURL string, headers map[string]string, body io.Reader) (int, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0, "", err
	}
	if err := allowHost(u.Hostname(), allowHosts); err != nil {
		return 0, "", err
	}
	if len(methods) > 0 && !containsStr(methods, method) {
		return 0, "", fmt.Errorf("blocked: method %s is not allowed for this tool", method)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return 0, "", err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close() //nolint:errcheck
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return 0, "", err
	}
	return resp.StatusCode, string(b), nil
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// shippedHTTPGet implements http_get.
func shippedHTTPGet(ctx context.Context, args json.RawMessage) (string, error) {
	var req struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return "", fmt.Errorf("http_get: %w", err)
	}
	// "*" grants any host; the dial-time address checks still refuse loopback,
	// link-local, private ranges and multicast.
	client := shippedHTTPClient([]string{"*"}, []string{"GET"}, 60*time.Second)
	status, body, err := fetchURL(ctx, client, []string{"*"}, []string{"GET"}, "GET", req.URL, req.Headers, nil)
	if err != nil {
		return "", err
	}
	out, _ := json.Marshal(map[string]any{"status": status, "body": body})
	return string(out), nil
}

// shippedHTTPPost implements http_post.
func shippedHTTPPost(ctx context.Context, args json.RawMessage) (string, error) {
	var req struct {
		URL     string            `json:"url"`
		Body    json.RawMessage   `json:"body"`
		Headers map[string]string `json:"headers"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return "", fmt.Errorf("http_post: %w", err)
	}
	body := string(req.Body)
	if body == "" {
		body = "{}"
	}
	if !json.Valid(req.Body) && len(req.Body) > 0 && req.Body[0] != '"' {
		// a non-string JSON value: pass it through as-is
		body = string(req.Body)
	}
	headers := map[string]string{"Content-Type": "application/json"}
	for k, v := range req.Headers {
		headers[k] = v
	}
	client := shippedHTTPClient([]string{"*"}, []string{"POST"}, 60*time.Second)
	status, respBody, err := fetchURL(ctx, client, []string{"*"}, []string{"POST"}, "POST", req.URL, headers, strings.NewReader(body))
	if err != nil {
		return "", err
	}
	out, _ := json.Marshal(map[string]any{"status": status, "body": respBody})
	return string(out), nil
}
