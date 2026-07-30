package plugin_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nine/internal/plugin"
)

// readEnvDump parses the KEY=VALUE lines the testplugin writes to NINE_TEST_ENV_DUMP
// into a map of the plugin's spawn environment.
func readEnvDump(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read env dump: %v", err)
	}
	env := make(map[string]string)
	for line := range strings.SplitSeq(string(data), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			env[k] = v
		}
	}
	return env
}

// An ephemeral cache dir is created under the root, handed over via env with
// PERSISTENT=0, and removed when the plugin stops.
func TestEphemeralCacheDir(t *testing.T) {
	bin := buildBinary(t, "./internal/plugin/testplugin")
	root := t.TempDir()
	dump := filepath.Join(t.TempDir(), "env.txt")

	m := plugin.NewManager("")
	m.SetCacheConfig(root, func(string) bool { return false })

	p, err := m.Start(bin, "NINE_TEST_ENV_DUMP="+dump)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	env := readEnvDump(t, dump)
	dir := env["NINE_PLUGIN_CACHE_DIR"]
	if dir == "" {
		t.Fatal("NINE_PLUGIN_CACHE_DIR not set")
	}
	if filepath.Dir(dir) != root {
		t.Errorf("cache dir %q not under root %q", dir, root)
	}
	if env["NINE_PLUGIN_CACHE_PERSISTENT"] != "0" {
		t.Errorf("NINE_PLUGIN_CACHE_PERSISTENT = %q, want 0", env["NINE_PLUGIN_CACHE_PERSISTENT"])
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("cache dir should exist while running: %v", err)
	}

	if err := m.Stop(p); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("ephemeral cache dir should be removed after Stop (stat err = %v)", err)
	}
}

// A persistent cache dir is <root>/<name>, reported PERSISTENT=1, and survives
// the plugin stopping.
func TestPersistentCacheDir(t *testing.T) {
	bin := buildBinary(t, "./internal/plugin/testplugin")
	root := t.TempDir()
	dump := filepath.Join(t.TempDir(), "env.txt")

	m := plugin.NewManager("")
	m.SetCacheConfig(root, func(string) bool { return true })

	p, err := m.Start(bin, "NINE_TEST_ENV_DUMP="+dump)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	env := readEnvDump(t, dump)
	dir := env["NINE_PLUGIN_CACHE_DIR"]
	if dir != filepath.Join(root, filepath.Base(bin)) {
		t.Errorf("persistent cache dir = %q, want %q", dir, filepath.Join(root, filepath.Base(bin)))
	}
	if env["NINE_PLUGIN_CACHE_PERSISTENT"] != "1" {
		t.Errorf("NINE_PLUGIN_CACHE_PERSISTENT = %q, want 1", env["NINE_PLUGIN_CACHE_PERSISTENT"])
	}

	if err := m.Stop(p); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("persistent cache dir must survive Stop: %v", err)
	}
}

// With no cache root configured, no cache env is handed over at all.
func TestNoCacheDirWhenUnconfigured(t *testing.T) {
	bin := buildBinary(t, "./internal/plugin/testplugin")
	dump := filepath.Join(t.TempDir(), "env.txt")

	m := plugin.NewManager("")
	p, err := m.Start(bin, "NINE_TEST_ENV_DUMP="+dump)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop(p) //nolint:errcheck

	if _, ok := readEnvDump(t, dump)["NINE_PLUGIN_CACHE_DIR"]; ok {
		t.Error("cache env should be absent when no cache root is configured")
	}
}

// The boot sweep reclaims leftover ephemeral dirs and leaves persistent ones.
func TestSweepCache(t *testing.T) {
	root := t.TempDir()
	ephemeral := []string{"browser.deadbeef", "files.12345678"}
	persistent := []string{"scanner", "browser"}
	for _, d := range append(append([]string{}, ephemeral...), persistent...) {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	m := plugin.NewManager("")
	m.SetCacheConfig(root, nil)
	m.SweepCache()

	for _, d := range ephemeral {
		if _, err := os.Stat(filepath.Join(root, d)); !os.IsNotExist(err) {
			t.Errorf("ephemeral leftover %q should be swept (stat err = %v)", d, err)
		}
	}
	for _, d := range persistent {
		if _, err := os.Stat(filepath.Join(root, d)); err != nil {
			t.Errorf("persistent dir %q should survive sweep: %v", d, err)
		}
	}
}

// SweepCache is a safe no-op when no cache root is set or the root is absent.
func TestSweepCacheNoRoot(t *testing.T) {
	plugin.NewManager("").SweepCache() // unconfigured: must not panic

	m := plugin.NewManager("")
	m.SetCacheConfig(filepath.Join(t.TempDir(), "does-not-exist"), nil)
	m.SweepCache() // missing root: must not panic
}

// Probe hands over a throwaway ephemeral cache dir and removes it with the
// process, whatever persist_cache would say.
func TestProbeUsesEphemeralCache(t *testing.T) {
	bin := buildBinary(t, "./internal/plugin/testplugin")
	dump := filepath.Join(t.TempDir(), "env.txt")

	if _, err := plugin.Probe(bin, "NINE_TEST_ENV_DUMP="+dump); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	env := readEnvDump(t, dump)
	dir := env["NINE_PLUGIN_CACHE_DIR"]
	if dir == "" {
		t.Fatal("Probe did not set NINE_PLUGIN_CACHE_DIR")
	}
	if env["NINE_PLUGIN_CACHE_PERSISTENT"] != "0" {
		t.Errorf("Probe PERSISTENT = %q, want 0", env["NINE_PLUGIN_CACHE_PERSISTENT"])
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("probe cache dir should be removed after Probe (stat err = %v)", err)
	}
}
