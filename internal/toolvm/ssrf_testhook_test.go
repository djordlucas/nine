package toolvm

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// allowTestLoopback relaxes the connect-time gate for loopback only, so a test
// can reach an httptest server. Everything else — link-local above all — stays
// blocked, which is what lets the metadata and redirect tests remain meaningful
// while it is in effect.
func allowTestLoopback(t *testing.T) {
	t.Helper()
	testDialCheck = func(address string) error {
		host, _, err := splitHostPort(address)
		if err != nil {
			return err
		}
		addr, err := netip.ParseAddr(host)
		if err != nil {
			return fmt.Errorf("blocked: %q is not a resolved IP address", host)
		}
		if addr.Unmap().IsLoopback() {
			return nil
		}
		return checkIP(addr)
	}
	t.Cleanup(func() { testDialCheck = nil })
}

// The escape hatch above is only acceptable if production genuinely cannot reach
// it. This asserts that by scanning every non-test source in the package: an
// assignment to testDialCheck outside a _test.go file would turn the SSRF gate
// into something an operator — or a bug — could switch off.
func TestDialGateHasNoProductionEscapeHatch(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
				continue
			}
			if strings.Contains(trimmed, "testDialCheck =") || strings.Contains(trimmed, "testDialCheck=") {
				t.Errorf("%s:%d assigns testDialCheck outside a test:\n\t%s", name, i+1, trimmed)
			}
		}
	}
}

// And it must be off unless a test turns it on.
func TestDialGateIsStrictByDefault(t *testing.T) {
	if testDialCheck != nil {
		t.Fatal("testDialCheck is set; the SSRF gate is relaxed outside a test that asked for it")
	}
	if err := checkAddr("127.0.0.1:80"); err == nil {
		t.Error("loopback was allowed by the default gate")
	}
	if err := checkAddr("169.254.169.254:80"); err == nil {
		t.Error("the metadata endpoint was allowed by the default gate")
	}
}
