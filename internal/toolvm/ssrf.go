package toolvm

import (
	"fmt"
	"net/netip"
	"strings"
)

// This file is the reason `net.http` is the one capability that needed real
// work (docs/sandboxed-tools.md §8). The filesystem is easy — a wazero pre-open
// is a battle-tested capability primitive that wazero enforces without our help.
// The network has no such primitive: wazero has no network at all, so every byte
// of this boundary is ours, and getting it wrong turns a sandboxed tool into an
// SSRF primitive with a nice manifest.
//
// The rule that matters most, and the one that is always forgotten:
//
//	Blocking by HOSTNAME is not enough. The check must be on the RESOLVED IP,
//	immediately before connect, and re-done for every connection — including
//	each redirect hop — or DNS rebinding walks straight through it.
//
// Hence two independent gates, both of which must pass:
//
//  1. The URL's hostname must match the operator's allowlist (allowHost, below).
//  2. The IP actually being dialed must be publicly routable (checkAddr, wired
//     into net.Dialer.Control in nethttp.go — which runs after resolution and
//     immediately before connect, so there is no window between the two).
//
// Gate 1 alone is defeated by a hostname that resolves wherever the attacker
// likes. Gate 2 alone would let a tool reach any public host. Neither is
// redundant.

// blockedRanges are the CIDR blocks a sandboxed tool may never reach, beyond
// what netip's own predicates cover. Each is here for a reason, and the reason
// is worth keeping next to the block.
var blockedRanges = []struct {
	prefix netip.Prefix
	why    string
}{
	// THE one. Every major cloud serves instance credentials from here:
	// http://169.254.169.254/latest/meta-data/iam/security-credentials/ on AWS,
	// metadata.google.internal on GCP. Without this block the sandbox has bought
	// nothing — the tool never escaped the VM, it just asked the VM politely for
	// its credentials. netip.Addr.IsLinkLocalUnicast covers it too; it is spelled
	// out here so that a future edit cannot quietly drop it.
	{netip.MustParsePrefix("169.254.0.0/16"), "link-local (cloud instance metadata)"},
	{netip.MustParsePrefix("fe80::/10"), "IPv6 link-local"},

	// RFC 1918 / RFC 4193: the rest of the internal network the daemon sits on.
	{netip.MustParsePrefix("10.0.0.0/8"), "private network"},
	{netip.MustParsePrefix("172.16.0.0/12"), "private network"},
	{netip.MustParsePrefix("192.168.0.0/16"), "private network"},
	{netip.MustParsePrefix("fc00::/7"), "IPv6 unique local address"},

	// Loopback: the daemon's own socket, and anything else bound to localhost.
	{netip.MustParsePrefix("127.0.0.0/8"), "loopback"},
	{netip.MustParsePrefix("::1/128"), "IPv6 loopback"},

	// Not publicly routable, and each has been used to smuggle past naive
	// filters at least once.
	{netip.MustParsePrefix("0.0.0.0/8"), "this-network"},
	{netip.MustParsePrefix("100.64.0.0/10"), "carrier-grade NAT"},
	{netip.MustParsePrefix("192.0.0.0/24"), "IETF protocol assignments"},
	{netip.MustParsePrefix("192.0.2.0/24"), "TEST-NET-1"},
	{netip.MustParsePrefix("198.18.0.0/15"), "benchmark network"},
	{netip.MustParsePrefix("198.51.100.0/24"), "TEST-NET-2"},
	{netip.MustParsePrefix("203.0.113.0/24"), "TEST-NET-3"},
	{netip.MustParsePrefix("240.0.0.0/4"), "reserved"},
	{netip.MustParsePrefix("255.255.255.255/32"), "broadcast"},
	{netip.MustParsePrefix("::/128"), "unspecified"},
	{netip.MustParsePrefix("2001:db8::/32"), "IPv6 documentation"},
	{netip.MustParsePrefix("64:ff9b::/96"), "NAT64 (would translate to any IPv4)"},
	{netip.MustParsePrefix("100::/64"), "IPv6 discard-only"},
}

// checkIP reports whether a sandboxed tool may connect to addr.
//
// The first thing it does is Unmap(), and that is not a formality: an IPv4
// address written as an IPv6-mapped literal (`::ffff:127.0.0.1`) is the classic
// way past a filter that only knows about `127.0.0.0/8`. Unmapping first means
// every block below is checked against the address's real family.
func checkIP(addr netip.Addr) error {
	if !addr.IsValid() {
		return fmt.Errorf("invalid IP address")
	}
	addr = addr.Unmap()

	// The stdlib predicates first: they are maintained, and they catch shapes
	// (interface-local multicast, for one) not worth restating as CIDRs.
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

	for _, b := range blockedRanges {
		if b.prefix.Contains(addr) {
			return fmt.Errorf("blocked: %s is %s", addr, b.why)
		}
	}

	// An IPv6 address carrying a zone (`fe80::1%eth0`) is scoped to an interface
	// on this host by definition, so it cannot be a legitimate remote target.
	if addr.Zone() != "" {
		return fmt.Errorf("blocked: %s is interface-scoped", addr)
	}

	return nil
}

// testDialCheck, when non-nil, replaces the connect-time gate below.
//
// It exists because httptest binds to loopback, which this file blocks by
// design — so the happy path is otherwise untestable end to end. It is nil in
// production and there is deliberately NO path from Config, nine.toml, or an
// environment variable that sets it: only code inside this package can, and only
// _test.go files do. TestDialGateHasNoProductionEscapeHatch enforces exactly
// that by scanning the non-test sources, so this cannot quietly become a real
// hole later.
var testDialCheck func(string) error

// checkAddr validates a "host:port" as handed to a dialer. It is the gate-2
// entry point, called from net.Dialer.Control with the address about to be
// connected — after DNS, so a hostname that resolves somewhere hostile is caught
// here no matter what it claimed to be.
func checkAddr(address string) error {
	if testDialCheck != nil {
		return testDialCheck(address)
	}
	host, _, err := splitHostPort(address)
	if err != nil {
		return err
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		// Control is always handed a resolved literal. A name reaching here means
		// resolution was bypassed somehow, and the safe reading of "I do not
		// understand this address" is to refuse it.
		return fmt.Errorf("blocked: %q is not a resolved IP address", host)
	}
	return checkIP(addr)
}

// splitHostPort is net.SplitHostPort, tolerating a bare host.
func splitHostPort(address string) (host, port string, err error) {
	if i := strings.LastIndex(address, ":"); i >= 0 && !strings.Contains(address[i+1:], "]") {
		host, port = address[:i], address[i+1:]
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
		if host == "" {
			return "", "", fmt.Errorf("blocked: empty host in %q", address)
		}
		return host, port, nil
	}
	if address == "" {
		return "", "", fmt.Errorf("blocked: empty address")
	}
	return address, "", nil
}

// allowHost reports whether hostname matches the operator's allowlist — gate 1.
//
// Matching is on the name, case-insensitively, with exactly two forms:
//
//	"api.example.com"    exact, and only that name
//	"*.example.com"      any subdomain, but NOT the apex
//
// The apex exclusion is deliberate and is the stricter reading: an operator who
// wants both writes both. Getting a host they did not intend is the failure that
// matters here; having to write one more line is not.
//
// A bare "*" permits any hostname, and is the one pattern an operator has to
// write deliberately. It used to be refused here, on the reasoning that anyone
// wanting unrestricted egress should write a native plugin instead — but that
// escape hatch pointed at *less* safety, not more: a plugin is a subprocess with
// the daemon's uid and none of this file's checks. Nine's own fetching tools are
// the case in point; they exist to retrieve whatever URL a model chose, which no
// host list expresses (R-TVM.12).
//
// It stays safe because the allowlist is not what makes egress safe. Every
// connection is checked at dial time by checkAddr, in the dialer's Control hook,
// so "*" permits arbitrary *public hosts* and never arbitrary *addresses*:
// loopback, link-local (cloud instance metadata), private ranges and multicast
// remain blocked, and being at dial time the check also survives DNS rebinding
// and redirects.
func allowHost(hostname string, allow []string) error {
	h := strings.ToLower(strings.TrimSuffix(hostname, "."))
	if h == "" {
		return fmt.Errorf("blocked: empty hostname")
	}

	for _, pattern := range allow {
		p := strings.ToLower(strings.TrimSpace(pattern))
		switch {
		case p == "": // ignore blank entries rather than matching everything
		case p == "*":
			return nil // any host; the address checks still apply at dial time
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
