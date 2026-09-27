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
	for _, key := range []string{"NINE_WORKSPACE_ROOT", "OPENAI_API_KEY"} {
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

// A well-formed net.http grant parses.
func TestNetHTTPGrantParses(t *testing.T) {
	cfg, err := loadTOML(t, `
[tool.fetcher.capabilities.net.http]
allow_hosts = ["api.example.com", "*.cdn.example.net"]
methods     = ["GET", "POST"]
max_bytes   = 1048576
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	g := cfg.Tool["fetcher"].Capabilities.Net.HTTP
	if g == nil {
		t.Fatal("grant did not parse")
	}
	if len(g.AllowHosts) != 2 || len(g.Methods) != 2 || g.MaxBytes != 1048576 {
		t.Errorf("grant = %+v", g)
	}
}

// The egress allowlist is only meaningful if the operator must be specific. Each
// of these is a way of *accidentally* saying "anywhere". A bare "*" is not among
// them: it says "anywhere" deliberately and in full, and is accepted — see
// TestNetHTTPGrantAcceptsDeliberateWildcard.
func TestNetHTTPGrantRejectsVagueAllowlists(t *testing.T) {
	for _, tc := range []struct{ name, table, want string }{
		{"no allow_hosts", "methods = [\"GET\"]", "needs allow_hosts"},
		{"no methods", "allow_hosts = [\"api.example.com\"]", "needs methods"},
		{"a URL rather than a hostname", "allow_hosts = [\"https://api.example.com/v1\"]\nmethods = [\"GET\"]", "must be a hostname"},
		{"an interior wildcard", "allow_hosts = [\"api.*.example.com\"]\nmethods = [\"GET\"]", "exact host"},
		{"an unknown method", "allow_hosts = [\"api.example.com\"]\nmethods = [\"TRACE\"]", "not a permitted method"},
		{"a negative cap", "allow_hosts = [\"api.example.com\"]\nmethods = [\"GET\"]\nmax_bytes = -1", "must not be negative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadTOML(t, "[tool.fetcher.capabilities.net.http]\n"+tc.table+"\n")
			if err == nil {
				t.Fatal("the grant was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
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
	if !cfg.Tools.IsEnabled() || cfg.Tools.UserDir != "/etc/nine/tools.d" {
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

// An empty config must stay valid, and must resolve to the on-by-default posture:
// the host runs and the generated tier with it, because both carry defaults an
// unconfigured deployment needs rather than opt-ins it has to discover.
func TestEmptyConfigIsValid(t *testing.T) {
	cfg, err := loadTOML(t, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Tools.IsEnabled() {
		t.Error("[tools] enabled does not default to true")
	}
	if !cfg.Tools.Agent.IsEnabled() {
		t.Error("[tools.agent] enabled does not default to true")
	}
	if !cfg.Tools.GeneratedEnabled() {
		t.Error("GeneratedEnabled is false on an empty config")
	}
}

// Unset and false are different answers, which is the whole reason these two are
// pointers: an operator must be able to decline a default that is now on.
func TestToolsDisabledExplicitly(t *testing.T) {
	cfg, err := loadTOML(t, `
[tools]
enabled = false

[tools.agent]
enabled = false
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Tools.IsEnabled() {
		t.Error("[tools] enabled = false did not turn the host off")
	}
	if cfg.Tools.Agent.IsEnabled() {
		t.Error("[tools.agent] enabled = false did not turn the tier off")
	}
}

// The host gate sits above the tier gate: the generated tier runs on the host, so
// with the host off it is off whatever it asked for.
func TestGeneratedTierNeedsTheHost(t *testing.T) {
	cfg, err := loadTOML(t, `
[tools]
enabled = false

[tools.agent]
enabled = true
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Tools.Agent.IsEnabled() {
		t.Error("[tools.agent] enabled = true was not read")
	}
	if cfg.Tools.GeneratedEnabled() {
		t.Error("GeneratedEnabled is true with [tools] enabled = false")
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

// Deliberately not overridable, in either direction. Whether the sandboxed-tool
// host runs is an operator decision that belongs in nine.toml; an environment
// variable able to move it would mean a deployment gaining or losing a whole
// execution subsystem from a stray export. Now that the default is on, the
// direction that matters is off: a NINE_TOOLS_ENABLED=false must not silently
// take the file tools away.
func TestToolsEnabledIsNotEnvOverridable(t *testing.T) {
	t.Setenv("NINE_TOOLS_ENABLED", "false")
	t.Setenv("NINE_TOOLS_USER_DIR", "/tools.d")

	cfg := &Config{}
	ApplyEnvOverrides(cfg)

	if !cfg.Tools.IsEnabled() {
		t.Error("[tools] enabled was switched off by an environment variable")
	}
	if cfg.Tools.Enabled != nil {
		t.Error("an environment variable set the pointer at all")
	}
}

// A bare "*" is the one pattern an operator has to write out, so it is accepted
// where the vague forms above are not.
//
// It grants any *host*. It does not grant any *address*: the dial-time checks in
// internal/toolvm/ssrf.go run whatever the allowlist says, which is why
// permitting this is not the escalation the old refusal treated it as. The
// refusal pointed operators at a native plugin instead — a subprocess with the
// daemon's uid and none of those checks.
func TestNetHTTPGrantAcceptsDeliberateWildcard(t *testing.T) {
	cfg, err := loadTOML(t, "[tool.fetcher.capabilities.net.http]\nallow_hosts = [\"*\"]\nmethods = [\"GET\"]\n")
	if err != nil {
		t.Fatalf("a bare wildcard was refused: %v", err)
	}
	g := cfg.Tool["fetcher"].Capabilities.Net.HTTP
	if g == nil || len(g.AllowHosts) != 1 || g.AllowHosts[0] != "*" {
		t.Errorf("allow_hosts = %+v, want the wildcard preserved", g)
	}
}

// The `[bootstrap]` table reaches the struct: a tag typo here would leave the
// path empty and the feature silently off, which is exactly the failure the
// packaged self-model exists to avoid (adr/personality-pattern.md §4).
func TestBootstrapTableParses(t *testing.T) {
	cfg, err := loadTOML(t, "[bootstrap]\nself_model_path = \"/etc/nine/self-model.toml\"\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Bootstrap.SelfModelPath; got != "/etc/nine/self-model.toml" {
		t.Errorf("self_model_path = %q, want the configured path", got)
	}
}

// Absent is the shipped posture and must load cleanly.
func TestBootstrapTableAbsent(t *testing.T) {
	cfg, err := loadTOML(t, "[llm]\nmodel = \"x\"\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Bootstrap.SelfModelPath != "" {
		t.Errorf("self_model_path = %q, want empty", cfg.Bootstrap.SelfModelPath)
	}
}

// The workspace root has a default because an unset one is not neutral: every
// shipped tool that declares fs is skipped without it, and with [tools] enabled
// defaulting to true that would be the out-of-the-box state.
func TestWorkspaceRootPrecedence(t *testing.T) {
	t.Run("env wins over the file", func(t *testing.T) {
		t.Setenv("NINE_WORKSPACE_ROOT", "/from/env")
		cfg := &Config{}
		cfg.Workspace.Root = "/from/file"
		if got := cfg.WorkspaceRoot(); got != "/from/env" {
			t.Errorf("WorkspaceRoot() = %q, want the environment to win", got)
		}
	})

	t.Run("file wins over the default", func(t *testing.T) {
		t.Setenv("NINE_WORKSPACE_ROOT", "")
		cfg := &Config{}
		cfg.Workspace.Root = "/from/file"
		if got := cfg.WorkspaceRoot(); got != "/from/file" {
			t.Errorf("WorkspaceRoot() = %q, want the file's value", got)
		}
	})

	// The platform default is never empty, which is the property that matters:
	// whatever it resolves to, a shipped fs tool has a root to mount.
	t.Run("never empty", func(t *testing.T) {
		t.Setenv("NINE_WORKSPACE_ROOT", "")
		if got := (&Config{}).WorkspaceRoot(); got == "" {
			t.Error("WorkspaceRoot() is empty with nothing configured; every fs tool would skip")
		}
	})
}
