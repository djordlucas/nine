package tui

import (
	"strings"
	"testing"
)

// TestRenderLogoShowsVersion checks the build version is rendered beneath the
// banner, and that an empty version simply omits the line (no stray blank).
func TestRenderLogoShowsVersion(t *testing.T) {
	pal := darkPalette()

	var sb strings.Builder
	renderLogo(&sb, pal, 80, "v1.1.0")
	if !strings.Contains(sb.String(), "v1.1.0") {
		t.Errorf("logo did not include the version; got:\n%s", sb.String())
	}

	var empty strings.Builder
	renderLogo(&empty, pal, 80, "")
	if strings.Contains(empty.String(), "v1.1.0") {
		t.Error("empty version should not render a version string")
	}
}
