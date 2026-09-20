package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"nine/internal/toolvm"
)

// approvalDiffTools are the file tools whose approval prompt carries a diff
// instead of a path. A path tells a human which file is about to change; it
// does not tell them what the change is, which is the only question the gate
// actually asks them.
var approvalDiffTools = map[string]bool{
	"write_file": true,
	"edit_file":  true,
}

// maxApprovalDiffLines bounds what a prompt shows. An approval prompt a person
// has to scroll is one they stop reading, and a gate nobody reads is worse than
// no gate — it trains the reflex that defeats it.
const maxApprovalDiffLines = 40

// approvalDiff renders the change a gated file tool would make, by running that
// tool's own preview.
//
// The preview is the tool's, deliberately: a second implementation in Go would
// be a second answer to "what will this do", and the one the human approves
// must be the one the tool then performs. Nothing is written — preview is the
// tool refusing to write — so this costs a sandboxed call and no side effect.
//
// Returns "" when there is nothing useful to show, in which case the caller
// falls back to naming the path. A preview that fails must never block the
// approval: the human can still decide with less information, and a gate that
// errors is a tool that cannot run at all.
func approvalDiff(ctx context.Context, host *toolvm.Host, toolName string, args json.RawMessage) string {
	if host == nil || !approvalDiffTools[toolName] {
		return ""
	}
	previewArgs, err := withPreview(args)
	if err != nil {
		return ""
	}
	out, err := host.Call(ctx, toolName, previewArgs)
	if err != nil {
		return ""
	}
	var p struct {
		Diff      string `json:"diff"`
		Added     int    `json:"added_lines"`
		Removed   int    `json:"removed_lines"`
		Exists    bool   `json:"exists"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		return ""
	}
	if p.Diff == "" {
		if p.Added == 0 && p.Removed == 0 && p.Exists {
			return "No change: the file already holds this content."
		}
		if p.Added > 0 || p.Removed > 0 {
			// Too large to render, but the human still needs the scale of it:
			// "+2000 −1900" is a different decision from "+2 −1".
			return fmt.Sprintf("+%d −%d line(s), diff shortened", p.Added, p.Removed)
		}
		return ""
	}

	lines := strings.Split(p.Diff, "\n")
	clipped := false
	if len(lines) > maxApprovalDiffLines {
		lines = lines[:maxApprovalDiffLines]
		clipped = true
	}
	summary := fmt.Sprintf("+%d −%d line(s)", p.Added, p.Removed)
	if clipped || p.Truncated {
		summary += ", diff shortened"
	}
	return summary + "\n" + strings.Join(lines, "\n")
}

// withPreview copies args with preview set, so the gate never mutates the
// arguments the tool will actually be called with.
func withPreview(args json.RawMessage) (json.RawMessage, error) {
	obj := map[string]json.RawMessage{}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &obj); err != nil {
			return nil, err
		}
	}
	obj["preview"] = json.RawMessage(`true`)
	return json.Marshal(obj)
}
