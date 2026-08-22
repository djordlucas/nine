package config_test

import (
	"strings"
	"testing"

	"nine/internal/config"
)

// An unknown provider must be refused, and an unset one must not be — the
// difference between "I did not choose" and "I chose something you do not have".
func TestCheckProvider(t *testing.T) {
	for _, tc := range []struct {
		provider string
		wantErr  bool
	}{
		{"", false},
		{"ollama", false},
		{"ollamma", true},
		{"vllm", true},
		{"openai", true},
	} {
		cfg := &config.Config{}
		cfg.LLM.Provider = tc.provider
		err := cfg.CheckProvider()
		if (err != nil) != tc.wantErr {
			t.Errorf("provider %q: err = %v, want error = %v", tc.provider, err, tc.wantErr)
			continue
		}
		// The message has to name the offending value, or an operator cannot
		// tell which of several config sources supplied it.
		if err != nil && !strings.Contains(err.Error(), tc.provider) {
			t.Errorf("provider %q: error does not name it: %v", tc.provider, err)
		}
	}
}
