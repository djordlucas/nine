package config

import (
	"os"
	"path/filepath"
	"time"

	"nine/internal/llm"
	"nine/internal/llm/anthropic"
	llmollama "nine/internal/llm/ollama"
)

// DefaultSocketPath is the Unix socket path used when none is configured.
const DefaultSocketPath = "/tmp/nine.sock"

// DefaultEventRetentionTurns is the per-agent turn window kept in the
// session_events journal when none is configured (docs/event-log.md v4).
const DefaultEventRetentionTurns = 200

// LoadDefault finds and loads nine.toml from standard locations, applying
// environment variable overrides. Returns an empty config if no file is found.
func LoadDefault() *Config {
	paths := []string{
		os.Getenv("NINE_CONFIG"),
		"nine.toml",
		"/nine.toml",
		os.Getenv("HOME") + "/.nine/nine.toml",
	}
	for _, p := range paths {
		if p == "" {
			continue
		}
		if cfg, err := Load(p); err == nil {
			ApplyEnvOverrides(cfg)
			return cfg
		}
	}
	cfg := &Config{}
	ApplyEnvOverrides(cfg)
	return cfg
}

// ApplyEnvOverrides applies NINE_* environment variables on top of cfg.
// The Docker Makefile and docker-compose pass these so one nine.toml serves both
// the native and container layouts: the container overrides the paths and
// endpoints that differ, without a second config file.
func ApplyEnvOverrides(cfg *Config) {
	if v := os.Getenv("NINE_LLM_PROVIDER"); v != "" {
		cfg.LLM.Provider = v
	}
	if v := os.Getenv("NINE_LLM_MODEL"); v != "" {
		cfg.LLM.Model = v
	}
	if v := os.Getenv("NINE_LLM_ENDPOINT"); v != "" {
		cfg.LLM.Endpoint = v
	}
	if v := os.Getenv("NINE_EMBED_PROVIDER"); v != "" {
		cfg.Embeddings.Provider = v
	}
	if v := os.Getenv("NINE_PLUGINS_BIN"); v != "" {
		cfg.Plugins.Bin = v
	}
	if v := os.Getenv("NINE_WORKSPACE_ROOT"); v != "" {
		cfg.Workspace.Root = v
	}
}

// SocketPath returns the configured Unix socket path, falling back to DefaultSocketPath.
func (cfg *Config) SocketPath() string {
	if cfg.Daemon.SocketPath != "" {
		return cfg.Daemon.SocketPath
	}
	return DefaultSocketPath
}

// DatabaseURL returns the PostgreSQL connection string for the memory store,
// honoring NINE_DATABASE_URL, then nine.toml's [memory].database_url, then a
// local docker-compose default (see docker-compose.yml).
func (cfg *Config) DatabaseURL() string {
	if v := os.Getenv("NINE_DATABASE_URL"); v != "" {
		return v
	}
	if cfg.Memory.DatabaseURL != "" {
		return cfg.Memory.DatabaseURL
	}
	return "postgres://nine:nine@localhost:5433/nine?sslmode=disable"
}

// EventRetention returns the session_events retention policy: how many recent
// turns to keep per agent, and the maximum age of any event (0 = no age limit).
// A configured turn count of 0 uses DefaultEventRetentionTurns; a negative value
// keeps all turns (returns 0, which disables turn-based pruning).
func (cfg *Config) EventRetention() (keepTurns int, maxAge time.Duration) {
	switch {
	case cfg.Daemon.EventRetentionTurns < 0:
		keepTurns = 0
	case cfg.Daemon.EventRetentionTurns == 0:
		keepTurns = DefaultEventRetentionTurns
	default:
		keepTurns = cfg.Daemon.EventRetentionTurns
	}
	if cfg.Daemon.EventRetentionDays > 0 {
		maxAge = time.Duration(cfg.Daemon.EventRetentionDays) * 24 * time.Hour
	}
	return keepTurns, maxAge
}

// BuildProvider constructs an LLM provider from cfg, with environment variable
// overrides applied on top.
func (cfg *Config) BuildProvider() llm.Provider {
	provider := cfg.LLM.Provider
	if e := os.Getenv("NINE_LLM_PROVIDER"); e != "" {
		provider = e
	}
	model := cfg.LLM.Model
	if e := os.Getenv("NINE_LLM_MODEL"); e != "" {
		model = e
	}
	switch provider {
	case "ollama":
		endpoint := cfg.LLM.Endpoint
		if e := os.Getenv("NINE_LLM_ENDPOINT"); e != "" {
			endpoint = e
		}
		if model == "" {
			model = "gemma4:e2b"
		}
		return llmollama.New(model, endpoint, cfg.LLM.NumCtx, cfg.LLM.ThinkingEnabled())
	default:
		apiKey := cfg.LLM.APIKey
		if apiKey == "" {
			apiKey = os.Getenv("ANTHROPIC_API_KEY")
		}
		if model == "" {
			model = "claude-haiku-4-5-20251001"
		}
		return anthropic.New(apiKey, model, cfg.LLM.Endpoint, cfg.LLM.TimeoutSeconds)
	}
}

// HITLTimeout returns the ask_human wait duration, defaulting to 5 minutes.
func (cfg *Config) HITLTimeout() time.Duration {
	if cfg.HITL.TimeoutSeconds > 0 {
		return time.Duration(cfg.HITL.TimeoutSeconds) * time.Second
	}
	return 5 * time.Minute
}

// ContextBudget returns the effective LLM context token budget.
func (cfg *Config) ContextBudget() int {
	if cfg.LLM.ContextBudget > 0 {
		return cfg.LLM.ContextBudget
	}
	// For Ollama, num_ctx is the actual model context window. Use it as the
	// budget so Nine fills the full window instead of the generic 8k default.
	if cfg.LLM.Provider == "ollama" && cfg.LLM.NumCtx > 0 {
		return cfg.LLM.NumCtx
	}
	return 8000
}

// PluginBin returns the full path to the named plugin binary.
func (cfg *Config) PluginBin(name string) string {
	if cfg.Plugins.Bin != "" {
		return filepath.Join(cfg.Plugins.Bin, name)
	}
	return filepath.Join("/data/bin", name)
}

// BuildQueue constructs an LLM queue from cfg.
func (cfg *Config) BuildQueue() *llm.Queue {
	return llm.NewQueue(cfg.BuildProvider(), max(cfg.LLM.MaxConcurrent, 1))
}

// PluginEnvs returns the extra environment variables required to start the named plugin.
func (cfg *Config) PluginEnvs(name string) []string {
	switch name {
	case "files":
		if cfg.Workspace.Root != "" {
			return []string{"NINE_WORKSPACE=" + cfg.Workspace.Root}
		}
		return nil
	case "browser":
		return []string{
			"BROWSER_HEADLESS=1",
			"BROWSER_TIMEOUT=30000",
			"BROWSER_VIEWPORT_WIDTH=1280",
			"BROWSER_VIEWPORT_HEIGHT=800",
		}
	default:
		return nil
	}
}
