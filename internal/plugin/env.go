package plugin

import (
	"os"
	"strings"
)

// hostEnvAllowlist names the host environment variables a spawned plugin is
// allowed to inherit from the daemon. Everything else in the daemon's
// environment is withheld — most importantly any *_TOKEN / *_KEY / cloud
// credentials the operator exported into the daemon's process, and the location
// of the database, which a plugin has no business opening directly. A plugin
// still receives the NINE_* vars the daemon sets and
// any operator-configured [plugin.<name>.settings]; those arrive as explicit
// spawn env, not through this inheritance (see Manager.Start / newClient).
//
// The list is deliberately small and benign: enough for a plugin to find
// executables, resolve temp/locale/timezone, and trust the system CA bundle.
// LC_* is matched by prefix (locale categories are open-ended).
var hostEnvAllowlist = map[string]bool{
	"PATH":          true,
	"HOME":          true,
	"TMPDIR":        true,
	"TMP":           true,
	"TEMP":          true,
	"TZ":            true,
	"TERM":          true,
	"USER":          true,
	"LOGNAME":       true,
	"LANG":          true,
	"LANGUAGE":      true,
	"SSL_CERT_FILE": true, // OpenSSL/Go CA bundle overrides — needed for HTTPS
	"SSL_CERT_DIR":  true,
}

// sanitizedHostEnv returns the allowlisted subset of the daemon's environment to
// hand to a spawned plugin. It replaces the previous wholesale os.Environ()
// inheritance, which leaked every daemon secret into plugin processes we do not
// necessarily trust. Callers append the daemon's explicit
// NINE_* / settings env after this base.
func sanitizedHostEnv() []string {
	environ := os.Environ()
	out := make([]string, 0, len(hostEnvAllowlist))
	for _, kv := range environ {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if hostEnvAllowlist[name] || strings.HasPrefix(name, "LC_") {
			out = append(out, kv)
		}
	}
	return out
}
