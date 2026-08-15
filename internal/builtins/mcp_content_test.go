package builtins

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A tools/call result may carry more than text, and everything that was not
// text used to be dropped. The case that made it matter is Playwright's
// browser_take_screenshot, which answers with a text summary *and* the image:
// the model got a confident description of a picture it never received, with no
// way to tell. These pin that nothing is silently discarded.

// pngContent is a one-pixel PNG as an MCP image part.
func pngContent(t *testing.T) mcpContent {
	t.Helper()
	raw := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x01, 0x02}
	return mcpContent{Type: "image", MimeType: "image/png", Data: base64.StdEncoding.EncodeToString(raw)}
}

// TestFlattenScreenshotShape uses the exact response shape observed from
// @playwright/mcp v0.0.79: a markdown summary, then the image.
func TestFlattenScreenshotShape(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NINE_PLUGIN_CACHE_DIR", dir)

	summary := "### Result\n- [Screenshot of viewport](.playwright-mcp/page.png)\n"
	got := flattenMCPContent("browser_take_screenshot", []mcpContent{
		{Type: "text", Text: summary},
		pngContent(t),
	})

	if !strings.Contains(got, summary) {
		t.Errorf("text part was lost:\n%s", got)
	}
	if !strings.Contains(got, "image/png") {
		t.Errorf("image part is not reported:\n%s", got)
	}

	// The bytes have to actually be somewhere, not just described.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read cache dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("wrote %d files to the cache dir, want 1", len(entries))
	}
	saved := filepath.Join(dir, entries[0].Name())
	if !strings.Contains(got, saved) {
		t.Errorf("saved path %q is not named in the output:\n%s", saved, got)
	}
	if b, err := os.ReadFile(saved); err != nil || len(b) != 11 {
		t.Errorf("saved file = %d bytes, err=%v; want the decoded image", len(b), err)
	}
	if ext := filepath.Ext(saved); ext != ".png" {
		t.Errorf("saved extension = %q, want .png", ext)
	}
}

// Base64 in the text stream would be the lazy alternative; it would also burn a
// context window on bytes no model reads. The path must be there instead.
func TestFlattenDoesNotInlineBase64(t *testing.T) {
	t.Setenv("NINE_PLUGIN_CACHE_DIR", t.TempDir())
	img := pngContent(t)

	got := flattenMCPContent("shot", []mcpContent{img})

	if strings.Contains(got, img.Data) {
		t.Errorf("base64 payload was inlined into the tool output:\n%s", got)
	}
}

// With no cache dir there is nowhere to put the bytes, but silence is still the
// wrong answer: the model must be able to tell something was not delivered.
func TestFlattenReportsImageWithoutCacheDir(t *testing.T) {
	t.Setenv("NINE_PLUGIN_CACHE_DIR", "")

	got := flattenMCPContent("shot", []mcpContent{pngContent(t)})

	for _, want := range []string{"image", "image/png", "not saved"} {
		if !strings.Contains(got, want) {
			t.Errorf("output does not mention %q:\n%s", want, got)
		}
	}
}

// An embedded resource carrying text is ordinary content and should read as
// such; one carrying a blob is binary and takes the same route as an image.
func TestFlattenResourceKinds(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NINE_PLUGIN_CACHE_DIR", dir)

	textRes := mcpContent{Type: "resource"}
	textRes.Resource = &struct {
		URI      string `json:"uri,omitempty"`
		MimeType string `json:"mimeType,omitempty"`
		Text     string `json:"text,omitempty"`
		Blob     string `json:"blob,omitempty"`
	}{URI: "file:///a.txt", MimeType: "text/plain", Text: "embedded text"}

	blobRes := mcpContent{Type: "resource"}
	blobRes.Resource = &struct {
		URI      string `json:"uri,omitempty"`
		MimeType string `json:"mimeType,omitempty"`
		Text     string `json:"text,omitempty"`
		Blob     string `json:"blob,omitempty"`
	}{URI: "file:///a.bin", MimeType: "application/octet-stream",
		Blob: base64.StdEncoding.EncodeToString([]byte("bytes"))}

	got := flattenMCPContent("t", []mcpContent{textRes, blobRes})

	if !strings.Contains(got, "embedded text") {
		t.Errorf("resource text was dropped:\n%s", got)
	}
	if !strings.Contains(got, "application/octet-stream") {
		t.Errorf("resource blob was not reported:\n%s", got)
	}
}

// A content type MCP adds later must look like something missing, not nothing.
func TestFlattenUnknownTypeIsReported(t *testing.T) {
	got := flattenMCPContent("t", []mcpContent{{Type: "hologram"}})

	if !strings.Contains(got, "hologram") {
		t.Errorf("unknown content type was dropped silently:\n%s", got)
	}
}

func TestFlattenResourceLink(t *testing.T) {
	got := flattenMCPContent("t", []mcpContent{
		{Type: "resource_link", Name: "report", URI: "file:///r.pdf"},
	})

	for _, want := range []string{"report", "file:///r.pdf"} {
		if !strings.Contains(got, want) {
			t.Errorf("resource_link does not mention %q:\n%s", want, got)
		}
	}
}

func TestExtensionForMIME(t *testing.T) {
	cases := map[string]string{
		"image/png":                ".png",
		"image/jpeg":               ".jpeg",
		"image/svg+xml":            ".xml",
		"image/png; charset=utf8":  ".png",
		"application/octet-stream": ".bin", // hyphen is not [a-z0-9]
		"":                         ".bin",
		"nonsense":                 ".bin",
		"image/../../etc/passwd":   ".bin", // never let a mime type shape a path
	}
	for mime, want := range cases {
		if got := extensionForMIME(mime); got != want {
			t.Errorf("extensionForMIME(%q) = %q, want %q", mime, got, want)
		}
	}
}
