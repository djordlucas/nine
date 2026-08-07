package toolvm

import (
	"net/netip"
	"strings"
	"testing"
)

// The block list is the whole security argument for net.http, so it is tested
// exhaustively rather than representatively. Each case is an address that has
// been used to get past a naive filter at least once.
func TestBlockedAddresses(t *testing.T) {
	for _, tc := range []struct{ addr, why string }{
		// THE one. Without this the sandbox has bought nothing: the tool never
		// escaped the VM, it just asked the VM politely for its credentials.
		{"169.254.169.254", "AWS/GCP instance metadata"},
		{"169.254.170.2", "ECS task metadata"},
		{"169.254.0.1", "link-local generally"},
		{"fe80::1", "IPv6 link-local"},

		{"127.0.0.1", "loopback"},
		{"127.0.0.53", "systemd-resolved"},
		{"127.1.2.3", "loopback, non-obvious form"},
		{"::1", "IPv6 loopback"},

		{"10.0.0.1", "RFC1918"},
		{"10.255.255.255", "RFC1918 edge"},
		{"172.16.0.1", "RFC1918"},
		{"172.31.255.255", "RFC1918 edge"},
		{"192.168.1.1", "RFC1918"},
		{"fc00::1", "IPv6 ULA"},
		{"fd12:3456::1", "IPv6 ULA"},

		{"0.0.0.0", "unspecified — routes to localhost on Linux"},
		{"0.0.0.1", "this-network"},
		{"::", "IPv6 unspecified"},
		{"255.255.255.255", "broadcast"},
		{"224.0.0.1", "multicast"},
		{"ff02::1", "IPv6 multicast"},
		{"100.64.0.1", "carrier-grade NAT"},
		{"192.0.0.1", "IETF protocol assignments"},
		{"198.18.0.1", "benchmark range"},
		{"240.0.0.1", "reserved"},
		{"64:ff9b::7f00:1", "NAT64 — translates to 127.0.0.1"},

		// The classic bypass: an IPv4 address wearing an IPv6 costume. A filter
		// that checks 127.0.0.0/8 without unmapping first walks right past it.
		{"::ffff:127.0.0.1", "IPv4-mapped loopback"},
		{"::ffff:169.254.169.254", "IPv4-mapped metadata endpoint"},
		{"::ffff:10.0.0.1", "IPv4-mapped RFC1918"},
	} {
		t.Run(tc.addr, func(t *testing.T) {
			addr, err := netip.ParseAddr(tc.addr)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if err := checkIP(addr); err == nil {
				t.Errorf("%s (%s) was ALLOWED; it must be blocked", tc.addr, tc.why)
			}
		})
	}
}

// The block list must not be so broad that it makes the capability useless.
func TestPublicAddressesAreAllowed(t *testing.T) {
	for _, addr := range []string{
		"1.1.1.1",
		"8.8.8.8",
		"93.184.216.34",  // example.com
		"172.15.255.255", // just below RFC1918's 172.16/12
		"172.32.0.1",     // just above it
		"192.167.255.255",
		"192.169.0.1",
		"9.255.255.255",
		"11.0.0.1",
		"2606:4700:4700::1111", // Cloudflare v6
	} {
		t.Run(addr, func(t *testing.T) {
			a, err := netip.ParseAddr(addr)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if err := checkIP(a); err != nil {
				t.Errorf("%s was blocked (%v); the capability must remain usable", addr, err)
			}
		})
	}
}

// checkAddr is what the dialer calls, so it must handle the host:port shapes the
// transport actually produces — including bracketed IPv6.
func TestCheckAddrShapes(t *testing.T) {
	for _, tc := range []struct {
		addr    string
		blocked bool
	}{
		{"93.184.216.34:443", false},
		{"[2606:4700:4700::1111]:443", false},
		{"169.254.169.254:80", true},
		{"[::ffff:127.0.0.1]:80", true},
		{"[::1]:8080", true},
		{"127.0.0.1:5432", true},
		// A name reaching the dialer means resolution was bypassed. The safe
		// reading of "I do not understand this" is refusal.
		{"example.com:443", true},
		{"", true},
	} {
		t.Run(tc.addr, func(t *testing.T) {
			err := checkAddr(tc.addr)
			if tc.blocked && err == nil {
				t.Errorf("%q was allowed; want blocked", tc.addr)
			}
			if !tc.blocked && err != nil {
				t.Errorf("%q was blocked (%v); want allowed", tc.addr, err)
			}
		})
	}
}

func TestAllowHostMatching(t *testing.T) {
	allow := []string{"api.example.com", "*.cdn.example.net"}

	for _, tc := range []struct {
		host    string
		allowed bool
		why     string
	}{
		{"api.example.com", true, "exact"},
		{"API.EXAMPLE.COM", true, "case-insensitive"},
		{"api.example.com.", true, "trailing dot is the same name"},
		{"a.cdn.example.net", true, "wildcard subdomain"},
		{"deep.a.cdn.example.net", true, "wildcard is a suffix match"},

		{"cdn.example.net", false, "wildcard does not match the apex"},
		{"example.com", false, "parent of an exact entry"},
		{"other.example.com", false, "sibling of an exact entry"},
		{"evil.com", false, "unrelated"},
		{"", false, "empty"},
		// Suffix matching done carelessly lets an attacker register a name that
		// merely ends with the allowed string.
		{"api.example.com.evil.com", false, "allowed name as a prefix of a hostile one"},
		{"notapi.example.com", false, "allowed name as a suffix without a dot"},
		{"evilcdn.example.net", false, "wildcard must match on a label boundary"},
	} {
		t.Run(tc.host+"/"+tc.why, func(t *testing.T) {
			err := allowHost(tc.host, allow)
			if tc.allowed && err != nil {
				t.Errorf("%q was blocked (%v): %s", tc.host, err, tc.why)
			}
			if !tc.allowed && err == nil {
				t.Errorf("%q was ALLOWED: %s", tc.host, tc.why)
			}
		})
	}
}

// An empty allowlist grants nothing. Config validation refuses one, but the
// matcher must fail closed regardless of how it was reached.
func TestEmptyAllowlistBlocksEverything(t *testing.T) {
	for _, h := range []string{"example.com", "api.example.com", "*"} {
		if err := allowHost(h, nil); err == nil {
			t.Errorf("%q allowed against an empty allowlist", h)
		}
	}
}

// A blank entry must not act as a wildcard.
func TestBlankAllowlistEntryMatchesNothing(t *testing.T) {
	if err := allowHost("example.com", []string{"", "   "}); err == nil {
		t.Error("a blank allowlist entry matched a host")
	}
}

// The refusal text is what the model reads, so it should name the reason.
func TestBlockReasonsAreLegible(t *testing.T) {
	addr := netip.MustParseAddr("169.254.169.254")
	err := checkIP(addr)
	if err == nil {
		t.Fatal("want a refusal")
	}
	if !strings.Contains(err.Error(), "link-local") {
		t.Errorf("error %q should name the reason", err)
	}
}
