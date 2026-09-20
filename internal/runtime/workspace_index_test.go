package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nine/internal/agent"
	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

func indexTestStore(t *testing.T) *memory.Store {
	t.Helper()
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func writeWS(t *testing.T, root, rel, body string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newTestScanner(t *testing.T, store *memory.Store, root string) *WorkspaceScanner {
	t.Helper()
	return NewWorkspaceScanner(store, root, DefaultIndexMaxFileBytes, DefaultIndexMaxFiles, time.Minute)
}

// The case the index exists for: a file Nine never wrote — here, one that was
// simply placed in the directory — is findable.
func TestScannerIndexesFilesNineDidNotWrite(t *testing.T) {
	store := indexTestStore(t)
	root := t.TempDir()
	writeWS(t, root, "docs/vendor.md", "The incident response SLA is four hours for severity one.\n")
	writeWS(t, root, "docs/menu.md", "Cafeteria menu for the week.\n")

	sc := newTestScanner(t, store, root)
	sc.ScanAll(context.Background())

	hits, err := store.WorkspaceSearch("incident response", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Path != "docs/vendor.md" {
		t.Fatalf("hits = %+v, want docs/vendor.md", hits)
	}
}

// Deletions propagate: an index that still lists a file someone removed sends
// the agent to read a path that is not there.
func TestScannerDropsVanishedFiles(t *testing.T) {
	store := indexTestStore(t)
	root := t.TempDir()
	writeWS(t, root, "notes.md", "pangolin census figures\n")

	sc := newTestScanner(t, store, root)
	sc.ScanAll(context.Background())
	if hits, _ := store.WorkspaceSearch("pangolin", "", 10); len(hits) != 1 {
		t.Fatalf("file was not indexed to begin with")
	}

	if err := os.Remove(filepath.Join(root, "notes.md")); err != nil {
		t.Fatal(err)
	}
	sc.ScanAll(context.Background())

	if hits, _ := store.WorkspaceSearch("pangolin", "", 10); len(hits) != 0 {
		t.Errorf("hits = %+v, want none after the file was deleted", hits)
	}
	files, _ := store.WorkspaceList("", "", "", 100)
	if len(files) != 0 {
		t.Errorf("index still lists %+v", files)
	}
}

// Rewriting a file replaces its postings rather than adding to them. A
// contentless FTS table cannot retract postings from text it never kept, so
// this is the property contentless_delete=1 buys.
func TestScannerReindexesChangedContent(t *testing.T) {
	store := indexTestStore(t)
	root := t.TempDir()
	writeWS(t, root, "a.md", "aardvark\n")

	sc := newTestScanner(t, store, root)
	sc.ScanAll(context.Background())

	// A changed size is what the scan notices; mtime alone can have one-second
	// resolution on some filesystems.
	writeWS(t, root, "a.md", "beluga beluga\n")
	sc.ScanAll(context.Background())

	if hits, _ := store.WorkspaceSearch("aardvark", "", 10); len(hits) != 0 {
		t.Errorf("the old content is still searchable: %+v", hits)
	}
	if hits, _ := store.WorkspaceSearch("beluga", "", 10); len(hits) != 1 {
		t.Errorf("the new content is not searchable")
	}
}

// Skipped files still list, with the reason. A file missing from a listing
// reads as a file that is not there.
func TestScannerRecordsSkippedFilesWithReasons(t *testing.T) {
	store := indexTestStore(t)
	root := t.TempDir()
	writeWS(t, root, "small.txt", "findable text\n")
	writeWS(t, root, "big.log", strings.Repeat("x", 4096))
	if err := os.WriteFile(filepath.Join(root, "image.png"), []byte{0x89, 'P', 'N', 'G', 0x00, 0xff, 0xfe}, 0o600); err != nil {
		t.Fatal(err)
	}

	// A ceiling small enough that big.log is over it.
	sc := NewWorkspaceScanner(store, root, 1024, DefaultIndexMaxFiles, time.Minute)
	sc.ScanAll(context.Background())

	files, err := store.WorkspaceList("", "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]memory.WorkspaceFile{}
	for _, f := range files {
		byPath[f.Path] = f
	}
	if len(byPath) != 3 {
		t.Fatalf("index holds %d files, want all three listed: %+v", len(byPath), files)
	}
	if !byPath["small.txt"].Indexed {
		t.Error("small.txt is not indexed")
	}
	if byPath["big.log"].Indexed || byPath["big.log"].Reason != SkipTooLarge {
		t.Errorf("big.log = %+v, want it skipped as too large", byPath["big.log"])
	}
	if byPath["image.png"].Indexed || byPath["image.png"].Reason != SkipBinary {
		t.Errorf("image.png = %+v, want it skipped as binary", byPath["image.png"])
	}
}

// .git is the operator's version control and .nine is Nine's own bookkeeping:
// indexing either wastes the budget and surfaces noise as search hits.
func TestScannerSkipsGitAndStateDirectories(t *testing.T) {
	store := indexTestStore(t)
	root := t.TempDir()
	writeWS(t, root, ".git/COMMIT_EDITMSG", "secret commit subject\n")
	writeWS(t, root, ".nine/trash/20260920T101010Z-aaaa/old.md", "secret trashed text\n")
	writeWS(t, root, "kept.md", "ordinary text\n")

	sc := newTestScanner(t, store, root)
	sc.ScanAll(context.Background())

	files, _ := store.WorkspaceList("", "", "", 100)
	if len(files) != 1 || files[0].Path != "kept.md" {
		t.Errorf("indexed %+v, want only kept.md", files)
	}
	if hits, _ := store.WorkspaceSearch("secret", "", 10); len(hits) != 0 {
		t.Errorf("hits = %+v, want nothing from .git or .nine", hits)
	}
}

// A repository's ignored build output is not worth indexing, and indexing it
// pushes real files past the scan bound.
func TestScannerHonorsGitignore(t *testing.T) {
	store := indexTestStore(t)
	root := t.TempDir()
	writeWS(t, root, ".gitignore", "node_modules/\n*.log\n")
	writeWS(t, root, "node_modules/pkg/index.js", "ignored dependency code\n")
	writeWS(t, root, "debug.log", "ignored log line\n")
	writeWS(t, root, "src/app.js", "real source code\n")

	sc := newTestScanner(t, store, root)
	sc.ScanAll(context.Background())

	paths := map[string]bool{}
	files, _ := store.WorkspaceList("", "", "", 100)
	for _, f := range files {
		paths[f.Path] = true
	}
	if !paths["src/app.js"] {
		t.Error("real source was not indexed")
	}
	if paths["node_modules/pkg/index.js"] || paths["debug.log"] {
		t.Errorf("ignored files were indexed: %+v", files)
	}
}

// A scan that stopped at its bound has not seen the rest of the tree, so it
// must not conclude the files it never reached are gone.
func TestScannerAtFileLimitKeepsUnseenRows(t *testing.T) {
	store := indexTestStore(t)
	root := t.TempDir()
	for i := 0; i < 8; i++ {
		writeWS(t, root, filepath.Join("many", string(rune('a'+i))+".txt"), "content\n")
	}

	full := newTestScanner(t, store, root)
	full.ScanAll(context.Background())
	before, _ := store.WorkspaceList("", "", "", 100)
	if len(before) != 8 {
		t.Fatalf("indexed %d files, want 8", len(before))
	}

	limited := NewWorkspaceScanner(store, root, DefaultIndexMaxFileBytes, 3, time.Minute)
	limited.ScanAll(context.Background())

	after, _ := store.WorkspaceList("", "", "", 100)
	if len(after) != 8 {
		t.Errorf("index holds %d files after a truncated scan, want the earlier rows kept", len(after))
	}
}

// changed_since is how an agent asks what appeared while it was away.
func TestWorkspaceListChangedSince(t *testing.T) {
	store := indexTestStore(t)
	root := t.TempDir()
	writeWS(t, root, "old.md", "first\n")

	sc := newTestScanner(t, store, root)
	sc.ScanAll(context.Background())

	cutoff := time.Now().UTC().Format("2006-01-02T15:04:05.000000Z07:00")
	time.Sleep(5 * time.Millisecond)

	writeWS(t, root, "new.md", "second\n")
	sc.ScanAll(context.Background())

	files, err := store.WorkspaceList("", "", cutoff, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != "new.md" {
		t.Errorf("changed_since returned %+v, want only new.md", files)
	}
}

// Finding a file by name is a different question from finding one by content,
// and a role without shell cannot answer it any other way.
func TestWorkspaceListGlob(t *testing.T) {
	store := indexTestStore(t)
	root := t.TempDir()
	writeWS(t, root, "src/api/handler.go", "package api\n")
	writeWS(t, root, "src/api/handler_test.go", "package api\n")
	writeWS(t, root, "src/worker/queue_test.go", "package worker\n")
	writeWS(t, root, "README.md", "docs\n")

	sc := newTestScanner(t, store, root)
	sc.ScanAll(context.Background())

	files, err := store.WorkspaceList("", "*_test.go", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("glob returned %+v, want the two test files", files)
	}
	for _, f := range files {
		if !strings.HasSuffix(f.Path, "_test.go") {
			t.Errorf("glob matched %s", f.Path)
		}
	}
}

// Search results come back with a snippet read from the file, and the backend
// reports what it could not look at.
func TestWorkspaceBackendSearchSnippetsAndSkips(t *testing.T) {
	store := indexTestStore(t)
	root := t.TempDir()
	writeWS(t, root, "runbook.md", "To restore prod: pg_restore from the nightly snapshot in s3://backups/pg.\n")
	writeWS(t, root, "huge.log", strings.Repeat("noise\n", 500))

	sc := NewWorkspaceScanner(store, root, 1024, DefaultIndexMaxFiles, time.Minute)
	sc.ScanAll(context.Background())

	be := NewWorkspaceBackend(store, sc, root)
	hits, skipped, scanned, err := be.Search(context.Background(), "nightly snapshot", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !scanned {
		t.Error("scanned = false after a completed scan")
	}
	if len(hits) != 1 || hits[0].Path != "runbook.md" {
		t.Fatalf("hits = %+v", hits)
	}
	if !strings.Contains(hits[0].Snippet, "s3://backups/pg") {
		t.Errorf("snippet = %q, want the matching region of the file", hits[0].Snippet)
	}
	if hits[0].Source != "workspace" {
		t.Errorf("source = %q, want the hit labelled", hits[0].Source)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want the oversized log counted", skipped)
	}
}

// The ceiling decides what is searchable without being asked, not what is
// reachable: naming an unindexed file searches it directly.
func TestWorkspaceBackendDirectScanOfUnindexedFile(t *testing.T) {
	store := indexTestStore(t)
	root := t.TempDir()
	body := strings.Repeat("filler\n", 200) + "checksum mismatch record=88213\n"
	writeWS(t, root, "logs/import.log", body)

	sc := NewWorkspaceScanner(store, root, 64, DefaultIndexMaxFiles, time.Minute)
	sc.ScanAll(context.Background())

	be := NewWorkspaceBackend(store, sc, root)
	hits, _, _, err := be.Search(context.Background(), "checksum", "logs/import.log", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("a named file over the index ceiling was not searched")
	}
	if !strings.Contains(hits[0].Snippet, "88213") {
		t.Errorf("snippet = %q", hits[0].Snippet)
	}
	// Labelled as a direct scan: it is literal and unranked, unlike an index hit.
	if hits[0].Source != "scan" {
		t.Errorf("source = %q, want \"scan\"", hits[0].Source)
	}
}

// A search refreshes the subtree it is about to look at, so a file written a
// moment ago by something other than Nine is already there.
func TestWorkspaceBackendRefreshesBeforeSearching(t *testing.T) {
	store := indexTestStore(t)
	root := t.TempDir()
	sc := newTestScanner(t, store, root)
	sc.ScanAll(context.Background())

	// Written after the last scan, as `shell` or a git pull would.
	writeWS(t, root, "fresh/notes.md", "quokka sighting confirmed\n")

	be := NewWorkspaceBackend(store, sc, root)
	hits, _, _, err := be.Search(context.Background(), "quokka", "fresh", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %+v, want the file written since the last scan", hits)
	}
}

// list_files answers through the same backend, as JSON the tool returns.
func TestWorkspaceBackendList(t *testing.T) {
	store := indexTestStore(t)
	root := t.TempDir()
	writeWS(t, root, "a/one.md", "one\n")
	writeWS(t, root, "b/two.md", "two\n")

	sc := newTestScanner(t, store, root)
	sc.ScanAll(context.Background())

	be := NewWorkspaceBackend(store, sc, root)
	out, err := be.List(context.Background(), "a", "", "", 50)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Files []memory.WorkspaceFile `json:"files"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", out, err)
	}
	if len(got.Files) != 1 || got.Files[0].Path != "a/one.md" {
		t.Errorf("files = %+v, want only the one under a/", got.Files)
	}
}

// A model passes whatever path shape it has: the /work alias, a bare relative
// path, or a trailing slash. They name the same directory.
func TestNormalizeWorkspacePrefix(t *testing.T) {
	cases := map[string]string{
		"":             "",
		"/work":        "",
		"/work/notes":  "notes",
		"notes":        "notes",
		"./notes":      "notes",
		"notes/":       "notes",
		"/work/a/b.md": "a/b.md",
	}
	for in, want := range cases {
		if got := normalizeWorkspacePrefix(in); got != want {
			t.Errorf("normalizeWorkspacePrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

var _ agent.WorkspaceBackend = (*workspaceBackend)(nil)
