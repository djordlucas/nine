package tui

import (
	"strings"
	"testing"
)

// TestRenderLogoShowsBanner checks that the nine banner logo and version are rendered.
func TestRenderLogoShowsBanner(t *testing.T) {
	pal := darkPalette()

	var sb strings.Builder
	renderLogo(&sb, pal, 80, "v1.1.0")
	if !strings.Contains(sb.String(), "v1.1.0") {
		t.Errorf("logo should include version; got:\n%s", sb.String())
	}
	if !strings.Contains(sb.String(), "███") {
		t.Errorf("logo did not include the nine banner; got:\n%s", sb.String())
	}
}
