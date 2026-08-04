package plugin

import (
	"strings"
	"testing"
)

// sanitizedHostEnv must pass allowlisted host vars through and withhold
// everything else — most importantly secrets the daemon was started with.
func TestSanitizedHostEnvWithholdsSecrets(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("LC_CTYPE", "en_US.UTF-8")
	t.Setenv("DATABASE_URL", "postgres://user:pw@host/db")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "shh")
	t.Setenv("NINE_DATABASE_URL", "postgres://leak")

	got := make(map[string]string)
	for _, kv := range sanitizedHostEnv() {
		if name, val, ok := strings.Cut(kv, "="); ok {
			got[name] = val
		}
	}

	if got["PATH"] != "/usr/bin:/bin" {
		t.Errorf("PATH = %q, want it passed through", got["PATH"])
	}
	if got["LC_CTYPE"] != "en_US.UTF-8" {
		t.Errorf("LC_CTYPE = %q, want LC_* passed through", got["LC_CTYPE"])
	}
	for _, secret := range []string{"DATABASE_URL", "AWS_SECRET_ACCESS_KEY", "NINE_DATABASE_URL"} {
		if v, ok := got[secret]; ok {
			t.Errorf("%s leaked to plugin env (=%q); scrub must withhold it", secret, v)
		}
	}
}
