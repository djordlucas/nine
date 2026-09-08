package llm

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestSanitizeRawMessage(t *testing.T) {
	tests := []struct {
		name     string
		input    json.RawMessage
		expected json.RawMessage
	}{
		{"valid JSON object", json.RawMessage(`{"key":"value"}`), json.RawMessage(`{"key":"value"}`)},
		{"valid JSON array", json.RawMessage(`[1,2,3]`), json.RawMessage(`[1,2,3]`)},
		{"valid JSON string", json.RawMessage(`"hello"`), json.RawMessage(`"hello"`)},
		{"valid JSON number", json.RawMessage(`42`), json.RawMessage(`42`)},
		{"valid JSON boolean", json.RawMessage(`true`), json.RawMessage(`true`)},
		{"valid JSON null", json.RawMessage(`null`), json.RawMessage(`null`)},
		{"valid empty object", json.RawMessage(`{}`), json.RawMessage(`{}`)},
		{"valid empty array", json.RawMessage(`[]`), json.RawMessage(`[]`)},
		{"empty slice", json.RawMessage(""), nil},
		{"nil", nil, nil},
		{"partial JSON - missing closing brace", json.RawMessage(`{"key":`), nil},
		{"partial JSON - missing value", json.RawMessage(`{"key":"value"`), nil},
		{"malformed JSON - invalid token", json.RawMessage(`{malformed`), nil},
		{"malformed JSON - unexpected end", json.RawMessage(`{"a":"b",`), nil},
		{"invalid JSON - bare word", json.RawMessage(`hello`), nil},
		{"whitespace only", json.RawMessage(`   `), nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeRawMessage(tt.input)
			if !bytes.Equal(got, tt.expected) {
				t.Errorf("SanitizeRawMessage() = %q, want %q", string(got), string(tt.expected))
			}
		})
	}
}

func TestIsValidJSON(t *testing.T) {
	tests := []struct {
		name    string
		input   []byte
		wantErr bool
	}{
		{"valid JSON", []byte(`{"key":"value"}`), false},
		{"empty", []byte(""), true},
		{"partial JSON", []byte(`{"key":`), true},
		{"malformed JSON", []byte(`{malformed`), true},
		{"valid empty object", []byte(`{}`), false},
		{"valid array", []byte(`[]`), false},
		{"valid string", []byte(`"hello"`), false},
		{"valid number", []byte(`42`), false},
		{"valid boolean", []byte(`true`), false},
		{"valid null", []byte(`null`), false},
		{"invalid - bare word", []byte(`hello`), true},
		{"whitespace", []byte(`   `), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := IsValidJSON(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("IsValidJSON() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSanitizeRawMessageMarshalability(t *testing.T) {
	// Test that sanitized RawMessage values can be successfully marshaled
	tests := []struct {
		name string
		input json.RawMessage
	}{
		{"valid JSON", json.RawMessage(`{"key":"value"}`)},
		{"empty", json.RawMessage("")},
		{"partial", json.RawMessage(`{"partial":`)},
		{"malformed", json.RawMessage(`{malformed`)},
		{"nil", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sanitized := SanitizeRawMessage(tt.input)
			
			// Create a struct with the sanitized value
			payload := struct {
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input,omitempty"`
			}{
				Name:  "test_tool",
				Input: sanitized,
			}
			
			// This should never fail
			data, err := json.Marshal(payload)
			if err != nil {
				t.Errorf("json.Marshal() failed for sanitized input %q: %v", string(tt.input), err)
			}
			_ = data // successfully marshaled
		})
	}
}
