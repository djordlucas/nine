package mcp

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Content is one item of a tools/call result. MCP defines several kinds and a
// server may return a mix — Playwright's browser_take_screenshot answers with
// a text summary *and* an image in the same response.
type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`     // image/audio: base64
	MimeType string `json:"mimeType,omitempty"` // image/audio
	URI      string `json:"uri,omitempty"`      // resource_link
	Name     string `json:"name,omitempty"`     // resource_link
	Resource *struct {
		URI      string `json:"uri,omitempty"`
		MimeType string `json:"mimeType,omitempty"`
		Text     string `json:"text,omitempty"`
		Blob     string `json:"blob,omitempty"` // base64
	} `json:"resource,omitempty"`
}

// FlattenContent renders a tools/call result as the single string Nine's tool
// contract returns.
//
// Everything that is not text used to be dropped on the floor. That is worse
// than it sounds: a screenshot came back as a text summary saying a screenshot
// was taken, plus the image — so the model was handed a confident description
// of a picture it never received, and had no way to tell. Nothing is silently
// discarded now. Binary parts are written to the plugin's cache dir and named
// in the text, so the bytes stay reachable (read_file) without spending a
// context window on base64.
func FlattenContent(tool string, content []Content) string {
	var b strings.Builder
	for _, c := range content {
		switch c.Type {
		case "text":
			b.WriteString(c.Text)

		case "image", "audio":
			writeBinaryPart(&b, tool, c.Type, c.MimeType, c.Data)

		case "resource":
			switch {
			case c.Resource == nil:
				fmt.Fprintf(&b, "\n[%s: empty resource]", c.Type)
			case c.Resource.Text != "":
				b.WriteString(c.Resource.Text)
			case c.Resource.Blob != "":
				writeBinaryPart(&b, tool, "resource", c.Resource.MimeType, c.Resource.Blob)
			default:
				fmt.Fprintf(&b, "\n[resource: %s]", c.Resource.URI)
			}

		case "resource_link":
			fmt.Fprintf(&b, "\n[resource: %s %s]", c.Name, c.URI)

		default:
			// An unknown kind is still reported rather than dropped: a future MCP
			// content type should look like something missing, not like nothing.
			fmt.Fprintf(&b, "\n[unsupported MCP content type %q]", c.Type)
		}
	}
	return b.String()
}

// writeBinaryPart saves a base64 part beside the plugin's cache dir and notes
// where it went. With no cache dir configured it says what was received and
// that it was not kept — still better than silence.
func writeBinaryPart(b *strings.Builder, tool, kind, mime, data string) {
	if data == "" {
		fmt.Fprintf(b, "\n[%s: empty]", kind)
		return
	}
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		fmt.Fprintf(b, "\n[%s: undecodable base64 (%d chars)]", kind, len(data))
		return
	}

	dir := os.Getenv("NINE_PLUGIN_CACHE_DIR")
	if dir == "" {
		fmt.Fprintf(b, "\n[%s: %s, %d bytes, not saved — no plugin cache dir configured]", kind, mime, len(decoded))
		return
	}
	// The tool name is server-supplied and lands in a path, so it is sanitized
	// rather than trusted: a server advertising a tool called "../escaped" would
	// otherwise write its image outside the cache dir, anywhere the daemon can
	// write. Verified before fixing — the file really did land in the parent.
	name := fmt.Sprintf("%s-%d%s", SafeFileName(tool), time.Now().UnixNano(), ExtensionForMIME(mime))
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, decoded, 0o600); err != nil {
		fmt.Fprintf(b, "\n[%s: %s, %d bytes, could not be saved: %v]", kind, mime, len(decoded), err)
		return
	}
	fmt.Fprintf(b, "\n[%s: %s, %d bytes, saved to %s]", kind, mime, len(decoded), path)
}

// SafeFileName reduces a server-supplied name to something that cannot steer a
// path: separators, traversal, and anything outside a conservative set are
// replaced. Empty or fully-stripped input still yields a usable stem.
func SafeFileName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "part"
	}
	return out
}

// ExtensionForMIME picks a file extension so a saved part is recognisable.
func ExtensionForMIME(mime string) string {
	// Strip any parameters ("image/png; charset=..."), then take the subtype.
	mime, _, _ = strings.Cut(mime, ";")
	_, sub, ok := strings.Cut(strings.TrimSpace(mime), "/")
	if !ok || sub == "" {
		return ".bin"
	}
	if i := strings.LastIndex(sub, "+"); i >= 0 { // image/svg+xml → xml
		sub = sub[i+1:]
	}
	for _, r := range sub {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return ".bin"
		}
	}
	return "." + sub
}
