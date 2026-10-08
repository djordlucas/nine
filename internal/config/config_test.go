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
	cfg, err := config.LoadDefault()
	if err != nil {
		t.Fatalf("LoadDefault: %v", err)
	}
	if cfg.LLM.Provider != "from-nine-config" {
		t.Errorf("with NINE_CONFIG set, provider = %q, want from-nine-config", cfg.LLM.Provider)
	}

	// Unset $NINE_CONFIG (and no ./nine.toml, no /nine.toml): falls through to
	// $HOME/.nine/nine.toml.
	t.Setenv("NINE_CONFIG", "")
	cfg, err = config.LoadDefault()
	if err != nil {
		t.Fatalf("LoadDefault: %v", err)
	}
	if cfg.LLM.Provider != "from-home" {
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
// (adr/tool-output-spill.md). It lives under [tools] rather than [agent]
// because [[agent]] is already the standing-agent table array — a regression
// here would silently move the key.
func TestToolsMaxOutputTokens(t *testing.T) {
	f, err := os.CreateTemp("", "nine-*.toml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString("[tools]\nmax_output_tokens = 8192\n\n[[process]]\nname = \"watcher\"\ntool = \"pursue\"\ngoal = \"watch things\"\n"); err != nil {
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
	// [tools] must not disturb the [[process]] table array.
	if len(cfg.Process) != 1 || cfg.Process[0].Name != "watcher" {
		t.Errorf("Process = %+v, want the one goal process to survive alongside [tools]", cfg.Process)
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

// No flavour of "empty" may clear the list. An env var that could re-enable
// `shell` is a hazard in the one direction this setting must never move by
// accident, and "" vs "," vs " " must not mean different things.
func TestPluginsDisabledEnvEmptyKeepsFile(t *testing.T) {
	for _, v := range []string{"", ",", " ", " , , "} {
		t.Run("value="+v, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Plugins.Disabled = []string{"shell"}

			t.Setenv("NINE_PLUGINS_DISABLED", v)
			config.ApplyEnvOverrides(cfg)

			if got := strings.Join(cfg.Plugins.Disabled, ","); got != "shell" {
				t.Errorf("with NINE_PLUGINS_DISABLED=%q, Disabled = %q, want the file's %q", v, got, "shell")
			}
		})
	}
}

// [[mcp.server]] mistakes are caught at load, where the operator can act on
// them, rather than surfacing later as a plugin that mysteriously never
// appeared. The env/headers pairing matters because silently ignoring the wrong
// one would leave someone believing they had passed a token.
func TestMCPServerValidation(t *testing.T) {
	cases := []struct {
		name    string
		toml    string
		wantErr string
	}{
		{"stdio ok", `[[mcp.server]]
name = "github"
command = "npx"`, ""},
		{"http ok", `[[mcp.server]]
name = "hosted"
url = "https://mcp.example.com/rpc"`, ""},
		{"missing name", `[[mcp.server]]
command = "npx"`, "name is required"},
		{"no transport", `[[mcp.server]]
name = "x"`, "needs command"},
		{"both transports", `[[mcp.server]]
name = "x"
command = "npx"
url = "https://example.com"`, "not both"},
		{"duplicate names", `[[mcp.server]]
name = "dup"
command = "a"
[[mcp.server]]
name = "dup"
command = "b"`, "duplicate"},
		{"underscore in name", `[[mcp.server]]
name = "bad_name"
command = "npx"`, "alphanumeric"},
		{"env with url", `[[mcp.server]]
name = "x"
url = "https://example.com"
[mcp.server.env]
TOKEN = "t"`, "use headers with url"},
		{"headers with command", `[[mcp.server]]
name = "x"
command = "npx"
[mcp.server.headers]
Authorization = "Bearer t"`, "use env with command"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := filepath.Join(t.TempDir(), "nine.toml")
			if err := os.WriteFile(f, []byte(c.toml), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := config.Load(f)
			switch {
			case c.wantErr == "" && err != nil:
				t.Errorf("Load: unexpected error %v", err)
			case c.wantErr != "" && err == nil:
				t.Errorf("Load: want an error mentioning %q, got nil", c.wantErr)
			case c.wantErr != "" && !strings.Contains(err.Error(), c.wantErr):
				t.Errorf("Load error = %q, want it to mention %q", err, c.wantErr)
			}
		})
	}
}

// The packaged self-model's path is overridable by environment, like the plugin,
// skill and tool directories: an image that mounts its own file should not have
// to rewrite the config it inherited (adr/personality-pattern.md §4).
func TestBootstrapSelfModelPathFromEnv(t *testing.T) {
	cfg := &config.Config{}
	cfg.Bootstrap.SelfModelPath = "/from/file.toml"

	t.Setenv("NINE_BOOTSTRAP_SELF_MODEL", "/from/env.toml")
	config.ApplyEnvOverrides(cfg)
	if cfg.Bootstrap.SelfModelPath != "/from/env.toml" {
		t.Errorf("self_model_path = %q, want the environment's value", cfg.Bootstrap.SelfModelPath)
	}

	// An empty variable leaves the file's value alone rather than clearing it,
	// which is what every other NINE_* override does.
	t.Setenv("NINE_BOOTSTRAP_SELF_MODEL", "")
	cfg.Bootstrap.SelfModelPath = "/from/file.toml"
	config.ApplyEnvOverrides(cfg)
	if cfg.Bootstrap.SelfModelPath != "/from/file.toml" {
		t.Errorf("self_model_path = %q, want the file's value", cfg.Bootstrap.SelfModelPath)
	}
}

func loadString(t *testing.T, body string) (*config.Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nine.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return config.Load(path)
}

// A standing agent, a predicate piping into it, reflection attached to its
// session and a plain standing tool, as [[process]] blocks.
func TestProcessBlocksLoad(t *testing.T) {
	cfg, err := loadString(t, `
[processes]
max_running = 6
authoritative = true

[[process]]
name = "sec-watch"
tool = "pursue"
goal = "Watch the repo for security issues"
role = "monitor"
schedule = "0 9 * * 1-5"

[[process]]
name = "cve-scan"
tool = "cve_scan"
every = "10s"
report_to = "sec-watch"

[[process]]
name = "sec-watch-reflect"
tool = "reflect"
every = "30m"
session = "sec-watch"

[[process]]
name = "tidy"
tool = "tidy_logs"
schedule = "0 3 * * *"
args = { keep = 7 }
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Process) != 4 || cfg.Processes.MaxRunningOrDefault() != 6 || !cfg.Processes.Authoritative {
		t.Errorf("processes = %+v, limits = %+v", cfg.Process, cfg.Processes)
	}
	if (config.ProcessesConfig{}).MaxRunningOrDefault() != config.DefaultMaxRunning {
		t.Error("max_running does not default")
	}
}

// Every mistake in a [[process]] block is refused at load, with its reason.
func TestProcessBlockMistakesAreRefused(t *testing.T) {
	cases := map[string]string{
		"no name":              "[[process]]\ntool = \"x\"\n",
		"no tool":              "[[process]]\nname = \"a\"\n",
		"reserved colon":       "[[process]]\nname = \"goal:a\"\ntool = \"x\"\n",
		"duplicate":            "[[process]]\nname = \"a\"\ntool = \"x\"\n[[process]]\nname = \"a\"\ntool = \"y\"\n",
		"both clocks":          "[[process]]\nname = \"a\"\ntool = \"x\"\nevery = \"1m\"\nschedule = \"* * * * *\"\n",
		"bad every":            "[[process]]\nname = \"a\"\ntool = \"x\"\nevery = \"soon\"\n",
		"bad schedule":         "[[process]]\nname = \"a\"\ntool = \"x\"\nschedule = \"not cron\"\n",
		"goal not pursue":      "[[process]]\nname = \"a\"\ntool = \"x\"\ngoal = \"g\"\n",
		"unknown session":      "[[process]]\nname = \"a\"\ntool = \"x\"\nsession = \"nobody\"\n",
		"unknown report_to":    "[[process]]\nname = \"a\"\ntool = \"x\"\nreport_to = \"nobody\"\n",
		"attached with role":   "[[process]]\nname = \"o\"\ntool = \"pursue\"\ngoal = \"g\"\n[[process]]\nname = \"a\"\ntool = \"reflect\"\nsession = \"o\"\nrole = \"x\"\n",
		"budget above default": "[[process]]\nname = \"a\"\ntool = \"x\"\nbudget = { turns_per_day = 500 }\n",
		"budget above ceiling": "[processes]\nbudget = { tokens_per_day = 1000 }\n[[process]]\nname = \"a\"\ntool = \"x\"\nbudget = { tokens_per_day = 2000 }\n",
		"negative budget":      "[processes]\nbudget = { turns_per_day = -1 }\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := loadString(t, body); err == nil {
				t.Errorf("Load accepted:\n%s", body)
			}
		})
	}
}

// The blocks [[process]] replaced fail loudly, naming the replacement: an
// ignored [[agent]] would make an agent vanish without a word.
func TestRetiredBlocksAreRefused(t *testing.T) {
	for _, body := range []string{
		"[[agent]]\nid = \"w\"\ndescription = \"watch\"\n",
		"[[standing_tool]]\nid = \"s\"\ntool = \"x\"\ninterval = \"1m\"\n",
		"[daemon]\nstanding_agents_authoritative = true\n",
		"[daemon]\nmax_goal_sessions = 5\n",
		"[tools.agent]\nmax_standing = 2\n",
	} {
		_, err := loadString(t, body)
		if err == nil || !strings.Contains(err.Error(), "replaced by") {
			t.Errorf("Load(%q) = %v, want an error naming the replacement", body, err)
		}
	}
}

// A process's budget is [processes] budget, defaulted, with each field the
// block sets taking its place: a block can only lower it, which Load enforces.
func TestProcessBudget(t *testing.T) {
	cfg, err := loadString(t, "[processes]\nbudget = { turns_per_day = 50 }\n"+
		"[[process]]\nname = \"a\"\ntool = \"x\"\nbudget = { tokens_per_day = 1000 }\n")
	if err != nil {
		t.Fatal(err)
	}
	ceiling := cfg.Processes.BudgetOrDefault()
	if ceiling != (config.BudgetConfig{TurnsPerDay: 50, TokensPerDay: config.DefaultBudgetTokensPerDay}) {
		t.Errorf("ceiling = %+v", ceiling)
	}
	if got := cfg.Process[0].Budget.Within(ceiling); got != (config.BudgetConfig{TurnsPerDay: 50, TokensPerDay: 1000}) {
		t.Errorf("process budget = %+v, want 50 turns and 1000 tokens", got)
	}
	if got := (config.ProcessesConfig{}).BudgetOrDefault(); got != (config.BudgetConfig{TurnsPerDay: 200, TokensPerDay: 2_000_000}) {
		t.Errorf("default budget = %+v", got)
	}
}
