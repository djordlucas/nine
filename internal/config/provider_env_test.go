package config

import "testing"

// Switching provider from the environment drops the file's endpoint and
// model, which belong to the file's provider, unless the environment names
// them too; naming the file's own provider again keeps them.
func TestProviderOverrideDropsTheOtherProvidersEndpoint(t *testing.T) {
	file := func() *Config {
		c := &Config{}
		c.LLM.Provider = ""
		c.LLM.Endpoint = "http://host.docker.internal:11434"
		c.LLM.Model = "qwen3.5:4b"
		return c
	}
	for _, tc := range []struct {
		name                      string
		env                       map[string]string
		wantEndpoint, wantModel   string
		wantProvider, wantIdModel string
	}{
		{"switch", map[string]string{"NINE_LLM_PROVIDER": "mistral"}, "", "", "mistral", "mistral-small-latest"},
		{"switch with its own", map[string]string{"NINE_LLM_PROVIDER": "mistral", "NINE_LLM_ENDPOINT": "https://gw.example/v1", "NINE_LLM_MODEL": "mistral-medium-latest"},
			"https://gw.example/v1", "mistral-medium-latest", "mistral", "mistral-medium-latest"},
		{"same provider", map[string]string{"NINE_LLM_PROVIDER": "ollama"}, "http://host.docker.internal:11434", "qwen3.5:4b", "ollama", "qwen3.5:4b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"NINE_LLM_PROVIDER", "NINE_LLM_ENDPOINT", "NINE_LLM_MODEL"} {
				t.Setenv(k, tc.env[k])
			}
			c := file()
			ApplyEnvOverrides(c)
			if c.LLM.Endpoint != tc.wantEndpoint || c.LLM.Model != tc.wantModel {
				t.Errorf("endpoint, model = %q, %q; want %q, %q", c.LLM.Endpoint, c.LLM.Model, tc.wantEndpoint, tc.wantModel)
			}
			if p, m := c.LLMIdentity(); p != tc.wantProvider || m != tc.wantIdModel {
				t.Errorf("identity = %s %s; want %s %s", p, m, tc.wantProvider, tc.wantIdModel)
			}
		})
	}
}
