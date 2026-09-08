// Package llm defines the provider-agnostic LLM interface and request queue.
package llm

import (
	"encoding/json"
)

// IsValidJSON checks if a byte slice contains valid JSON.
// It returns nil if valid, or an error describing the syntax problem.
func IsValidJSON(b []byte) error {
	if len(b) == 0 {
		return &json.SyntaxError{}
	}
	var v any
	return json.Unmarshal(b, &v)
}

// SanitizeRawMessage returns a valid json.RawMessage, converting invalid JSON
// to nil so it can be safely marshaled later. Empty slices are also converted to nil
// to avoid "unexpected end of JSON input" errors during marshaling.
func SanitizeRawMessage(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	if err := IsValidJSON(raw); err != nil {
		return nil
	}
	return raw
}
