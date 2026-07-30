package plugin

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// ephemeralCacheDirRe matches an ephemeral cache dir name: any base ending in a
// dot and 8 hex chars (the crypto/rand suffix allocCacheDir writes, mirroring
// allocSocketPath). Such a dir is disposable by definition, so the boot sweep
// reclaims it; a persistent dir (<name>, no suffix) never matches.
var ephemeralCacheDirRe = regexp.MustCompile(`\.[0-9a-f]{8}$`)

// allocCacheDir creates the named plugin's cache directory under the manager's
// cache root and returns its path plus whether it is ephemeral. A persistent
// plugin gets <root>/<name>, reused across restarts; every other plugin gets
// <root>/<name>.<rand8>, unique per process so two instances never share scratch
// and the boot sweep can safely reclaim it (docs/plugin-capabilities.md §4).
// With no cache root configured it returns ("", true, nil): cache dirs are off.
func (m *Manager) allocCacheDir(name string) (dir string, ephemeral bool, err error) {
	m.mu.Lock()
	root := m.cacheRoot
	persistFn := m.persistCache
	m.mu.Unlock()

	if root == "" {
		return "", true, nil
	}

	persist := persistFn != nil && persistFn(name)
	if persist {
		dir = filepath.Join(root, name)
	} else {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", true, fmt.Errorf("alloc cache dir: %w", err)
		}
		dir = filepath.Join(root, fmt.Sprintf("%s.%s", name, hex.EncodeToString(b[:])))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", !persist, fmt.Errorf("create cache dir: %w", err)
	}
	return dir, !persist, nil
}

// SweepCache removes leftover ephemeral cache dirs under the cache root — those
// matching <name>.<hex>, orphaned by a previous daemon that exited without
// stopping its plugins (the common path today; see docs/plugin-capabilities.md
// §4). It is meant to run at boot before any plugin starts, so every match is
// disposable. Persistent dirs are left untouched, and a missing or unconfigured
// root is a no-op.
func (m *Manager) SweepCache() {
	m.mu.Lock()
	root := m.cacheRoot
	m.mu.Unlock()
	if root == "" {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return // absent root or unreadable: nothing to reclaim
	}
	for _, e := range entries {
		if e.IsDir() && ephemeralCacheDirRe.MatchString(e.Name()) {
			os.RemoveAll(filepath.Join(root, e.Name())) //nolint:errcheck // best-effort
		}
	}
}

// persistentEnv renders the NINE_PLUGIN_CACHE_PERSISTENT value.
func persistentEnv(persistent bool) string {
	if persistent {
		return "1"
	}
	return "0"
}
