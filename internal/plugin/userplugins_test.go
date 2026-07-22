package plugin_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"nine/internal/plugin"
)

// seedManifest writes a `<name>.toml` sidecar in dir pointing at binPath.
func seedManifest(t *testing.T, dir, name, binPath string) {
	t.Helper()
	content := fmt.Sprintf("name = %q\nentrypoint = %q\n", name, binPath)
	if err := os.WriteFile(filepath.Join(dir, name+".toml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func statusByName(sts []plugin.UserPluginStatus, name string) (plugin.UserPluginStatus, bool) {
	for _, s := range sts {
		if s.Name == name {
			return s, true
		}
	}
	return plugin.UserPluginStatus{}, false
}

func TestProbeValidPlugin(t *testing.T) {
	bin := buildBinary(t, "./internal/plugin/testplugin")
	desc, err := plugin.Probe(bin)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if len(desc.Tools) != 1 || desc.Tools[0].Name != "echo" {
		t.Fatalf("unexpected tools: %+v", desc.Tools)
	}
}

func TestProbeRejectsNonPlugin(t *testing.T) {
	// A binary that runs but never listens on the socket is not a plugin.
	if _, err := plugin.Probe("/usr/bin/true"); err == nil {
		t.Fatal("expected Probe to reject a non-plugin binary")
	}
}

func TestLoadUserPlugins(t *testing.T) {
	bin := buildBinary(t, "./internal/plugin/testplugin")
	dir := t.TempDir()
	seedManifest(t, dir, "weather", bin)

	m := plugin.NewManager("")
	m.LoadUserPlugins(dir)

	st, ok := statusByName(m.UserStatus(), "weather")
	if !ok {
		t.Fatal("weather not in UserStatus")
	}
	if !st.Loaded {
		t.Fatalf("weather not loaded: %s", st.Err)
	}

	var found *plugin.Plugin
	for _, p := range m.Running() {
		if p.Name == filepath.Base(bin) {
			found = p
		}
	}
	if found == nil {
		t.Fatal("user plugin not in Running()")
	}
	if !found.User {
		t.Error("loaded plugin should be marked User")
	}
	_ = m.StopAll()
}

func TestUserPluginCollisionSkipped(t *testing.T) {
	bin := buildBinary(t, "./internal/plugin/testplugin")

	m := plugin.NewManager("")
	// Start one copy as an already-running (built-in-style) plugin owning "echo".
	if _, err := m.Start(bin); err != nil {
		t.Fatalf("Start: %v", err)
	}

	dir := t.TempDir()
	seedManifest(t, dir, "dupe", bin) // same binary → same tool "echo"
	m.LoadUserPlugins(dir)

	st, ok := statusByName(m.UserStatus(), "dupe")
	if !ok {
		t.Fatal("dupe not in UserStatus")
	}
	if st.Loaded {
		t.Fatal("colliding plugin should not be loaded")
	}
	if st.Err == "" {
		t.Fatal("colliding plugin should carry a reason")
	}
	_ = m.StopAll()
}

func TestReloadUserPluginsRemoves(t *testing.T) {
	bin := buildBinary(t, "./internal/plugin/testplugin")
	dir := t.TempDir()
	seedManifest(t, dir, "weather", bin)

	m := plugin.NewManager("")
	m.LoadUserPlugins(dir)
	if len(m.Running()) != 1 {
		t.Fatalf("expected 1 running, got %d", len(m.Running()))
	}

	// Remove the manifest and reload: the plugin should be stopped and gone.
	if err := os.Remove(filepath.Join(dir, "weather.toml")); err != nil {
		t.Fatal(err)
	}
	m.ReloadUserPlugins()
	if len(m.Running()) != 0 {
		t.Fatalf("expected 0 running after reload, got %d", len(m.Running()))
	}
	if st, ok := statusByName(m.UserStatus(), "weather"); ok && st.Loaded {
		t.Fatal("weather should no longer be loaded after reload")
	}
	_ = m.StopAll()
}
