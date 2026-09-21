package runtime

import (
	"os"
	"path/filepath"
	"testing"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

func migrationStore(t *testing.T) *memory.Store {
	t.Helper()
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// Files an agent saved with the retired file_store keep their paths, so
// anything that recorded where it put a file — a memory_set, a skill, a note in
// a report — still finds it there.
func TestMigrateStoredFilesKeepsPaths(t *testing.T) {
	store := migrationStore(t)
	root := t.TempDir()
	if err := store.FileStore("runbooks/db-restore.md", "pg_restore from the nightly snapshot\n"); err != nil {
		t.Fatal(err)
	}
	if err := store.FileStore("notes/pricing.md", "4200 per seat\n"); err != nil {
		t.Fatal(err)
	}

	MigrateStoredFilesToWorkspace(store, root)

	for path, want := range map[string]string{
		"runbooks/db-restore.md": "pg_restore from the nightly snapshot\n",
		"notes/pricing.md":       "4200 per seat\n",
	} {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Errorf("%s was not written to the workspace: %v", path, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}

	// The rows are gone, so nothing is left stranded in a table no tool reads.
	left, err := store.FileList("")
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("store still holds %v", left)
	}
}

// Spilled output is not agent-authored and the store is its home: the retention
// sweep still owns it, and moving it would put untrusted tool output into the
// workspace where a tool could rewrite it.
func TestMigrateStoredFilesLeavesSpills(t *testing.T) {
	store := migrationStore(t)
	root := t.TempDir()
	if err := store.FileStore("spill/agent-1/tool-abcd.txt", "truncated output\n"); err != nil {
		t.Fatal(err)
	}

	MigrateStoredFilesToWorkspace(store, root)

	if _, found, _ := store.FileFetch("spill/agent-1/tool-abcd.txt"); !found {
		t.Error("the spill was migrated out of the store")
	}
	if _, err := os.Stat(filepath.Join(root, "spill")); !os.IsNotExist(err) {
		t.Error("a spill was written into the workspace")
	}
}

// A workspace path already in use is a different file with the same name.
// Overwriting it would destroy the operator's file to rescue the agent's.
func TestMigrateStoredFilesDoesNotClobber(t *testing.T) {
	store := migrationStore(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.md"), []byte("the operator's file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.FileStore("notes.md", "the agent's file\n"); err != nil {
		t.Fatal(err)
	}

	MigrateStoredFilesToWorkspace(store, root)

	if got, _ := os.ReadFile(filepath.Join(root, "notes.md")); string(got) != "the operator's file\n" {
		t.Errorf("notes.md = %q, want the existing file untouched", got)
	}
	displaced := filepath.Join(root, ".nine", "migrated-store", "notes.md")
	if got, err := os.ReadFile(displaced); err != nil || string(got) != "the agent's file\n" {
		t.Errorf("displaced copy = %q, %v", got, err)
	}
}

// Running twice must not lose anything: a second boot has nothing to move, and
// a row is dropped only after its file exists.
func TestMigrateStoredFilesIsIdempotent(t *testing.T) {
	store := migrationStore(t)
	root := t.TempDir()
	if err := store.FileStore("a.md", "content\n"); err != nil {
		t.Fatal(err)
	}

	MigrateStoredFilesToWorkspace(store, root)
	MigrateStoredFilesToWorkspace(store, root)

	if got, _ := os.ReadFile(filepath.Join(root, "a.md")); string(got) != "content\n" {
		t.Errorf("a.md = %q after two runs", got)
	}
	if _, err := os.Stat(filepath.Join(root, ".nine", "migrated-store", "a.md")); !os.IsNotExist(err) {
		t.Error("the second run displaced the file it had already migrated")
	}
}

// With no workspace configured there is nowhere to migrate to, and dropping the
// rows would destroy the files outright.
func TestMigrateStoredFilesWithoutWorkspaceKeepsRows(t *testing.T) {
	store := migrationStore(t)
	if err := store.FileStore("a.md", "content\n"); err != nil {
		t.Fatal(err)
	}

	MigrateStoredFilesToWorkspace(store, "")

	if _, found, _ := store.FileFetch("a.md"); !found {
		t.Error("rows were dropped with no workspace to write them to")
	}
}
