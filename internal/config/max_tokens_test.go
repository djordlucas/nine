package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"nine/internal/config"
)

// TestMaxReplyTokens covers [llm] max_tokens: unset keeps the 2048 default,
// the file sets it, and NINE_LLM_MAX_TOKENS overrides the file when it is a
// positive integer.
func TestMaxReplyTokens(t *testing.T) {
	load := func(t *testing.T, toml string) *config.Config {
		t.Helper()
		path := filepath.Join(t.TempDir(), "nine.toml")
		if err := os.WriteFile(path, []byte(toml), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	t.Run("unset keeps the default", func(t *testing.T) {
		cfg := load(t, "[llm]\nprovider = \"mistral\"\n")
		if got := cfg.MaxReplyTokens(); got != config.DefaultMaxReplyTokens {
			t.Fatalf("MaxReplyTokens() = %d, want %d", got, config.DefaultMaxReplyTokens)
		}
	})

	t.Run("the file sets it", func(t *testing.T) {
		cfg := load(t, "[llm]\nmax_tokens = 8192\n")
		if got := cfg.MaxReplyTokens(); got != 8192 {
			t.Fatalf("MaxReplyTokens() = %d, want 8192", got)
		}
	})

	t.Run("the environment overrides the file", func(t *testing.T) {
		cfg := load(t, "[llm]\nmax_tokens = 4096\n")
		t.Setenv("NINE_LLM_MAX_TOKENS", "16384")
		config.ApplyEnvOverrides(cfg)
		if got := cfg.MaxReplyTokens(); got != 16384 {
			t.Fatalf("MaxReplyTokens() = %d, want 16384", got)
		}
	})

	t.Run("a bad environment value is ignored", func(t *testing.T) {
		cfg := load(t, "[llm]\nmax_tokens = 4096\n")
		t.Setenv("NINE_LLM_MAX_TOKENS", "lots")
		config.ApplyEnvOverrides(cfg)
		if got := cfg.MaxReplyTokens(); got != 4096 {
			t.Fatalf("MaxReplyTokens() = %d, want 4096", got)
		}
	})
}
