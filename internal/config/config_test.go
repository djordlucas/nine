package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nine/internal/config"
)

// R-CFG.1: config is resolved by trying, in order, $NINE_CONFIG → ./nine.toml →
// /nine.toml → ~/.nine/nine.toml; the first that exists wins.
func TestLoadDefaultResolutionOrder(t *testing.T) {
	// Neutralize LLM env overrides so the provider reflects the file only.
	t.Setenv("NINE_LLM_PROVIDER", "")

	// Work from an empty dir so there is no ./nine.toml to interfere.
	work := t.TempDir()
	t.Chdir(work)

	envFile := filepath.Join(work, "env.toml")
	if err := os.WriteFile(envFile, []byte("[llm]\nprovider = \"from-nine-config\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".nine"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".nine", "nine.toml"), []byte("[llm]\nprovider = \"from-home\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	// $NINE_CONFIG has highest precedence.
	t.Setenv("NINE_CONFIG", envFile)
	if cfg := config.LoadDefault(); cfg.LLM.Provider != "from-nine-config" {
		t.Errorf("with NINE_CONFIG set, provider = %q, want from-nine-config", cfg.LLM.Provider)
	}

	// Unset $NINE_CONFIG (and no ./nine.toml, no /nine.toml): falls through to
	// $HOME/.nine/nine.toml.
	t.Setenv("NINE_CONFIG", "")
	if cfg := config.LoadDefault(); cfg.LLM.Provider != "from-home" {
		t.Errorf("without NINE_CONFIG, provider = %q, want from-home (~/.nine fallback)", cfg.LLM.Provider)
	}
}

const sampleTOML = `
[llm]
provider       = "ollama"
model          = "qwen3.5:4b"
endpoint       = "https://ollama.example.com"
context_budget = 4096
max_concurrent = 2

[plugins]
bin = "/data/bin"
disabled = ["shell", "browser"]
user_dir = "/data/plugins.d"

[memory]
path = "/data/memory.db"

[embeddings]
provider = "ollama"
model    = "nomic-embed-text"
endpoint = "http://host.docker.internal:11434"
api_key  = ""
`

func TestLoad(t *testing.T) {
	f, err := os.CreateTemp("", "nine-*.toml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(sampleTOML); err != nil {
		t.Fatal(err)
	}
	f.Close()

	cfg, err := config.Load(f.Name())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"LLM.Provider", cfg.LLM.Provider, "ollama"},
		{"LLM.Model", cfg.LLM.Model, "qwen3.5:4b"},
		{"LLM.Endpoint", cfg.LLM.Endpoint, "https://ollama.example.com"},
		{"LLM.ContextBudget", cfg.LLM.ContextBudget, 4096},
		{"LLM.MaxConcurrent", cfg.LLM.MaxConcurrent, 2},
		{"Plugins.Bin", cfg.Plugins.Bin, "/data/bin"},
		{"Plugins.Disabled", strings.Join(cfg.Plugins.Disabled, ","), "shell,browser"},
		{"Plugins.UserDir", cfg.Plugins.UserDir, "/data/plugins.d"},
		{"Memory.Path", cfg.Memory.Path, "/data/memory.db"},
		{"Embeddings.Provider", cfg.Embeddings.Provider, "ollama"},
		{"Embeddings.Model", cfg.Embeddings.Model, "nomic-embed-text"},
		{"Embeddings.Endpoint", cfg.Embeddings.Endpoint, "http://host.docker.internal:11434"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := config.Load("/nonexistent/path/nine.toml")
	if err == nil {
		t.Error("expected error for missing file, got nil")
	}
}

func TestEventRetention(t *testing.T) {
	cases := []struct {
		name      string
		turns     int
		days      int
		wantTurns int
		wantAge   time.Duration
	}{
		{"unset uses default", 0, 0, config.DefaultEventRetentionTurns, 0},
		{"explicit turns", 50, 0, 50, 0},
		{"negative keeps all", -1, 0, 0, 0},
		{"age in days", 0, 7, config.DefaultEventRetentionTurns, 7 * 24 * time.Hour},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Daemon.EventRetentionTurns = c.turns
			cfg.Daemon.EventRetentionDays = c.days
			gotTurns, gotAge := cfg.EventRetention()
			if gotTurns != c.wantTurns || gotAge != c.wantAge {
				t.Errorf("EventRetention() = (%d, %v), want (%d, %v)", gotTurns, gotAge, c.wantTurns, c.wantAge)
			}
		})
	}
}

func TestThinkingEnabled(t *testing.T) {
	// Unset (nil) defaults to on.
	var l config.LLMConfig
	if !l.ThinkingEnabled() {
		t.Error("unset thinking should default to enabled")
	}
	// Explicit false disables; explicit true enables.
	f, tr := false, true
	l.Thinking = &f
	if l.ThinkingEnabled() {
		t.Error("explicit false should disable")
	}
	l.Thinking = &tr
	if !l.ThinkingEnabled() {
		t.Error("explicit true should enable")
	}
}

func TestRelatedSessionsIndexEnabled(t *testing.T) {
	// Unset (nil) defaults to on.
	var d config.DaemonConfig
	if !d.RelatedSessionsIndexEnabled() {
		t.Error("unset related_sessions_index should default to enabled")
	}
	// Explicit false disables; explicit true enables.
	f, tr := false, true
	d.RelatedSessionsIndex = &f
	if d.RelatedSessionsIndexEnabled() {
		t.Error("explicit false should disable")
	}
	d.RelatedSessionsIndex = &tr
	if !d.RelatedSessionsIndexEnabled() {
		t.Error("explicit true should enable")
	}
}

func TestRelatedSessionsIndexParsesFalse(t *testing.T) {
	// A file that sets it false round-trips to a disabling *bool.
	f, err := os.CreateTemp(t.TempDir(), "*.toml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("[daemon]\nrelated_sessions_index = false\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	cfg, err := config.Load(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Daemon.RelatedSessionsIndexEnabled() {
		t.Error("related_sessions_index = false should disable")
	}
}

func TestPlanApprovalMode(t *testing.T) {
	if got := (config.PlanningConfig{}).PlanApprovalMode(); got != "on-risky" {
		t.Errorf("default = %q, want on-risky", got)
	}
	if got := (config.PlanningConfig{PlanApproval: "off"}).PlanApprovalMode(); got != "off" {
		t.Errorf("explicit = %q, want off", got)
	}
}

// TestToolsMaxOutputTokens covers the dispatcher output-cap knob
// (docs/tool-output-spill.md). It lives under [tools] rather than [agent]
// because [[agent]] is already the standing-agent table array — a regression
// here would silently move the key.
func TestToolsMaxOutputTokens(t *testing.T) {
	f, err := os.CreateTemp("", "nine-*.toml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString("[tools]\nmax_output_tokens = 8192\n\n[[agent]]\nid = \"watcher\"\ndescription = \"watch things\"\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	cfg, err := config.Load(f.Name())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Tools.MaxOutputTokens != 8192 {
		t.Errorf("Tools.MaxOutputTokens = %d, want 8192", cfg.Tools.MaxOutputTokens)
	}
	// [tools] must not disturb the [[agent]] table array.
	if len(cfg.Agents) != 1 || cfg.Agents[0].ID != "watcher" {
		t.Errorf("Agents = %+v, want the one standing agent to survive alongside [tools]", cfg.Agents)
	}
}

// An unset cap stays zero so the dispatcher keeps agent.DefaultMaxOutputTokens.
func TestToolsMaxOutputTokensUnset(t *testing.T) {
	f, err := os.CreateTemp("", "nine-*.toml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString("[llm]\nprovider = \"ollama\"\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	cfg, err := config.Load(f.Name())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Tools.MaxOutputTokens != 0 {
		t.Errorf("Tools.MaxOutputTokens = %d, want 0 (meaning: use the default)", cfg.Tools.MaxOutputTokens)
	}
}

// NINE_PLUGINS_DISABLED is the container's way to withhold a plugin without
// baking a second nine.toml into an image (R-PLUG.14). It replaces the file's
// list rather than merging, so what the operator sets is what they get.
func TestPluginsDisabledEnvOverride(t *testing.T) {
	cfg := &config.Config{}
	cfg.Plugins.Disabled = []string{"http"}

	t.Setenv("NINE_PLUGINS_DISABLED", "shell, browser ,,")
	config.ApplyEnvOverrides(cfg)

	if got := strings.Join(cfg.Plugins.Disabled, ","); got != "shell,browser" {
		t.Errorf("Disabled = %q, want %q (trimmed, empties dropped)", got, "shell,browser")
	}
}

// An unset variable must leave the file's list alone rather than clearing it.
func TestPluginsDisabledEnvUnsetKeepsFile(t *testing.T) {
	cfg := &config.Config{}
	cfg.Plugins.Disabled = []string{"shell"}

	t.Setenv("NINE_PLUGINS_DISABLED", "")
	config.ApplyEnvOverrides(cfg)

	if got := strings.Join(cfg.Plugins.Disabled, ","); got != "shell" {
		t.Errorf("Disabled = %q, want the file's value %q", got, "shell")
	}
}
