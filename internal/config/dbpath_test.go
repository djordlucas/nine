package config_test

import (
	"strings"
	"testing"

	"nine/internal/config"
)

// TestDatabasePathRejectsDSN covers the post-migration trap: Nine stored its
// state in PostgreSQL before the SQLite migration, so a leftover DSN is a
// plausible leftover in both the environment and nine.toml. SQLite accepts one
// as a relative path and silently creates an empty database under a directory
// tree named after the URL, which is indistinguishable from data loss.
func TestDatabasePathRejectsDSN(t *testing.T) {
	dsns := []string{
		"postgres://nine:nine@localhost:5432/nine",
		"postgresql://localhost/nine",
		"mysql://root@127.0.0.1:3306/nine",
		"sqlite://relative/nine.db",
	}
	for _, dsn := range dsns {
		t.Run(dsn, func(t *testing.T) {
			t.Setenv("NINE_DB_PATH", dsn)
			_, err := (&config.Config{}).DatabasePath()
			if err == nil {
				t.Fatalf("DatabasePath(%q) = nil error, want rejection", dsn)
			}
			// The message has to name the offending setting, or an operator
			// cannot tell whether to edit the env or nine.toml.
			if !strings.Contains(err.Error(), "NINE_DB_PATH") {
				t.Errorf("error %q does not name the source setting", err)
			}
		})
	}
}

// TestDatabasePathRejectsDSNFromTOML checks the other source. [memory].path is
// reachable even when the env is clean.
func TestDatabasePathRejectsDSNFromTOML(t *testing.T) {
	t.Setenv("NINE_DB_PATH", "")
	cfg := &config.Config{}
	cfg.Memory.Path = "postgres://nine:nine@localhost:5432/nine"

	_, err := cfg.DatabasePath()
	if err == nil {
		t.Fatal("DatabasePath() = nil error, want rejection")
	}
	if !strings.Contains(err.Error(), "[memory].path") {
		t.Errorf("error %q does not name the source setting", err)
	}
}

// TestDatabasePathAcceptsFilePaths guards the false-positive direction. A colon
// is legal in a filesystem path, so the check requires a scheme *and* "://" —
// rejecting on a bare colon would break Windows drive paths and any relative
// path that happens to contain one.
func TestDatabasePathAcceptsFilePaths(t *testing.T) {
	paths := []string{
		"/data/nine.db",
		"~/.nine/nine.db",
		"nine.db",
		"./weird:name/nine.db",
		`C:\Users\me\nine.db`,
		"/tmp/a:b/nine.db",
	}
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			t.Setenv("NINE_DB_PATH", p)
			got, err := (&config.Config{}).DatabasePath()
			if err != nil {
				t.Fatalf("DatabasePath(%q) = %v, want no error", p, err)
			}
			if got != p {
				t.Errorf("DatabasePath() = %q, want %q", got, p)
			}
		})
	}
}

// TestDatabasePathPrecedence pins the documented order: env wins over
// [memory].path, which wins over the platform default.
func TestDatabasePathPrecedence(t *testing.T) {
	cfg := &config.Config{}
	cfg.Memory.Path = "/from/toml.db"

	t.Setenv("NINE_DB_PATH", "/from/env.db")
	got, err := cfg.DatabasePath()
	if err != nil {
		t.Fatalf("DatabasePath: %v", err)
	}
	if got != "/from/env.db" {
		t.Errorf("with env set, DatabasePath() = %q, want /from/env.db", got)
	}

	t.Setenv("NINE_DB_PATH", "")
	got, err = cfg.DatabasePath()
	if err != nil {
		t.Fatalf("DatabasePath: %v", err)
	}
	if got != "/from/toml.db" {
		t.Errorf("with env unset, DatabasePath() = %q, want /from/toml.db", got)
	}
}
