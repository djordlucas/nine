package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadTOML(t *testing.T, body string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "nine.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

// §6.2: the daemon's environment holds LLM provider API keys, so a tool granted
// one would be a credential exfiltration primitive. These are refused at load —
// a config error, not a silent filter, so an operator who meant it finds out.
func TestReservedEnvKeysAreRefused(t *testing.T) {
	for _, key := range []string{"NINE_WORKSPACE_ROOT", "ANTHROPIC_API_KEY", "OPENAI_API_KEY"} {
		t.Run(key, func(t *testing.T) {
			_, err := loadTOML(t, `
[tool.leaky.capabilities]
env = ["`+key+`"]
`)
			if err == nil {
				t.Fatalf("granting %q was accepted", key)
			}
			if !strings.Contains(err.Error(), "reserved") {
				t.Errorf("error = %v, want it to name the refusal", err)
			}
		})
	}
}

func TestOrdinaryEnvKeyIsAccepted(t *testing.T) {
	cfg, err := loadTOML(t, `
[tool.tzaware.capabilities]
env = ["TZ"]
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Tool["tzaware"].Capabilities.Env; len(got) != 1 || got[0] != "TZ" {
		t.Errorf("Env = %v", got)
	}
}

// A relative mount would resolve against the daemon's working directory, which
// is not something an operator writing a grant is thinking about.
func TestFSMountMustBeAbsolute(t *testing.T) {
	_, err := loadTOML(t, `
[tool.reader.capabilities.fs]
read = [{ host = "data", guest = "/data" }]
`)
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("err = %v, want an absolute-path error", err)
	}
}

func TestFSMountNeedsBothPaths(t *testing.T) {
	_, err := loadTOML(t, `
[tool.reader.capabilities.fs]
read = [{ host = "/srv/data" }]
`)
	if err == nil || !strings.Contains(err.Error(), "guest") {
		t.Fatalf("err = %v, want a missing-guest-path error", err)
	}
}

// net.http is stage 4. Accepting the grant would advertise a boundary that does
// not exist yet, so it is refused by name at config load.
func TestNetHTTPGrantIsRefused(t *testing.T) {
	_, err := loadTOML(t, `
[tool.fetcher.capabilities.net.http]
allow_hosts = ["api.example.com"]
`)
	if err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("err = %v, want net.http refused", err)
	}
}

// The full shape from docs/sandboxed-tools.md §7 must parse.
func TestToolsSectionParses(t *testing.T) {
	cfg, err := loadTOML(t, `
[tools]
enabled   = true
user_dir  = "/etc/nine/tools.d"
timeout   = "5s"
memory_mb = 16

[tool.csv_stats.capabilities.fs]
read = [{ host = "/srv/data", guest = "/data" }]
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Tools.Enabled || cfg.Tools.UserDir != "/etc/nine/tools.d" {
		t.Errorf("[tools] = %+v", cfg.Tools)
	}
	if cfg.Tools.Timeout != "5s" || cfg.Tools.MemoryMB != 16 {
		t.Errorf("bounds = %q / %d", cfg.Tools.Timeout, cfg.Tools.MemoryMB)
	}
	mounts := cfg.Tool["csv_stats"].Capabilities.FS.Read
	if len(mounts) != 1 || mounts[0].Host != "/srv/data" || mounts[0].Guest != "/data" {
		t.Errorf("mounts = %+v", mounts)
	}
}

// An empty config must stay valid: the subsystem is off and nothing is required.
func TestEmptyConfigIsValid(t *testing.T) {
	cfg, err := loadTOML(t, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Tools.Enabled {
		t.Error("[tools] enabled defaults to true")
	}
}

// The container points at its mounted tools directory through this override,
// the same way it does for plugins and skills.
func TestToolsUserDirEnvOverride(t *testing.T) {
	t.Setenv("NINE_TOOLS_USER_DIR", "/tools.d")

	cfg := &Config{}
	cfg.Tools.UserDir = "./tools.d"
	ApplyEnvOverrides(cfg)

	if cfg.Tools.UserDir != "/tools.d" {
		t.Errorf("UserDir = %q, want the override to win", cfg.Tools.UserDir)
	}
}

// Deliberately not overridable. Turning the sandboxed-tool host on is an
// operator decision that belongs in nine.toml; an environment variable able to
// switch it on would mean a deployment gaining a whole execution subsystem from
// a stray export.
func TestToolsEnabledIsNotEnvOverridable(t *testing.T) {
	t.Setenv("NINE_TOOLS_ENABLED", "true")
	t.Setenv("NINE_TOOLS_USER_DIR", "/tools.d")

	cfg := &Config{}
	ApplyEnvOverrides(cfg)

	if cfg.Tools.Enabled {
		t.Error("[tools] enabled was switched on by an environment variable")
	}
}
