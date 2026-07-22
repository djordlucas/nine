package plugin

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestLoadManifest(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name    string
		content string
		wantErr bool
	}{
		{"valid", "name = \"weather\"\nentrypoint = \"./weather\"\n", false},
		{"missing name", "entrypoint = \"./weather\"\n", true},
		{"missing entrypoint", "name = \"weather\"\n", true},
		{"unknown key", "name = \"weather\"\nentrypoint = \"./weather\"\nfoo = 1\n", true},
		{"bad toml", "name = \n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, "m.toml")
			writeFile(t, path, tc.content)
			_, err := LoadManifest(path)
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestDiscoverPlugins(t *testing.T) {
	dir := t.TempDir()

	// A well-formed plugin: manifest + a present (non-executed) binary.
	writeFile(t, filepath.Join(dir, "weather.toml"), "name = \"weather\"\nentrypoint = \"./weather\"\n")
	writeFile(t, filepath.Join(dir, "weather"), "#!/bin/sh\n")

	// A manifest whose binary is absent.
	writeFile(t, filepath.Join(dir, "ghost.toml"), "name = \"ghost\"\nentrypoint = \"./ghost\"\n")

	// A malformed manifest.
	writeFile(t, filepath.Join(dir, "broken.toml"), "name = \n")

	// A non-manifest file that must be ignored.
	writeFile(t, filepath.Join(dir, "notes.txt"), "ignore me")

	got := discoverPlugins(dir)
	if len(got) != 3 {
		t.Fatalf("expected 3 discovered, got %d", len(got))
	}

	// Sorted by name: broken, ghost, weather.
	byName := map[string]discovered{}
	for _, d := range got {
		byName[d.Name] = d
	}
	if got[0].Name != "broken" || got[1].Name != "ghost" || got[2].Name != "weather" {
		t.Fatalf("not sorted by name: %v", []string{got[0].Name, got[1].Name, got[2].Name})
	}
	if byName["broken"].Err == nil {
		t.Error("broken manifest should carry an error")
	}
	if byName["ghost"].Err == nil {
		t.Error("missing binary should carry an error")
	}
	if byName["weather"].Err != nil {
		t.Errorf("valid plugin should not carry an error: %v", byName["weather"].Err)
	}
	if byName["weather"].BinaryPath != filepath.Join(dir, "weather") {
		t.Errorf("entrypoint not resolved against dir: %q", byName["weather"].BinaryPath)
	}
}

func TestDiscoverPluginsAbsentDir(t *testing.T) {
	if got := discoverPlugins(""); got != nil {
		t.Errorf("empty dir should yield nil, got %v", got)
	}
	if got := discoverPlugins(filepath.Join(t.TempDir(), "does-not-exist")); got != nil {
		t.Errorf("absent dir should yield nil, got %v", got)
	}
}
