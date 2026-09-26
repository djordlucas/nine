package memory_test

import (
	"os"
	"path/filepath"
	"testing"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

// TestBackupToSnapshotIsReadable is the property the command exists for: the
// snapshot is a complete, openable database, not a fragment needing sidecars.
func TestBackupToSnapshotIsReadable(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("k", "v"); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "snap.db")
	abs, err := store.BackupTo(dst)
	if err != nil {
		t.Fatalf("BackupTo: %v", err)
	}
	if abs != dst {
		t.Errorf("returned path = %q, want %q", abs, dst)
	}

	// No sidecars: VACUUM INTO writes one self-contained file.
	for _, sidecar := range []string{dst + "-wal", dst + "-shm"} {
		if _, err := os.Stat(sidecar); err == nil {
			t.Errorf("snapshot left a sidecar behind: %s", sidecar)
		}
	}

	restored, err := memory.OpenReadOnly(dst)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer restored.Close() //nolint:errcheck
	got, found, err := restored.Get("k")
	if err != nil {
		t.Fatal(err)
	}
	if !found || got != "v" {
		t.Errorf("snapshot value = %q found=%v, want \"v\" true", got, found)
	}
}

// TestBackupToRefusesExistingDestination guards the mistake that turns a backup
// routine into no backup at all: silently replacing the previous snapshot.
func TestBackupToRefusesExistingDestination(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "snap.db")
	if err := os.WriteFile(dst, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BackupTo(dst); err == nil {
		t.Fatal("BackupTo overwrote an existing file")
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "existing" {
		t.Errorf("existing file was modified: %q", b)
	}
}

func TestBackupToRejectsBadPaths(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"empty":       "",
		"missing dir": filepath.Join(t.TempDir(), "nope", "snap.db"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := store.BackupTo(path); err == nil {
				t.Errorf("BackupTo(%q) succeeded; want an error", path)
			}
		})
	}
}
