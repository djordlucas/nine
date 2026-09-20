package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedTrashEntry writes one trash entry as the file tools write them:
// .nine/trash/<UTC>-<random>/<original relative path>.
func seedTrashEntry(t *testing.T, root string, at time.Time, rel, body string) string {
	t.Helper()
	// Two entries seeded in the same second need distinct names; the suffix is
	// random in production, and unique-per-call here.
	var name string
	for i := 0; ; i++ {
		name = at.UTC().Format("20060102T150405Z") + "-" + string(rune('a'+i)) + "0000000"
		if _, err := os.Stat(filepath.Join(root, WorkspaceTrashDir, name)); os.IsNotExist(err) {
			break
		}
	}
	dir := filepath.Join(root, WorkspaceTrashDir, name)
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}

func trashEntryNames(t *testing.T, root string) []string {
	t.Helper()
	dirents, err := os.ReadDir(filepath.Join(root, WorkspaceTrashDir))
	if err != nil {
		t.Fatalf("read trash: %v", err)
	}
	var out []string
	for _, d := range dirents {
		out = append(out, d.Name())
	}
	return out
}

// The workspace is frequently a git repository the operator mounted. A trash
// directory turning up in `git status` after every edit is noise they cannot
// switch off, and something they might commit by accident.
func TestPrepareWorkspaceStateIgnoresItself(t *testing.T) {
	root := t.TempDir()
	if err := PrepareWorkspaceState(root); err != nil {
		t.Fatalf("PrepareWorkspaceState: %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, WorkspaceTrashDir)); err != nil {
		t.Errorf("trash directory was not created: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(root, WorkspaceStateDir, ".gitignore"))
	if err != nil {
		t.Fatalf("gitignore: %v", err)
	}
	if strings.TrimSpace(string(body)) != "*" {
		t.Errorf("gitignore = %q, want it to ignore everything including itself", body)
	}
}

// An operator's own edit to the ignore file survives a restart.
func TestPrepareWorkspaceStateIsIdempotent(t *testing.T) {
	root := t.TempDir()
	if err := PrepareWorkspaceState(root); err != nil {
		t.Fatal(err)
	}
	custom := "*\n!keep-me\n"
	if err := os.WriteFile(filepath.Join(root, WorkspaceStateDir, ".gitignore"), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := PrepareWorkspaceState(root); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(root, WorkspaceStateDir, ".gitignore"))
	if string(body) != custom {
		t.Errorf("gitignore = %q, want the operator's version untouched", body)
	}
}

// Age is read from the entry's name, not the file's mtime: a rename preserves
// mtime, so a trashed file still reports when it was last *written*, which can
// be months before anyone deleted it.
func TestSweepTrashRemovesExpiredEntries(t *testing.T) {
	root := t.TempDir()
	if err := PrepareWorkspaceState(root); err != nil {
		t.Fatal(err)
	}
	old := seedTrashEntry(t, root, time.Now().Add(-10*24*time.Hour), "notes/old.md", "stale")
	fresh := seedTrashEntry(t, root, time.Now().Add(-1*time.Hour), "notes/new.md", "recent")

	// The old entry's file is given a recent mtime, so a sweep keyed on mtime
	// would keep exactly the wrong one.
	now := time.Now()
	if err := os.Chtimes(filepath.Join(root, WorkspaceTrashDir, old, "notes", "old.md"), now, now); err != nil {
		t.Fatal(err)
	}

	sweepTrash(root, 7*24*time.Hour, 1<<30)

	names := trashEntryNames(t, root)
	if len(names) != 1 || names[0] != fresh {
		t.Errorf("trash holds %v, want only the fresh entry %s", names, fresh)
	}
}

// Age alone is not enough: a week of large deletions can outgrow a volume long
// before anything expires, and the trash sits on the operator's own disk.
func TestSweepTrashEnforcesSizeBoundOldestFirst(t *testing.T) {
	root := t.TempDir()
	if err := PrepareWorkspaceState(root); err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("x", 4096)
	oldest := seedTrashEntry(t, root, time.Now().Add(-3*time.Hour), "a.txt", body)
	middle := seedTrashEntry(t, root, time.Now().Add(-2*time.Hour), "b.txt", body)
	newest := seedTrashEntry(t, root, time.Now().Add(-1*time.Hour), "c.txt", body)

	// Room for one entry only.
	sweepTrash(root, 7*24*time.Hour, 5000)

	names := trashEntryNames(t, root)
	if len(names) != 1 || names[0] != newest {
		t.Errorf("trash holds %v, want only the newest entry %s (oldest goes first)", names, newest)
	}
	for _, gone := range []string{oldest, middle} {
		if _, err := os.Stat(filepath.Join(root, WorkspaceTrashDir, gone)); !os.IsNotExist(err) {
			t.Errorf("%s survived the size sweep", gone)
		}
	}
}

// A negative retention disables the age bound. The size bound still applies, so
// "keep things longer" never means "keep everything for ever".
func TestSweepTrashRetentionDisabledKeepsAgeButNotSize(t *testing.T) {
	root := t.TempDir()
	if err := PrepareWorkspaceState(root); err != nil {
		t.Fatal(err)
	}
	ancient := seedTrashEntry(t, root, time.Now().Add(-400*24*time.Hour), "a.txt", "tiny")

	sweepTrash(root, 0, 1<<30)
	if names := trashEntryNames(t, root); len(names) != 1 || names[0] != ancient {
		t.Errorf("trash holds %v, want the ancient entry kept when retention is disabled", names)
	}

	sweepTrash(root, 0, 1)
	if names := trashEntryNames(t, root); len(names) != 0 {
		t.Errorf("trash holds %v, want the size bound to apply regardless", names)
	}
}

// A directory nobody here created is not a housekeeping decision to make
// quietly: the trash lives inside the operator's workspace.
func TestSweepTrashLeavesUnrecognizedDirectories(t *testing.T) {
	root := t.TempDir()
	if err := PrepareWorkspaceState(root); err != nil {
		t.Fatal(err)
	}
	stranger := filepath.Join(root, WorkspaceTrashDir, "not-an-entry")
	if err := os.MkdirAll(stranger, 0o755); err != nil {
		t.Fatal(err)
	}
	seedTrashEntry(t, root, time.Now().Add(-30*24*time.Hour), "a.txt", "stale")

	sweepTrash(root, 7*24*time.Hour, 1<<30)

	if _, err := os.Stat(stranger); err != nil {
		t.Errorf("an unrecognized directory was swept: %v", err)
	}
}

// A missing trash is an empty trash, not an error to log on every tick.
func TestSweepTrashWithNoTrashDirectory(t *testing.T) {
	sweepTrash(t.TempDir(), 7*24*time.Hour, 1<<30)
}
