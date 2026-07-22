package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nine/internal/config"
)

func TestPluginValidateNoPathNoConfig(t *testing.T) {
	c := &CLI{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}
	err := c.PluginValidate(&config.Config{}, "")
	if err == nil {
		t.Fatal("expected an error when no path is given and user_dir is unset")
	}
	if !strings.Contains(err.Error(), "user_dir") {
		t.Errorf("error should mention user_dir, got: %v", err)
	}
}

func TestPluginValidateDirReportsBadManifests(t *testing.T) {
	dir := t.TempDir()
	// A malformed manifest and a manifest whose binary is missing — both should
	// be reported as failures without executing anything.
	writeTOML(t, dir, "broken.toml", "name = \n")
	writeTOML(t, dir, "ghost.toml", "name = \"ghost\"\nentrypoint = \"./ghost\"\n")

	var out bytes.Buffer
	c := &CLI{Out: &out, Err: &bytes.Buffer{}}
	err := c.PluginValidate(&config.Config{}, dir)
	if err == nil {
		t.Fatal("expected validation to fail on bad manifests")
	}
	s := out.String()
	if !strings.Contains(s, "2 manifest(s) checked, 2 invalid") {
		t.Errorf("summary missing/wrong:\n%s", s)
	}
	if !strings.Contains(s, "still starts without them") {
		t.Errorf("expected fail-soft note:\n%s", s)
	}
}

func TestPluginValidateEmptyDir(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	c := &CLI{Out: &out, Err: &bytes.Buffer{}}
	if err := c.PluginValidate(&config.Config{}, dir); err != nil {
		t.Fatalf("empty dir should not error: %v", err)
	}
	if !strings.Contains(out.String(), "No plugin manifests found") {
		t.Errorf("expected 'no manifests' message, got:\n%s", out.String())
	}
}

func writeTOML(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
