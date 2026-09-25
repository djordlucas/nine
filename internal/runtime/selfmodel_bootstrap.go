package runtime

import (
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"nine/internal/memory"
)

// bootstrapSentinel marks a database whose packaged self-model has been applied.
//
// The check is a sentinel rather than "is self/identity set?" because the two
// answer different questions. `self/identity` is set by BootstrapSelfKV on every
// fresh database, so keying on it would mean the packaged file only ever won a
// race with the generic default. The sentinel says *this file already ran here*,
// which is the thing that must happen once.
const bootstrapSentinel = "self/_bootstrapped"

// selfModelSectionKey is where a bootstrap section lands in KV: `[identity]`
// becomes `self/identity`.
//
// One key per section, holding the section's rendered lines — not one key per
// field. The self-model assembler reads whole keys by name
// (internal/selfmodel), so a `self/identity/name` written per the original
// sketch in adr/personality-pattern.md §4.2.2 would never reach a turn: the
// model would be handed the generic `self/identity` instead, and the packaged
// identity would sit in the store unread. Rendering the section into the key the
// assembler already reads is what makes the feature do what it says.
func selfModelSectionKey(section string) string { return "self/" + section }

// BootstrapSelfModel applies the operator's packaged self-model, if one is
// configured and this database has not had it applied before. It reports whether
// it wrote anything.
//
// It runs *before* BootstrapSelfKV, so a file that provides `self/identity`
// keeps the generic default from ever being written — the defaults are a
// fallback for an instance nobody packaged, not a baseline to be overwritten.
//
// A malformed file is an error and stops the boot. The alternative — log it and
// carry on — starts an instance that believes it is generic Nine while its
// operator believes it is Alice, and the difference only surfaces in what the
// model says about itself much later.
func BootstrapSelfModel(store *memory.Store, path string) (bool, error) {
	if store == nil || strings.TrimSpace(path) == "" {
		return false, nil
	}

	if _, found, err := store.Get(bootstrapSentinel); err != nil {
		return false, fmt.Errorf("check %s: %w", bootstrapSentinel, err)
	} else if found {
		return false, nil
	}

	data, err := os.ReadFile(path) //nolint:gosec // operator-configured path
	if err != nil {
		if os.IsNotExist(err) {
			// A configured path that is not there is a deployment mistake worth
			// naming, but not one worth refusing to boot over: the instance still
			// runs, with the defaults, and the log says why it is generic.
			slog.Warn("self-model bootstrap file not found; using defaults", "path", path)
			return false, nil
		}
		return false, fmt.Errorf("read self-model bootstrap %s: %w", path, err)
	}

	var doc map[string]any
	if err := toml.Unmarshal(data, &doc); err != nil {
		return false, fmt.Errorf("parse self-model bootstrap %s: %w", path, err)
	}

	entries, err := renderSelfModel(doc)
	if err != nil {
		return false, fmt.Errorf("self-model bootstrap %s: %w", path, err)
	}
	if len(entries) == 0 {
		slog.Warn("self-model bootstrap file has no sections; using defaults", "path", path)
		return false, nil
	}

	// The sentinel is written last. A failure part-way leaves it unset, so the
	// next boot retries the whole file rather than resuming into a half-applied
	// self-model nobody can tell from a complete one.
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := store.Set(k, entries[k]); err != nil {
			return false, fmt.Errorf("write %s: %w", k, err)
		}
	}
	if err := store.Set(bootstrapSentinel, "true"); err != nil {
		return false, fmt.Errorf("write %s: %w", bootstrapSentinel, err)
	}

	slog.Info("applied packaged self-model", "path", path, "keys", len(entries))
	return true, nil
}

// renderSelfModel turns a parsed bootstrap file into KV entries.
//
// A table becomes one entry holding `field: value` lines, sorted so the same
// file always produces the same text. A scalar at the top level becomes its own
// entry, which is how a file can set `self/identity` directly without inventing
// a section for it.
func renderSelfModel(doc map[string]any) (map[string]string, error) {
	out := map[string]string{}
	for name, raw := range doc {
		section := strings.TrimSpace(name)
		if section == "" || strings.Contains(section, "/") {
			return nil, fmt.Errorf("section %q: a name must be non-empty and carry no %q", name, "/")
		}
		switch v := raw.(type) {
		case map[string]any:
			body, err := renderSection(v)
			if err != nil {
				return nil, fmt.Errorf("section %q: %w", section, err)
			}
			if body != "" {
				out[selfModelSectionKey(section)] = body
			}
		default:
			s, err := renderScalar(raw)
			if err != nil {
				return nil, fmt.Errorf("key %q: %w", section, err)
			}
			out[selfModelSectionKey(section)] = s
		}
	}
	return out, nil
}

// renderSection renders one table's fields as `name: value` lines.
func renderSection(tbl map[string]any) (string, error) {
	fields := make([]string, 0, len(tbl))
	for k := range tbl {
		fields = append(fields, k)
	}
	sort.Strings(fields)

	var b strings.Builder
	for _, f := range fields {
		s, err := renderScalar(tbl[f])
		if err != nil {
			return "", fmt.Errorf("field %q: %w", f, err)
		}
		fmt.Fprintf(&b, "%s: %s\n", f, s)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// renderScalar renders one value as the text a model will read.
//
// A nested table is refused rather than flattened: the self-model is prose a
// model reads, and a two-level structure has no sensible rendering into it. The
// error names the field, so the fix is obvious.
func renderScalar(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case bool:
		return strconv.FormatBool(t), nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), nil
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			s, err := renderScalar(e)
			if err != nil {
				return "", err
			}
			parts = append(parts, s)
		}
		return strings.Join(parts, ", "), nil
	case map[string]any:
		return "", fmt.Errorf("nested tables are not supported in a self-model file")
	default:
		return fmt.Sprintf("%v", v), nil
	}
}
