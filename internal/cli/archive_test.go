package cli_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nine/internal/cli"
	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

// writeTarGz builds an archive from a name→content map, for the cases a real
// backup would never produce.
func writeTarGz(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range entries {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestArchiveRoundTrip is the whole feature: store and workspace captured
// together and restored together.
func TestArchiveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg, db := cfgAt(t, dir)
	work := filepath.Join(dir, "workspace")
	cfg.Workspace.Root = work

	if err := os.MkdirAll(filepath.Join(work, "notes"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "notes", "report.md"), []byte("original report"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The trash is archived too: it is always included.
	if err := os.MkdirAll(filepath.Join(work, ".nine", "trash"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, ".nine", "trash", "gone.txt"), []byte("deleted"), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := memtest.OpenAt(t, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("k", "original"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	c := &cli.CLI{Out: &out, Err: &out}
	arc := filepath.Join(dir, "full.tar.gz")
	if err := c.Backup(cfg, arc); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if !strings.Contains(out.String(), "workspace:") {
		t.Errorf("backup output does not mention the workspace:\n%s", out.String())
	}

	// Diverge both sides.
	store2, err := memtest.OpenAt(t, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := store2.Set("k", "changed"); err != nil {
		t.Fatal(err)
	}
	if err := store2.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "notes", "report.md"), []byte("changed report"), 0o600); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	if err := c.Restore(cfg, arc); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	restored, err := memory.OpenReadOnly(db)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := restored.Get("k")
	restored.Close() //nolint:errcheck
	if err != nil {
		t.Fatal(err)
	}
	if got != "original" {
		t.Errorf("store value = %q, want \"original\"", got)
	}

	b, err := os.ReadFile(filepath.Join(work, "notes", "report.md"))
	if err != nil {
		t.Fatalf("workspace file missing after restore: %v", err)
	}
	if string(b) != "original report" {
		t.Errorf("workspace file = %q, want \"original report\"", b)
	}
	if _, err := os.Stat(filepath.Join(work, ".nine", "trash", "gone.txt")); err != nil {
		t.Errorf("the trash was not restored: %v", err)
	}
}

// TestArchiveSkipsSymlinks: a link is a path, not content. Archiving one would
// recreate a pointer into the host filesystem on extract.
func TestArchiveSkipsSymlinks(t *testing.T) {
	dir := t.TempDir()
	cfg, db := cfgAt(t, dir)
	work := filepath.Join(dir, "workspace")
	cfg.Workspace.Root = work
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "real.txt"), []byte("here"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(work, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	store, err := memtest.OpenAt(t, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	c := &cli.CLI{Out: &out, Err: &out}
	arc := filepath.Join(dir, "s.tar.gz")
	if err := c.Backup(cfg, arc); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if !strings.Contains(out.String(), "skipped:") {
		t.Errorf("the skipped symlink was not reported:\n%s", out.String())
	}

	names := tarNames(t, arc)
	for _, n := range names {
		if strings.HasSuffix(n, "escape") {
			t.Errorf("the symlink was archived: %q", n)
		}
	}
}

// TestExtractRefusesTraversal is the untrusted-input case: an archive is a file
// someone hands you, and an entry naming ../ must not write outside.
func TestExtractRefusesTraversal(t *testing.T) {
	dir := t.TempDir()
	cfg, db := cfgAt(t, dir)
	cfg.Workspace.Root = filepath.Join(dir, "workspace")
	if err := os.MkdirAll(cfg.Workspace.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := memtest.OpenAt(t, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	arc := filepath.Join(dir, "evil.tar.gz")
	writeTarGz(t, arc, map[string]string{"../../pwned.txt": "owned"})

	var out bytes.Buffer
	c := &cli.CLI{Out: &out, Err: &out}
	err = c.Restore(cfg, arc)
	if err == nil {
		t.Fatal("Restore accepted an archive with a traversing entry")
	}
	if !strings.Contains(err.Error(), "escapes the destination") {
		t.Errorf("error = %v, want one naming the escape", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pwned.txt")); err == nil {
		t.Error("the traversing entry was written outside the destination")
	}
	// Refused before anything moved.
	if m, _ := filepath.Glob(db + ".replaced-*"); len(m) != 0 {
		t.Errorf("a refused restore displaced the database: %v", m)
	}
}

func TestRestoreRefusesArchiveWithoutDatabase(t *testing.T) {
	dir := t.TempDir()
	cfg, db := cfgAt(t, dir)
	cfg.Workspace.Root = filepath.Join(dir, "workspace")
	if err := os.MkdirAll(cfg.Workspace.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := memtest.OpenAt(t, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	arc := filepath.Join(dir, "nodb.tar.gz")
	writeTarGz(t, arc, map[string]string{"workspace/a.txt": "just files"})

	var out bytes.Buffer
	c := &cli.CLI{Out: &out, Err: &out}
	err = c.Restore(cfg, arc)
	if err == nil || !strings.Contains(err.Error(), "not a nine backup archive") {
		t.Errorf("error = %v, want a refusal naming the missing database", err)
	}
}

// tarNames lists the entry names in a gzipped tar.
func tarNames(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close() //nolint:errcheck
	var names []string
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
	}
	return names
}
