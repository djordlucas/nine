package runtime

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"nine/internal/memory"
)

// The workspace scan is what makes a file Nine never wrote findable. A bind
// mount arrives full, a `git pull` adds a thousand files, an operator drops a
// PDF in for analysis — none of that goes through a tool, so an index fed by
// Nine's own writes would never see any of it. The filesystem is the record;
// the index is a cache over it.

const (
	// DefaultScanInterval is how often the workspace is rescanned.
	DefaultScanInterval = time.Minute
	// DefaultIndexMaxFileBytes is the largest file whose text is indexed.
	//
	// The bound is about churn, not storage: FTS5 indexes a row as one document
	// with no way to append, so a file that grows retracts and retokenizes its
	// whole contents every time the scan sees it change. A 40 MiB log gaining a
	// line a minute costs 40 MiB of tokenization a minute, for ever. 8 MiB
	// covers source, documentation and configuration — the files where searching
	// the whole workspace pays — while text above it is almost always data that
	// both churns and ranks poorly.
	DefaultIndexMaxFileBytes int64 = 8 << 20
	// DefaultIndexMaxFiles bounds one scan. Beyond it the scan stops and says so
	// rather than presenting a partial index as a complete one.
	DefaultIndexMaxFiles = 50_000
)

// Reasons a known file carries no postings. They are reported to the agent, so
// "no matches" is never mistaken for "not there".
const (
	SkipTooLarge = "larger than the index limit"
	SkipBinary   = "not valid UTF-8"
	SkipUnread   = "could not be read"
)

// WorkspaceScanner keeps the index in step with the directory.
type WorkspaceScanner struct {
	store        *memory.Store
	root         string
	maxFileBytes int64
	maxFiles     int
	interval     time.Duration
	ignore       *gitIgnore
}

// NewWorkspaceScanner builds a scanner, or nil when there is nothing to scan.
func NewWorkspaceScanner(store *memory.Store, root string, maxFileBytes int64, maxFiles int, interval time.Duration) *WorkspaceScanner {
	if store == nil || root == "" {
		return nil
	}
	if maxFileBytes <= 0 {
		maxFileBytes = DefaultIndexMaxFileBytes
	}
	if maxFiles <= 0 {
		maxFiles = DefaultIndexMaxFiles
	}
	if interval <= 0 {
		interval = DefaultScanInterval
	}
	return &WorkspaceScanner{
		store: store, root: root,
		maxFileBytes: maxFileBytes, maxFiles: maxFiles, interval: interval,
	}
}

// Run scans at startup and then on every tick until ctx is cancelled. The first
// scan happens in the background: a large workspace must not hold up the daemon,
// and a search issued before it finishes says so rather than reporting an empty
// index as an empty workspace.
func (w *WorkspaceScanner) Run(ctx context.Context) {
	if w == nil {
		return
	}
	w.ScanAll(ctx)
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.ScanAll(ctx)
		}
	}
}

// ScanAll walks the whole workspace and records what changed.
func (w *WorkspaceScanner) ScanAll(ctx context.Context) {
	if w == nil {
		return
	}
	n, truncated, err := w.scan(ctx, "")
	if err != nil {
		slog.Warn("workspace scan failed", "root", w.root, "err", err)
		return
	}
	if err := w.store.SetWorkspaceScanTime(time.Now()); err != nil {
		slog.Warn("workspace scan: cannot record completion", "err", err)
	}
	if truncated {
		slog.Warn("workspace scan stopped at the file limit",
			"root", w.root, "limit", w.maxFiles, "indexed", n)
	}
}

// ScanSubtree refreshes one subtree before a search or a listing answers, so a
// file written seconds ago — by `shell`, by a build, by a `git pull` — is
// already there. Bounded work: the subtree, not the workspace.
//
// prefix is a workspace-relative path; an empty one rescans everything.
func (w *WorkspaceScanner) ScanSubtree(ctx context.Context, prefix string) {
	if w == nil {
		return
	}
	if _, _, err := w.scan(ctx, prefix); err != nil {
		slog.Debug("workspace subtree scan failed", "prefix", prefix, "err", err)
	}
}

// scan walks `prefix` under the root, upserting changed files and dropping rows
// whose files are gone. Returns how many files it saw and whether it stopped at
// the file limit.
func (w *WorkspaceScanner) scan(ctx context.Context, prefix string) (int, bool, error) {
	w.ignore = loadGitIgnore(w.root)

	start := filepath.Join(w.root, filepath.FromSlash(prefix))
	info, err := os.Stat(start)
	if err != nil {
		if os.IsNotExist(err) {
			// The subtree is gone: drop what the index still claims is there.
			w.dropMissing(prefix, map[string]bool{})
			return 0, false, nil
		}
		return 0, false, err
	}

	seen := make(map[string]bool)
	count := 0
	truncated := false

	walkRoot := start
	if !info.IsDir() {
		walkRoot = filepath.Dir(start)
	}

	err = filepath.WalkDir(walkRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable directory is skipped, not fatal
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, rerr := filepath.Rel(w.root, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)

		if d.IsDir() {
			if skipDir(rel) || (w.ignore != nil && w.ignore.match(rel+"/")) {
				return filepath.SkipDir
			}
			return nil
		}
		if w.ignore != nil && w.ignore.match(rel) {
			return nil
		}
		if count >= w.maxFiles {
			truncated = true
			return filepath.SkipAll
		}
		count++
		seen[rel] = true
		w.indexFile(p, rel)
		return nil
	})
	if err != nil && ctx.Err() == nil {
		return count, truncated, err
	}

	// A scan that stopped early has not seen the rest of the tree, so it cannot
	// conclude anything about what is missing from it.
	if !truncated {
		w.dropMissing(prefix, seen)
	}
	return count, truncated, nil
}

// indexFile records one file, with its text when the file is indexable.
func (w *WorkspaceScanner) indexFile(full, rel string) {
	st, err := os.Stat(full)
	if err != nil {
		return
	}
	size := st.Size()
	mtime := st.ModTime().UnixMilli()

	// Unchanged since the last scan: (size, mtime) is the same pair a rewrite
	// would change, and re-reading every file every minute is what makes a scan
	// too expensive to run often.
	if oldSize, oldMTime, found, err := w.store.WorkspaceIndexState(rel); err == nil && found {
		if oldSize == size && oldMTime == mtime {
			return
		}
	}

	if size > w.maxFileBytes {
		w.upsert(rel, size, mtime, nil, SkipTooLarge)
		return
	}
	body, err := os.ReadFile(full) //nolint:gosec // a path inside the operator's own workspace
	if err != nil {
		w.upsert(rel, size, mtime, nil, SkipUnread)
		return
	}
	if !utf8.Valid(body) {
		w.upsert(rel, size, mtime, nil, SkipBinary)
		return
	}
	text := string(body)
	w.upsert(rel, size, mtime, &text, "")
}

func (w *WorkspaceScanner) upsert(rel string, size, mtime int64, content *string, reason string) {
	if err := w.store.WorkspaceIndexUpsert(rel, size, mtime, content, reason); err != nil {
		slog.Debug("workspace index upsert failed", "path", rel, "err", err)
	}
}

// dropMissing removes index rows under prefix whose files are no longer there.
func (w *WorkspaceScanner) dropMissing(prefix string, seen map[string]bool) {
	known, err := w.store.WorkspaceIndexPaths(prefix)
	if err != nil {
		return
	}
	for _, p := range known {
		if seen[p] {
			continue
		}
		if _, err := os.Stat(filepath.Join(w.root, filepath.FromSlash(p))); err == nil {
			continue // still there; this scan simply did not reach it
		}
		if err := w.store.WorkspaceIndexDelete(p); err != nil {
			slog.Debug("workspace index delete failed", "path", p, "err", err)
		}
	}
}

// skipDir reports directories the scan never enters: the operator's version
// control, and Nine's own bookkeeping.
func skipDir(rel string) bool {
	if rel == "." {
		return false
	}
	base := filepath.Base(rel)
	return base == ".git" || rel == WorkspaceStateDir || strings.HasPrefix(rel, WorkspaceStateDir+"/")
}

// gitIgnore is a deliberately small matcher over the root .gitignore: literal
// names, directory entries, and `*` globs. Nested ignore files and negation are
// not implemented — the effect of missing a rule is an extra indexed file, not
// a missing one, which is the safe direction for a search index.
type gitIgnore struct{ patterns []string }

func loadGitIgnore(root string) *gitIgnore {
	body, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		return nil
	}
	var pats []string
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		pats = append(pats, strings.TrimPrefix(line, "/"))
	}
	sort.Strings(pats)
	return &gitIgnore{patterns: pats}
}

func (g *gitIgnore) match(rel string) bool {
	if g == nil {
		return false
	}
	base := filepath.Base(strings.TrimSuffix(rel, "/"))
	for _, p := range g.patterns {
		dirOnly := strings.HasSuffix(p, "/")
		pat := strings.TrimSuffix(p, "/")
		if dirOnly && !strings.HasSuffix(rel, "/") {
			continue
		}
		if ok, _ := filepath.Match(pat, base); ok {
			return true
		}
		if ok, _ := filepath.Match(pat, strings.TrimSuffix(rel, "/")); ok {
			return true
		}
		// A bare directory name ignores everything beneath it.
		if !strings.ContainsAny(pat, "*?[") && strings.HasPrefix(rel, pat+"/") {
			return true
		}
	}
	return false
}
