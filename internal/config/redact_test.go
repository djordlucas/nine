package config

import (
	"strings"
	"testing"
)

// The viewer shows what is set and no credential: keys, tokens and an MCP
// server's env and headers are hidden, and an unset setting is left out.
func TestRedactedTOMLHidesCredentials(t *testing.T) {
	cfg := &Config{}
	cfg.LLM.Provider = "mistral"
	cfg.LLM.Model = "mistral-small-latest"
	cfg.LLM.APIKey = "sk-live-123"
	cfg.API.AuthToken = "tok-456"
	cfg.MCP.Servers = []MCPServer{{
		Name:    "github",
		URL:     "https://mcp.example",
		Headers: map[string]string{"Authorization": "Bearer gh-789"},
		Env:     map[string]string{"GITHUB_TOKEN": "gh-000"},
	}}

	out, err := cfg.RedactedTOML()
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"sk-live-123", "tok-456", "gh-789", "gh-000"} {
		if strings.Contains(out, secret) {
			t.Errorf("a credential is shown: %q in\n%s", secret, out)
		}
	}
	for _, unset := range []string{"key_path", "[api.tls]", "timeout_seconds = 0"} {
		if strings.Contains(out, unset) {
			t.Errorf("an unset setting is shown: %q in\n%s", unset, out)
		}
	}
	for _, want := range []string{`model = "mistral-small-latest"`, `api_key = "` + Redacted + `"`, `auth_token = "` + Redacted + `"`, `Authorization = "` + Redacted + `"`, `[llm]`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
}
