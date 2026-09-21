package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"

	"nine/internal/toolvm"
)

// storeToolBytes is where a tool's binary result goes.
//
// A model cannot read bytes, so returning them to it is never the answer; what a
// tool needs is somewhere to *put* an artifact — a rendered image, a compressed
// archive — and a way to refer to it afterwards. The file store already is that
// place, with the retrieval machinery (`read_file`, `*_ref`) built and taught,
// so this reuses it rather than inventing a second destination.
//
// The bytes are stored base64-encoded, which is not a shortcut. The store is a
// TEXT column that replaces NUL bytes with U+FFFD (memory.Store.FileStore, and
// TestFileStoreStripsNULBytes explains why: SQLite's length() and substr() treat
// a NUL as end-of-value, so a stored NUL silently truncates every windowed read
// past it). Raw bytes cannot survive that. Base64 survives it exactly, and the
// `.b64` suffix means nothing downstream has to guess what it is holding.
func (d *Dispatcher) storeToolBytes(ctx context.Context, toolName string, out toolvm.Output) (string, error) {
	if d.spill == nil {
		// No sink: the bytes have nowhere to live, and inventing one here would
		// mean writing outside the store the operator configured.
		return "", fmt.Errorf(
			"tool %q returned %d bytes but this daemon has no file store configured to put them in",
			toolName, len(out.Bytes))
	}

	// The plain tool name: the sink owns path naming (runtime.spillPath renders
	// `spill/<agent>/<tool>-<random>.txt`), and an earlier attempt to smuggle a
	// `.b64` extension through this argument only produced
	// `make_icon-png-b64-<random>.txt` — the dot sanitized away and `.txt`
	// appended regardless. The path is a handle; the banner below is what
	// describes the content, and it is the thing the model actually reads.
	encoded := base64.StdEncoding.EncodeToString(out.Bytes)
	path, err := d.spill(ctx, toolName, encoded)
	if err != nil {
		// Unlike an over-cap *text* result, there is no degraded form to fall
		// back to: truncated base64 is not a smaller answer, it is a corrupt one.
		return "", fmt.Errorf("tool %q returned %d bytes that could not be stored: %w",
			toolName, len(out.Bytes), err)
	}

	slog.Info("tool returned bytes", "tool", toolName,
		"bytes", len(out.Bytes), "media_type", out.MediaType, "path", path)

	return byteResultBanner(len(out.Bytes), out.MediaType, path), nil
}

// sanitizeMediaType constrains a tool-supplied media type to the shape of one.
//
// It is interpolated into the banner below, which speaks in Nine's voice and
// tells the model what to do next. A tool's ordinary output reaches the model
// verbatim and that is fine — it is plainly the tool talking. This is different:
// unconstrained, a tool could put newlines and its own instructions inside
// Nine's framing, which is a thing its output cannot otherwise do.
//
// RFC 6838 tokens are narrow, so anything outside them is dropped rather than
// escaped, and an empty or over-long result falls back to "binary data".
func sanitizeMediaType(s string) string {
	if len(s) > 64 {
		return ""
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '/' || r == '-' || r == '+' || r == '.' || r == '_':
			b.WriteRune(r)
		default:
			return "" // a media type does not contain anything else
		}
	}
	return b.String()
}

// byteResultBanner is what the model actually reads. It has to do two things at
// once: not pretend bytes are text, and leave the model able to act.
//
// The wording follows spillPreview's lesson — name tools to CALL, with arguments
// spelled out as arguments. Written as signatures, live models responded by
// writing code instead of issuing a tool call.
func byteResultBanner(n int, mediaType, path string) string {
	what := "binary data"
	if clean := sanitizeMediaType(mediaType); clean != "" {
		what = clean
	}
	return fmt.Sprintf(
		"[nine: this tool returned %d bytes of %s, which is not text and is not shown here.\n"+
			"The bytes are saved in the memory file store, base64-encoded, at this path:\n"+
			"    %s\n"+
			"Do NOT try to reconstruct or guess the content. To use it:\n"+
			"  * give the path above to another tool's *_ref argument to hand it the whole\n"+
			"    content without reading it yourself;\n"+
			"  * or call read_file with path set to the path above to read the base64 text\n"+
			"    in windows, if you genuinely need the encoding itself.]",
		n, what, path)
}
