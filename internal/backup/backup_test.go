package backup

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRemoveTempOnlyRemovesABackupsDirectory(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	dir, err := NewTempDir()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, DBEntry)
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	other := filepath.Join(t.TempDir(), "keep.db")
	if err := os.WriteFile(other, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveTemp(other); err == nil {
		t.Error("removed a file that is not a backup's")
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("the other file is gone: %v", err)
	}

	if err := RemoveTemp(file); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the backup's directory is still there: %v", err)
	}
}

func TestNewTempDirSweepsAbandonedOnes(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	old, err := os.MkdirTemp("", TempPrefix)
	if err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-2 * TempMaxAge)
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatal(err)
	}
	fresh, err := NewTempDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("an abandoned backup directory was not swept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("the new directory: %v", err)
	}
}
