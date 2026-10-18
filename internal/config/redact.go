package config

import (
	"bytes"
	"regexp"

	"github.com/BurntSushi/toml"
)

// Redacted marks a secret's place in RedactedTOML: the setting is there, its
// value is not shown.
const Redacted = "(set, hidden)"

// secretKey names a setting whose value is a credential.
var secretKey = regexp.MustCompile(`(^|_)(api_key|key|token|secret|password)$`)

// RedactedTOML is the configuration the daemon runs with — the file, then the
// NINE_* overrides — as TOML: only what is set, since an unset setting (an
// empty string, a zero, false, an empty list) takes its default, and with
// every credential replaced by Redacted: a key, token, secret or password,
// and each value of an MCP server's env and headers, which carry tokens.
func (cfg *Config) RedactedTOML() (string, error) {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
		return "", err
	}
	var tree map[string]any
	if _, err := toml.Decode(buf.String(), &tree); err != nil {
		return "", err
	}
	redact(tree)
	prune(tree)
	buf.Reset()
	enc := toml.NewEncoder(&buf)
	enc.Indent = ""
	if err := enc.Encode(tree); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func redact(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			switch inner := val.(type) {
			case string:
				if inner != "" && secretKey.MatchString(k) && k != "key_path" {
					t[k] = Redacted
				}
			case map[string]any:
				if k == "env" || k == "headers" {
					for name, s := range inner {
						if s, ok := s.(string); ok && s != "" {
							inner[name] = Redacted
						}
					}
					continue
				}
				redact(inner)
			default:
				redact(val)
			}
		}
	case []map[string]any:
		for _, m := range t {
			redact(m)
		}
	case []any:
		for _, e := range t {
			redact(e)
		}
	}
}

// prune drops what is unset from a decoded TOML tree, and a table left empty.
func prune(m map[string]any) {
	for k, v := range m {
		switch t := v.(type) {
		case map[string]any:
			prune(t)
			if len(t) == 0 {
				delete(m, k)
			}
		case []map[string]any:
			for _, e := range t {
				prune(e)
			}
			if len(t) == 0 {
				delete(m, k)
			}
		case []any:
			if len(t) == 0 {
				delete(m, k)
			}
		case string:
			if t == "" {
				delete(m, k)
			}
		case bool:
			if !t {
				delete(m, k)
			}
		case int64:
			if t == 0 {
				delete(m, k)
			}
		case float64:
			if t == 0 {
				delete(m, k)
			}
		}
	}
}
