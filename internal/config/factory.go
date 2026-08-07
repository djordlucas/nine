package config

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
		cfg, err := Load(p)
		if err == nil {
			ApplyEnvOverrides(cfg)
			return cfg
		}
		// A missing file is normal — try the next path. Anything else (a TOML
		// parse error, a settings-validation error) means a config file is present
		// but unusable; surface it rather than silently falling through to an empty
		// config the operator did not intend.
		if !errors.Is(err, fs.ErrNotExist) {
			// path and err come from the operator's own config discovery, not an
			// untrusted source, so logging them verbatim is safe.
			slog.Warn("ignoring unusable config file", "path", p, "err", err) //nolint:gosec // G706: operator-controlled path
		}
	}
	cfg := &Config{}
	ApplyEnvOverrides(cfg)
	return cfg
}

// ApplyEnvOverrides applies NINE_* environment variables on top of cfg.
// The Docker Makefile's `up`/`up-hot` targets pass these so one nine.toml serves
// both the native and container layouts: the container overrides the paths and
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
	if v := os.Getenv("NINE_PLUGINS_USER_DIR"); v != "" {
		cfg.Plugins.UserDir = v
	}
	if v := os.Getenv("NINE_PLUGINS_CACHE_DIR"); v != "" {
		cfg.Plugins.CacheDir = v
	}
	if v := os.Getenv("NINE_WORKSPACE_ROOT"); v != "" {
		cfg.Workspace.Root = v
	}
	if v := os.Getenv("NINE_SKILLS_USER_DIR"); v != "" {
		cfg.Skills.UserDir = v
	}
	// Sandboxed tools (spec/contracts/toolvm.md). Only the path is overridable,
	// matching the plugin and skill dirs: whether the subsystem is on at all is a
	// deliberate operator decision that belongs in nine.toml, not something a
	// stray environment variable should be able to switch on.
	if v := os.Getenv("NINE_TOOLS_USER_DIR"); v != "" {
		cfg.Tools.UserDir = v
	}
}

// SocketPath returns the configured Unix socket path, falling back to DefaultSocketPath.
func (cfg *Config) SocketPath() string {
	if cfg.Daemon.SocketPath != "" {
		return cfg.Daemon.SocketPath
	}
	return DefaultSocketPath
}

// DatabasePath returns the filesystem path of the SQLite database backing the
// memory store, honoring NINE_DB_PATH, then nine.toml's [memory].path, then a
// platform default.
//
// The default follows the workspace: inside the container /data is the mounted
// volume that already holds the workspace, so the database lives beside it and a
// single volume carries all durable state. Natively it sits next to the config
// in ~/.nine.
//
// An operator-supplied value is checked for a URL scheme (dbPathError): Nine
// stored its state in PostgreSQL before the SQLite migration, and a leftover DSN
// is otherwise accepted verbatim as a relative path. Validation lives here rather
// than in Validate because it must cover the environment variable, which is read
// at call time, and because LoadDefault falls back to an unvalidated empty Config
// when no nine.toml is found — the exact case where a stale NINE_DB_PATH bites.
func (cfg *Config) DatabasePath() (string, error) {
	if v := os.Getenv("NINE_DB_PATH"); v != "" {
		return v, dbPathError(v, "NINE_DB_PATH")
	}
	if cfg.Memory.Path != "" {
		return cfg.Memory.Path, dbPathError(cfg.Memory.Path, "[memory].path")
	}
	if fi, err := os.Stat("/data"); err == nil && fi.IsDir() {
		return "/data/nine.db", nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "nine.db", nil
	}
	return filepath.Join(home, ".nine", "nine.db"), nil
}

// dsnSchemeRE matches a leading URL scheme (RFC 3986 §3.1) followed by "://".
// Requiring the slashes keeps a legitimate Windows drive path ("C:\db") and a
// relative path containing a colon from being mistaken for a DSN.
var dsnSchemeRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.\-]*://`)

// dbPathError rejects a database path that is a connection URL rather than a
// filesystem path. Without it SQLite treats the DSN as a relative path and
// silently creates a fresh, empty database in a directory tree named after the
// URL — e.g. "postgres://nine:nine@localhost:5432/nine" becomes the directory
// "postgres:/nine:nine@localhost:5432/". Every table is then empty, which is
// indistinguishable from total data loss even though the real database is
// untouched wherever it actually lives. source names the origin so the message
// points at the thing to edit.
func dbPathError(path, source string) error {
	if !dsnSchemeRE.MatchString(path) {
		return nil
	}
	return fmt.Errorf("%s is a connection URL (%q), not a file path: Nine stores all state in a single SQLite file since the PostgreSQL migration — set it to something like ~/.nine/nine.db (see docs/configuration.md)", source, path)
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

// PluginEnvs returns the extra environment variables to start the named plugin:
// Nine's built-in defaults with the operator's [plugin.<name>.settings] layered
// on top. Settings come last so an operator value wins on a duplicate key (e.g.
// BROWSER_HEADLESS), which is what lets an operator override a built-in default
// (docs/plugin-capabilities.md §3). It applies to every plugin, built-in or user;
// a plugin with no defaults and no settings gets an empty slice.
//
// The settings were validated at load (Config.Validate), so the error from
// pluginSettingsEnv here is defensive: on the impossible residual error the
// settings are dropped rather than crashing a spawn.
func (cfg *Config) PluginEnvs(name string) []string {
	env := cfg.pluginDefaults(name)
	if entry, ok := cfg.Plugin[name]; ok {
		if settings, err := pluginSettingsEnv(name, entry.Settings); err == nil {
			env = append(env, settings...)
		}
	}
	return env
}

// pluginDefaults returns the built-in default environment Nine ships for a
// plugin so it works out of the box. Operator settings layer on top (PluginEnvs).
func (cfg *Config) pluginDefaults(name string) []string {
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

// PluginCacheRoot returns the directory under which each plugin's cache dir is
// created (docs/plugin-capabilities.md §4): the configured [plugins].cache_dir,
// else os.UserCacheDir()/nine/plugins. It returns "" only when no directory is
// configured and the OS user cache dir cannot be resolved, in which case the
// daemon runs without per-plugin cache dirs rather than failing.
func (cfg *Config) PluginCacheRoot() string {
	if cfg.Plugins.CacheDir != "" {
		return cfg.Plugins.CacheDir
	}
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		return ""
	}
	return filepath.Join(base, "nine", "plugins")
}

// PluginPersistCache reports whether the named plugin's cache dir persists across
// restarts (docs/plugin-capabilities.md §4). Default false — an unconfigured
// plugin gets an ephemeral dir wiped when it exits.
func (cfg *Config) PluginPersistCache(name string) bool {
	return cfg.Plugin[name].PersistCache
}

// reservedPluginEnvKeys are the environment variables Nine computes freshly per
// spawn. An operator [plugin.<name>.settings] key naming one is a config error,
// because overriding it breaks the transport or the cache contract rather than
// merely changing a default. It is exactly these three — not the whole NINE_
// prefix, so NINE_WORKSPACE and other pluginDefaults values stay overridable
// (docs/plugin-capabilities.md §3).
var reservedPluginEnvKeys = map[string]bool{
	"NINE_PLUGIN_SOCKET":           true,
	"NINE_PLUGIN_CACHE_DIR":        true,
	"NINE_PLUGIN_CACHE_PERSISTENT": true,
}

// pluginEnvKeyRe matches a POSIX environment-variable name. Keys are used
// verbatim as env-var names, so a key outside this shape is a config error.
var pluginEnvKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// pluginSettingsEnv turns a plugin's operator settings into KEY=VALUE strings,
// validating each key name and value type. Keys are emitted in sorted order for
// a reproducible spawn environment; because keys are distinct, order does not
// affect precedence. It is called both at load (Config.Validate, for the error)
// and at spawn (PluginEnvs, for the values).
func pluginSettingsEnv(name string, settings map[string]any) ([]string, error) {
	if len(settings) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(settings))
	for k := range settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if !pluginEnvKeyRe.MatchString(k) {
			return nil, fmt.Errorf("plugin %q: settings key %q is not a valid environment variable name", name, k)
		}
		if reservedPluginEnvKeys[k] {
			return nil, fmt.Errorf("plugin %q: settings key %q is reserved by Nine and cannot be set here", name, k)
		}
		val, err := stringifyPluginSetting(settings[k])
		if err != nil {
			return nil, fmt.Errorf("plugin %q: settings key %q: %w", name, k, err)
		}
		out = append(out, k+"="+val)
	}
	return out, nil
}

// stringifyPluginSetting renders a TOML scalar as an environment-variable value.
// A table or array (or any non-scalar such as a datetime) is a config error —
// env vars are strings, and inventing an encoding for structured values invites
// two plugins to disagree about it (docs/plugin-capabilities.md §3).
func stringifyPluginSetting(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case bool:
		return strconv.FormatBool(t), nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	case int:
		return strconv.Itoa(t), nil
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64), nil
	default:
		return "", fmt.Errorf("value must be a string, integer, float, or boolean, not %T", v)
	}
}
