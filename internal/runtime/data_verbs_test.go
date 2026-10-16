package runtime_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nine/internal/memory/memtest"
	"nine/internal/protocol"
	"nine/internal/runtime"
	"nine/internal/sqldb"
)

// dataHarness is a daemon with a workspace and a database, as the web UI's
// Data section reaches them.
func dataHarness(t *testing.T) (root, sock string) {
	t.Helper()
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sqldb.Open(filepath.Join(t.TempDir(), "database.sqlite"), sqldb.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	root = t.TempDir()
	if err := runtime.PrepareWorkspaceState(root); err != nil {
		t.Fatal(err)
	}
	d, sock := startDaemon(t, makeFactory(seqProvider(nil)), nil, nil)
	d.ConfigureMemory(store)
	d.ConfigureWorkspace(root)
	d.ConfigureDatabase(db)
	return root, sock
}

// The operator lists, reads, writes and deletes workspace files; a replaced or
// deleted file goes to the trash, a stale version is refused, and Nine's own
// state is neither listed nor reachable.
func TestWorkspaceOverTheWire(t *testing.T) {
	root, sock := dataHarness(t)
	c := dial(t, sock)

	var e protocol.WorkspaceEntry
	if err := c.Workspace(protocol.WorkspaceWrite, protocol.WorkspaceArgs{Path: "notes/today.md", Content: []byte("one")}, &e); err != nil {
		t.Fatalf("write: %v", err)
	}
	if e.Path != "notes/today.md" || e.Bytes != 3 || e.Version == "" || e.Trashed != "" {
		t.Fatalf("written = %+v", e)
	}

	var ls protocol.WorkspaceListing
	if err := c.Workspace(protocol.WorkspaceList, protocol.WorkspaceArgs{}, &ls); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ls.Entries) != 1 || ls.Entries[0].Name != "notes" || !ls.Entries[0].Dir {
		t.Fatalf("root = %+v, want notes/ only (no .nine)", ls.Entries)
	}

	var f protocol.WorkspaceFile
	if err := c.Workspace(protocol.WorkspaceRead, protocol.WorkspaceArgs{Path: "/notes/today.md"}, &f); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(f.Content) != "one" || f.Version != e.Version {
		t.Fatalf("read = %+v", f)
	}

	var e2 protocol.WorkspaceEntry
	if err := c.Workspace(protocol.WorkspaceWrite, protocol.WorkspaceArgs{Path: "notes/today.md", Content: []byte("two!"), IfVersion: f.Version}, &e2); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if e2.Trashed == "" {
		t.Fatal("an overwrite names no trash entry")
	}
	if old, err := os.ReadFile(filepath.Join(root, runtime.WorkspaceTrashDir, e2.Trashed, "notes/today.md")); err != nil || string(old) != "one" {
		t.Errorf("trashed copy = %q, %v", old, err)
	}
	err := c.Workspace(protocol.WorkspaceWrite, protocol.WorkspaceArgs{Path: "notes/today.md", Content: []byte("x"), IfVersion: f.Version}, &e)
	if err == nil || !strings.Contains(err.Error(), "changed since it was read") {
		t.Errorf("stale write: err = %v", err)
	}

	for _, p := range []string{".nine/trash", "../outside", "notes/../../outside"} {
		if err := c.Workspace(protocol.WorkspaceList, protocol.WorkspaceArgs{Path: p}, &ls); err == nil {
			t.Errorf("list %s: reached", p)
		}
	}
	if err := c.Workspace(protocol.WorkspaceWrite, protocol.WorkspaceArgs{Path: "spill/x", Content: []byte("x")}, &e); err == nil {
		t.Error("write spill/x: allowed")
	}
	// A link out of the workspace is neither listed nor followed.
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := c.Workspace(protocol.WorkspaceWrite, protocol.WorkspaceArgs{Path: "escape/x", Content: []byte("x")}, &e); err == nil {
		t.Error("write through a link out of the workspace: allowed")
	}

	if err := c.Workspace(protocol.WorkspaceDelete, protocol.WorkspaceArgs{Path: "notes"}, &e); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Errorf("delete a full directory: err = %v", err)
	}
	if err := c.Workspace(protocol.WorkspaceDelete, protocol.WorkspaceArgs{Path: "notes/today.md"}, &e); err != nil || e.Trashed == "" {
		t.Fatalf("delete: %+v, %v", e, err)
	}
	if err := c.Workspace(protocol.WorkspaceDelete, protocol.WorkspaceArgs{Path: "notes"}, &e); err != nil {
		t.Fatalf("delete the empty directory: %v", err)
	}
	if err := c.Workspace(protocol.WorkspaceRead, protocol.WorkspaceArgs{Path: "notes/today.md"}, &f); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("read a deleted file: err = %v", err)
	}
}

// The operator lists tables, edits a row and runs a statement; the store is
// not among the tables.
func TestDatabaseOverTheWire(t *testing.T) {
	_, sock := dataHarness(t)
	c := dial(t, sock)

	var run sqldb.RunResult
	if err := c.Database(protocol.DatabaseRun, protocol.DatabaseArgs{SQL: "CREATE TABLE seen (url TEXT, n INTEGER)"}, &run); err != nil || !run.Write {
		t.Fatalf("create: %+v, %v", run, err)
	}
	if err := c.Database(protocol.DatabaseInsert, protocol.DatabaseArgs{Table: "seen", Values: map[string]any{"url": "a", "n": 7}}, &run); err != nil || run.Changes != 1 {
		t.Fatalf("insert: %+v, %v", run, err)
	}
	var tables struct{ Tables []sqldb.Table }
	if err := c.Database(protocol.DatabaseTables, protocol.DatabaseArgs{}, &tables); err != nil {
		t.Fatal(err)
	}
	if len(tables.Tables) != 1 || tables.Tables[0].Name != "seen" || tables.Tables[0].Rows != 1 {
		t.Fatalf("tables = %+v, want seen only", tables.Tables)
	}
	if err := c.Database(protocol.DatabaseUpdate, protocol.DatabaseArgs{Table: "seen", RowID: 1, Values: map[string]any{"n": 8}}, &run); err != nil {
		t.Fatal(err)
	}
	var page sqldb.Page
	if err := c.Database(protocol.DatabaseRows, protocol.DatabaseArgs{Table: "seen"}, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != 1 || page.Rows[0][1] != float64(8) || page.RowIDs[0] != 1 {
		t.Fatalf("page = %+v", page)
	}
	// A whole number is stored as an integer, not 8.0.
	if err := c.Database(protocol.DatabaseRun, protocol.DatabaseArgs{SQL: "SELECT typeof(n) FROM seen"}, &run); err != nil || run.Rows[0][0] != "integer" {
		t.Fatalf("typeof = %+v, %v", run.Rows, err)
	}
	if err := c.Database(protocol.DatabaseRun, protocol.DatabaseArgs{SQL: "ATTACH DATABASE ':memory:' AS o"}, &run); err == nil {
		t.Error("ATTACH: allowed")
	}
}
