package config_test

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nine/internal/config"
)

// captureLogs redirects slog for the duration of a test and returns what was
// written, so a warning can be asserted on rather than assumed.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nine.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// An absent schema_version is a schema-1 file, not a zero. Every config
// written before the field existed omits it.
func TestLoadTreatsAbsentSchemaAsOne(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, "[llm]\nprovider = \"ollama\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SchemaVersion != config.CurrentConfigSchema {
		t.Errorf("SchemaVersion = %d, want %d", cfg.SchemaVersion, config.CurrentConfigSchema)
	}
}

func TestLoadRefusesANewerSchema(t *testing.T) {
	path := writeConfig(t, "schema_version = 99\n[llm]\nprovider = \"ollama\"\n")
	_, err := config.Load(path)
	var tooNew *config.SchemaTooNewError
	if !errors.As(err, &tooNew) {
		t.Fatalf("Load error = %v, want a SchemaTooNewError", err)
	}
	if tooNew.Found != 99 || tooNew.Known != config.CurrentConfigSchema {
		t.Errorf("found=%d known=%d, want 99 and %d", tooNew.Found, tooNew.Known, config.CurrentConfigSchema)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error does not name the file: %v", err)
	}
}

// The case the whole hard-stop exists for: a newer config must not degrade
// into booting on defaults, which would silently drop every setting the
// operator wrote.
func TestLoadDefaultStopsOnANewerSchema(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("NINE_CONFIG", writeConfig(t, "schema_version = 99\n"))

	cfg, err := config.LoadDefault()
	if err == nil {
		t.Fatal("LoadDefault accepted a config from a newer schema")
	}
	if cfg != nil {
		t.Errorf("LoadDefault returned a config alongside the error: %+v", cfg)
	}
	var tooNew *config.SchemaTooNewError
	if !errors.As(err, &tooNew) {
		t.Errorf("error = %v, want a SchemaTooNewError", err)
	}
}

// An unparseable file keeps the old behaviour: warn, try the next path.
// Only a schema mismatch is fatal.
func TestLoadDefaultStillFallsThroughOnAParseError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".nine"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".nine", "nine.toml"),
		[]byte("[llm]\nprovider = \"from-home\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NINE_CONFIG", writeConfig(t, "this is not valid toml ==="))

	cfg, err := config.LoadDefault()
	if err != nil {
		t.Fatalf("LoadDefault: %v", err)
	}
	if cfg.LLM.Provider != "from-home" {
		t.Errorf("provider = %q, want from-home — a parse error should fall through", cfg.LLM.Provider)
	}
}

func TestLoadWarnsOnUnknownKeys(t *testing.T) {
	logs := captureLogs(t)
	if _, err := config.Load(writeConfig(t,
		"[llm]\nprovider = \"ollama\"\nprovdier = \"typo\"\n")); err != nil {
		t.Fatalf("Load: %v", err)
	}
	out := logs.String()
	if !strings.Contains(out, "not recognised") {
		t.Errorf("no warning for an unknown key:\n%s", out)
	}
	if !strings.Contains(out, "provdier") {
		t.Errorf("the warning does not name the offending key:\n%s", out)
	}
}

// A misspelled key must not be mistaken for a valid one, and a valid one must
// not be reported. Guards against the warning becoming noise nobody reads.
//
// Both shipped configs are covered. The published one matters more: it reaches
// people who never open it, and a typo there would be reported to an operator who
// is not reading the container's logs.
func TestShippedConfigsProduceNoUnknownKeyWarning(t *testing.T) {
	for _, path := range []string{"../../nine.toml", "../../docker/nine.toml"} {
		t.Run(path, func(t *testing.T) {
			logs := captureLogs(t)
			if _, err := config.Load(path); err != nil {
				t.Fatalf("Load: %v", err)
			}
			if strings.Contains(logs.String(), "not recognised") {
				t.Errorf("%s reports unknown keys:\n%s", path, logs.String())
			}
		})
	}
}
