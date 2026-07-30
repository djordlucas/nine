package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nine/internal/config"
)

// loadTOML writes body to a temp nine.toml and loads it, returning the load
// error so a test can assert on validation failures.
func loadTOML(t *testing.T, body string) (*config.Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nine.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return config.Load(path)
}

// envMap resolves a KEY=VALUE slice the way os/exec does: a duplicate key takes
// its last occurrence, so it models the effective environment the plugin sees.
func envMap(kvs []string) map[string]string {
	m := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}

// Operator settings layer on top of built-in defaults, and win on a duplicate
// key — the property that makes `BROWSER_HEADLESS = "0"` override the default.
func TestPluginEnvsSettingsOverrideDefaults(t *testing.T) {
	cfg, err := loadTOML(t, `
[plugin.browser.settings]
BROWSER_HEADLESS = "0"
EXTRA_FLAG       = "on"
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	env := cfg.PluginEnvs("browser")
	got := envMap(env)
	if got["BROWSER_HEADLESS"] != "0" {
		t.Errorf("BROWSER_HEADLESS = %q, want 0 (operator override)", got["BROWSER_HEADLESS"])
	}
	if got["BROWSER_TIMEOUT"] != "30000" {
		t.Errorf("BROWSER_TIMEOUT = %q, want built-in default 30000", got["BROWSER_TIMEOUT"])
	}
	if got["EXTRA_FLAG"] != "on" {
		t.Errorf("EXTRA_FLAG = %q, want on", got["EXTRA_FLAG"])
	}

	// The override must sit *after* the default in the slice, since precedence is
	// realized by append order + exec's last-wins dedup, not by an explicit merge.
	defIdx, setIdx := -1, -1
	for i, kv := range env {
		switch kv {
		case "BROWSER_HEADLESS=1":
			defIdx = i
		case "BROWSER_HEADLESS=0":
			setIdx = i
		}
	}
	if defIdx == -1 || setIdx == -1 || setIdx < defIdx {
		t.Errorf("override ordering wrong: default at %d, setting at %d (want setting after default)", defIdx, setIdx)
	}
}

// A user plugin Nine ships no defaults for still receives its operator settings —
// the whole point of schema-less pass-through.
func TestPluginEnvsUserPluginSettings(t *testing.T) {
	cfg, err := loadTOML(t, `
[plugin.weather.settings]
WEATHER_API_KEY = "sk-123"
UNITS           = "metric"
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := envMap(cfg.PluginEnvs("weather"))
	if got["WEATHER_API_KEY"] != "sk-123" {
		t.Errorf("WEATHER_API_KEY = %q, want sk-123", got["WEATHER_API_KEY"])
	}
	if got["UNITS"] != "metric" {
		t.Errorf("UNITS = %q, want metric", got["UNITS"])
	}
}

// A plugin with neither defaults nor settings gets an empty environment.
func TestPluginEnvsEmpty(t *testing.T) {
	cfg, err := loadTOML(t, "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if env := cfg.PluginEnvs("shell"); len(env) != 0 {
		t.Errorf("PluginEnvs(shell) = %v, want empty", env)
	}
}

// TOML scalars are stringified: strings verbatim, integers/floats via strconv,
// booleans as true/false.
func TestPluginEnvsStringifiesScalars(t *testing.T) {
	cfg, err := loadTOML(t, `
[plugin.demo.settings]
STR   = "hello"
INT   = 5000
FLOAT = 1.5
BOOL  = true
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := envMap(cfg.PluginEnvs("demo"))
	for k, want := range map[string]string{
		"STR": "hello", "INT": "5000", "FLOAT": "1.5", "BOOL": "true",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
}

// A NINE_-prefixed default that is not one of the three reserved spawn vars stays
// overridable through settings.
func TestPluginEnvsNineWorkspaceOverridable(t *testing.T) {
	cfg, err := loadTOML(t, `
[plugin.files.settings]
NINE_WORKSPACE = "/custom/ws"
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := envMap(cfg.PluginEnvs("files")); got["NINE_WORKSPACE"] != "/custom/ws" {
		t.Errorf("NINE_WORKSPACE = %q, want /custom/ws", got["NINE_WORKSPACE"])
	}
}

func TestPluginSettingsValidation(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"valid scalars", `[plugin.p.settings]` + "\nA = \"x\"\nB = 1\n", false},
		{"invalid key name", `[plugin.p.settings]` + "\n\"bad-key\" = \"x\"\n", true},
		{"key starting with digit", `[plugin.p.settings]` + "\n\"1KEY\" = \"x\"\n", true},
		{"reserved socket key", `[plugin.p.settings]` + "\nNINE_PLUGIN_SOCKET = \"/tmp/x\"\n", true},
		{"reserved cache key", `[plugin.p.settings]` + "\nNINE_PLUGIN_CACHE_DIR = \"/tmp/c\"\n", true},
		{"array value", `[plugin.p.settings]` + "\nARR = [1, 2]\n", true},
		{"inline table value", `[plugin.p]` + "\n" + `settings = { NESTED = { a = 1 } }` + "\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadTOML(t, tc.body)
			if tc.wantErr && err == nil {
				t.Errorf("Load(%q): want error, got nil", tc.body)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Load(%q): unexpected error: %v", tc.body, err)
			}
		})
	}
}
