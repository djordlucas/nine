package runtime

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"nine/internal/agent"
	"nine/internal/memory"
)

// workspaceBackend is the dispatcher's view of the index: search and list, with
// the scanner underneath so an answer reflects the directory as it is now.
type workspaceBackend struct {
	store   *memory.Store
	scanner *WorkspaceScanner
	root    string
}

// NewWorkspaceBackend wires the index into the tools. Returns nil when no
// workspace is configured, which leaves the tools reporting that rather than
// answering from an index of nothing.
func NewWorkspaceBackend(store *memory.Store, scanner *WorkspaceScanner, root string) agent.WorkspaceBackend {
	if store == nil || root == "" {
		return nil
	}
	return &workspaceBackend{store: store, scanner: scanner, root: root}
}

// snippetRadius is how much of a line's surroundings a hit shows.
const snippetRadius = 90

// maxDirectScanBytes bounds a direct scan of one named file. Large, but finite:
// a scan reads the file, and a tool call has a deadline.
const maxDirectScanBytes = 256 << 20

func (w *workspaceBackend) Search(ctx context.Context, query, pathPrefix string, limit int) ([]agent.WorkspaceHit, int, bool, error) {
	if limit <= 0 {
		limit = 10
	}
	prefix := normalizeWorkspacePrefix(pathPrefix)

	// Refresh what is about to be searched, so a file written seconds ago by
	// `shell`, a build, or a git pull is already there. Bounded by the subtree.
	w.scanner.ScanSubtree(ctx, prefix)

	_, scanned, err := w.store.WorkspaceScanTime()
	if err != nil {
		scanned = false
	}

	hits, err := w.store.WorkspaceSearch(query, prefix, limit)
	if err != nil {
		return nil, 0, scanned, err
	}
	out := make([]agent.WorkspaceHit, 0, len(hits))
	for _, h := range hits {
		out = append(out, agent.WorkspaceHit{
			Path:    h.Path,
			Snippet: w.snippet(h.Path, query),
			Source:  "workspace",
		})
	}

	skipped, err := w.store.WorkspaceSkipped(prefix)
	if err != nil {
		skipped = 0
	}

	// A named file that carries no postings is searched directly. The index
	// ceiling decides what is searchable without being asked, not what is
	// reachable: a 200 MB log dropped in for analysis is still searchable when
	// the model names it.
	if len(out) == 0 && prefix != "" {
		if direct, derr := w.directScan(prefix, query, limit); derr == nil && len(direct) > 0 {
			out = append(out, direct...)
		}
	}
	return out, skipped, scanned, nil
}

func (w *workspaceBackend) List(ctx context.Context, prefix, pattern, changedSince string, limit int) (string, error) {
	p := normalizeWorkspacePrefix(prefix)
	w.scanner.ScanSubtree(ctx, p)
	return w.store.WorkspaceListJSON(p, pattern, changedSince, limit)
}

// snippet reads the matching region out of the file rather than out of the
// index. The index is contentless — it holds no text to quote — and reading the
// file is also what keeps a snippet honest when the file changed after it was
// indexed.
func (w *workspaceBackend) snippet(rel, query string) string {
	f, err := os.Open(filepath.Join(w.root, filepath.FromSlash(rel))) //nolint:gosec // inside the operator's workspace
	if err != nil {
		return ""
	}
	defer f.Close() //nolint:errcheck

	terms := searchTerms(query)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		lower := strings.ToLower(line)
		for _, t := range terms {
			if idx := strings.Index(lower, t); idx >= 0 {
				return clipAround(line, idx, snippetRadius)
			}
		}
	}
	return ""
}

// directScan searches one file by reading it, for a file the index skipped.
// Literal matching, no stemming and no ranking — which the caller labels, so a
// mixed result set never pretends the two kinds are the same.
func (w *workspaceBackend) directScan(rel, query string, limit int) ([]agent.WorkspaceHit, error) {
	full := filepath.Join(w.root, filepath.FromSlash(rel))
	st, err := os.Stat(full)
	if err != nil || st.IsDir() || st.Size() > maxDirectScanBytes {
		return nil, err
	}
	f, err := os.Open(full) //nolint:gosec // inside the operator's workspace
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck

	terms := searchTerms(query)
	if len(terms) == 0 {
		return nil, nil
	}
	var out []agent.WorkspaceHit
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	lineNo := 0
	for sc.Scan() && len(out) < limit {
		lineNo++
		line := sc.Text()
		lower := strings.ToLower(line)
		for _, t := range terms {
			if idx := strings.Index(lower, t); idx >= 0 {
				out = append(out, agent.WorkspaceHit{
					Path:    fmt.Sprintf("%s:%d", rel, lineNo),
					Snippet: clipAround(line, idx, snippetRadius),
					Source:  "scan",
				})
				break
			}
		}
	}
	return out, sc.Err()
}

// searchTerms reduces a query to the words a literal scan can look for.
func searchTerms(query string) []string {
	var out []string
	for _, f := range strings.Fields(strings.ToLower(query)) {
		f = strings.Trim(f, `"'.,:;()[]{}`)
		if len(f) >= 2 {
			out = append(out, f)
		}
	}
	return out
}

// clipAround returns the neighbourhood of a match, with ellipses where it cut.
func clipAround(line string, at, radius int) string {
	r := []rune(line)
	// `at` is a byte offset; convert once so a multi-byte line is not cut mid-rune.
	at = len([]rune(line[:at]))
	from := at - radius
	if from < 0 {
		from = 0
	}
	to := at + radius
	if to > len(r) {
		to = len(r)
	}
	s := string(r[from:to])
	if from > 0 {
		s = "…" + s
	}
	if to < len(r) {
		s += "…"
	}
	return strings.TrimSpace(s)
}

// normalizeWorkspacePrefix turns whatever a model passed into a
// workspace-relative path: /work/notes, notes, and /data/workspace/notes all
// mean the same directory.
func normalizeWorkspacePrefix(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = strings.TrimPrefix(p, "/work/")
	if p == "/work" {
		return ""
	}
	return strings.TrimPrefix(filepath.ToSlash(filepath.Clean(p)), "./")
}
