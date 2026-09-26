package cli_test

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nine/internal/cli"
	"nine/internal/config"
	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

// cfgAt points the CLI at a database path in dir, with a socket path nothing
// listens on so CanConnect reports the daemon down.
func cfgAt(t *testing.T, dir string) (*config.Config, string) {
	t.Helper()
	db := filepath.Join(dir, "nine.db")
	cfg := &config.Config{}
	cfg.Memory.Path = db
	cfg.Daemon.SocketPath = filepath.Join(dir, "absent.sock")
	return cfg, db
}

// TestRestoreRoundTrip is the whole point: what backup wrote, restore brings
// back.
func TestRestoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg, db := cfgAt(t, dir)

	store, err := memtest.OpenAt(t, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("k", "original"); err != nil {
		t.Fatal(err)
	}
	snap := filepath.Join(dir, "snap.db")
	if _, err := store.BackupTo(snap); err != nil {
		t.Fatal(err)
	}
	// Diverge from the snapshot, then close so the file is not held open.
	if err := store.Set("k", "changed"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	c := &cli.CLI{Out: &out, Err: &out}
	if err := c.Restore(cfg, snap); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	restored, err := memory.OpenReadOnly(db)
	if err != nil {
		t.Fatalf("open restored db: %v", err)
	}
	defer restored.Close() //nolint:errcheck
	got, found, err := restored.Get("k")
	if err != nil {
		t.Fatal(err)
	}
	if !found || got != "original" {
		t.Errorf("restored value = %q found=%v, want \"original\" true", got, found)
	}
}

// TestRestoreDisplacesSidecars pins the failure the command exists to prevent:
// a restored database left paired with the previous one's write-ahead log.
func TestRestoreDisplacesSidecars(t *testing.T) {
	dir := t.TempDir()
	cfg, db := cfgAt(t, dir)

	store, err := memtest.OpenAt(t, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("k", "v"); err != nil {
		t.Fatal(err)
	}
	snap := filepath.Join(dir, "snap.db")
	if _, err := store.BackupTo(snap); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// Whether Close leaves sidecars is SQLite's business; plant them so the
	// test asserts on displacement regardless.
	for _, sc := range []string{db + "-wal", db + "-shm"} {
		if err := os.WriteFile(sc, []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var out bytes.Buffer
	c := &cli.CLI{Out: &out, Err: &out}
	if err := c.Restore(cfg, snap); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	for _, sc := range []string{db + "-wal", db + "-shm"} {
		if _, err := os.Stat(sc); err == nil {
			t.Errorf("stale sidecar survived the restore: %s", sc)
		}
	}
	// Nothing destroyed: the displaced originals are still on disk.
	matches, err := filepath.Glob(db + ".replaced-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Error("the displaced database was not kept")
	}
	if !strings.Contains(out.String(), "undo with:") {
		t.Errorf("output does not say how to undo:\n%s", out.String())
	}
}

func TestRestoreRefusesNonDatabase(t *testing.T) {
	dir := t.TempDir()
	cfg, db := cfgAt(t, dir)
	if err := os.WriteFile(db, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	junk := filepath.Join(dir, "notadb.db")
	if err := os.WriteFile(junk, []byte("certainly not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	c := &cli.CLI{Out: &out, Err: &out}
	if err := c.Restore(cfg, junk); err == nil {
		t.Fatal("Restore accepted a file that is not a database")
	}
	// The live database is untouched, because the check runs before the move.
	b, err := os.ReadFile(db)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "live" {
		t.Errorf("live database was disturbed by a refused restore: %q", b)
	}
}

func TestRestoreRefusesTheLiveDatabase(t *testing.T) {
	dir := t.TempDir()
	cfg, db := cfgAt(t, dir)
	store, err := memtest.OpenAt(t, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	c := &cli.CLI{Out: &out, Err: &out}
	err = c.Restore(cfg, db)
	if err == nil || !strings.Contains(err.Error(), "live database") {
		t.Errorf("Restore(live db) error = %v, want a refusal naming the live database", err)
	}
}

// TestRestoreRefusesWhileDaemonRunning is the guard that matters most: the
// daemon holds the database open, so swapping the file underneath it corrupts
// the restore and the sessions in flight. Sockets go in /tmp rather than
// t.TempDir() because macOS caps Unix-socket paths at 104 bytes.
func TestRestoreRefusesWhileDaemonRunning(t *testing.T) {
	dir := t.TempDir()
	cfg, db := cfgAt(t, dir)

	sock := filepath.Join("/tmp", fmt.Sprintf("nine-restore-test-%d.sock", os.Getpid()))
	os.Remove(sock) //nolint:errcheck
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()       //nolint:errcheck
	defer os.Remove(sock) //nolint:errcheck
	cfg.Daemon.SocketPath = sock

	store, err := memtest.OpenAt(t, db)
	if err != nil {
		t.Fatal(err)
	}
	snap := filepath.Join(dir, "snap.db")
	if _, err := store.BackupTo(snap); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	c := &cli.CLI{Out: &out, Err: &out}
	err = c.Restore(cfg, snap)
	if err == nil {
		t.Fatal("Restore ran while the daemon was reachable")
	}
	if !strings.Contains(err.Error(), "daemon is running") {
		t.Errorf("error = %v, want one naming the running daemon", err)
	}
	// Refused before anything moved.
	if m, _ := filepath.Glob(db + ".replaced-*"); len(m) != 0 {
		t.Errorf("a refused restore displaced the database: %v", m)
	}
}
