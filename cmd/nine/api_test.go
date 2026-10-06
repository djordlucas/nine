package main

import (
	"testing"

	"nine/internal/config"
)

// The config file's [api] settings must survive when no flag or environment
// variable sets them. A flag parser that pre-filled defaults used to overwrite
// them, so timeout_seconds = 900 still cut every turn off at 30s.
func TestMergeAPIConfigKeepsFileValuesWithoutFlags(t *testing.T) {
	for _, env := range []string{"NINE_API_PORT", "NINE_API_HOST", "NINE_API_TIMEOUT_SECONDS", "NINE_API_MAX_CONNECTIONS"} {
		t.Setenv(env, "")
	}
	file := config.APIConfig{Host: "0.0.0.0", Port: 9090, TimeoutSeconds: 900, MaxConnections: 7}

	got := mergeAPIConfig(file, parseAPIFlags([]string{"serve"}))

	if got.GetHost() != "0.0.0.0" || got.GetPort() != 9090 ||
		got.GetTimeoutSeconds() != 900 || got.GetMaxConnections() != 7 {
		t.Fatalf("file values lost: host=%q port=%d timeout=%d maxConn=%d",
			got.GetHost(), got.GetPort(), got.GetTimeoutSeconds(), got.GetMaxConnections())
	}
}

func TestMergeAPIConfigFlagsOverrideFile(t *testing.T) {
	t.Setenv("NINE_API_TIMEOUT_SECONDS", "")
	file := config.APIConfig{TimeoutSeconds: 900}

	got := mergeAPIConfig(file, parseAPIFlags([]string{"serve", "--timeout", "120"}))

	if got.GetTimeoutSeconds() != 120 {
		t.Fatalf("timeout = %d, want the flag's 120", got.GetTimeoutSeconds())
	}
}

func TestMergeAPIConfigDefaultsWhenNothingIsSet(t *testing.T) {
	t.Setenv("NINE_API_TIMEOUT_SECONDS", "")
	got := mergeAPIConfig(config.APIConfig{}, parseAPIFlags([]string{"serve"}))

	if got.GetTimeoutSeconds() != config.DefaultAPITimeoutSeconds || got.GetPort() != config.DefaultAPIPort {
		t.Fatalf("defaults not applied: timeout=%d port=%d", got.GetTimeoutSeconds(), got.GetPort())
	}
}
