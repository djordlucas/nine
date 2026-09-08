package mistral

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nine/internal/llm"
)

func TestProvider_Complete(t *testing.T) {
	// Create a test server that mimics Mistral's API
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify auth header
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer test-api-key") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		// Verify content type
		if r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "bad content type", http.StatusBadRequest)
			return
		}

		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Return a mock streaming response
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		// Simple response with content
		response := `data: {"choices":[{"delta":{"content":"Hello from Mistral"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}

`
		w.Write([]byte(response))
	}))
	defer server.Close()

	provider := New("mistral-tiny", server.URL, "test-api-key", 30)

	req := llm.Request{
		System:  "You are a helpful assistant",
		Messages: []llm.Message{{Role: "user", Text: "Hello"}},
		Tools:    nil,
		MaxTokens: 100,
	}

	resp, err := provider.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}

	if resp.Text != "Hello from Mistral" {
		t.Errorf("Expected 'Hello from Mistral', got %q", resp.Text)
	}

	if resp.StopReason != "end_turn" {
		t.Errorf("Expected stop reason 'end_turn', got %q", resp.StopReason)
	}

	if resp.Usage.InputTokens != 10 {
		t.Errorf("Expected 10 input tokens, got %d", resp.Usage.InputTokens)
	}

	if resp.Usage.OutputTokens != 5 {
		t.Errorf("Expected 5 output tokens, got %d", resp.Usage.OutputTokens)
	}
}

func TestProvider_Complete_WithTools(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer test-key") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Verify tools were sent
		if len(req.Tools) == 0 {
			http.Error(w, "expected tools", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		// Response with tool call
		response := `data: {"choices":[{"delta":{"content":""},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":8,"total_tokens":28}}

`
		w.Write([]byte(response))
	}))
	defer server.Close()

	provider := New("mistral-small", server.URL, "test-key", 30)

	tools := []llm.ToolDef{
		{Name: "get_weather", Description: "Get current weather", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}

	req := llm.Request{
		Messages: []llm.Message{{Role: "user", Text: "What's the weather?"}},
		Tools:    tools,
	}

	_, err := provider.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete with tools failed: %v", err)
	}
}

func TestProvider_Complete_AuthError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer valid-key") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"message":"Invalid API key","type":"invalid_request_error"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	provider := New("mistral-tiny", server.URL, "invalid-key", 30)

	req := llm.Request{
		Messages: []llm.Message{{Role: "user", Text: "Hello"}},
	}

	_, err := provider.Complete(context.Background(), req)
	if err == nil {
		t.Fatal("Expected error for invalid API key")
	}

	if !strings.Contains(err.Error(), "unauthorized") && !strings.Contains(err.Error(), "Invalid API key") {
		t.Errorf("Expected auth error, got: %v", err)
	}
}

func TestProvider_Complete_ToolResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`data: {"choices":[{"delta":{"content":"result"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}

`))
	}))
	defer server.Close()

	provider := New("mistral-tiny", server.URL, "test-key", 30)

	// Message with tool results
	toolResults := []llm.ToolResult{
		{ToolCallID: "call_1", Content: "25°C", IsError: false},
	}

	req := llm.Request{
		Messages: []llm.Message{
			{Role: "user", Text: "Get weather"},
			{Role: "assistant", Text: "", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "get_weather", Input: json.RawMessage(`{}`)}}},
			{Role: "user", Text: "", ToolResults: toolResults},
		},
	}

	_, err := provider.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete with tool results failed: %v", err)
	}
}

func TestProvider_Complete_StreamingToolCalls(t *testing.T) {
	// Test that tool calls streamed as partial deltas are correctly accumulated
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		
		// Simulate Mistral streaming tool calls as partial deltas
		w.Write([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function"}]}}]}

`))
		w.Write([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"get_weather"}}]}}]}

`))
		w.Write([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{"}}]}}]}

`))
		w.Write([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"city\":"}}]}}]}

`))
		w.Write([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\""}}]}}]}

`))
		w.Write([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"}"}}]}}]}

`))
		w.Write([]byte(`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}

`))
		w.Write([]byte(`data: [DONE]

`))
	}))
	defer server.Close()

	provider := New("mistral-tiny", server.URL, "test-key", 30)

	tools := []llm.ToolDef{
		{Name: "get_weather", Description: "Get current weather", InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)},
	}

	req := llm.Request{
		Messages: []llm.Message{{Role: "user", Text: "What's the weather in Paris?"}},
		Tools:    tools,
	}

	resp, err := provider.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete with streaming tool calls failed: %v", err)
	}

	if len(resp.ToolCalls) != 1 {
		t.Fatalf("Expected 1 tool call, got %d", len(resp.ToolCalls))
	}

	tc := resp.ToolCalls[0]
	if tc.ID != "call_1" {
		t.Errorf("Expected tool call ID 'call_1', got %q", tc.ID)
	}

	if tc.Name != "get_weather" {
		t.Errorf("Expected tool call name 'get_weather', got %q", tc.Name)
	}

	expectedArgs := `{"city":"Paris"}`
	if string(tc.Input) != expectedArgs {
		t.Errorf("Expected arguments %q, got %q", expectedArgs, string(tc.Input))
	}

	if resp.StopReason != "tool_use" {
		t.Errorf("Expected stop reason 'tool_use', got %q", resp.StopReason)
	}
}

func TestProvider_New(t *testing.T) {
	tests := []struct {
		name       string
		model      string
		endpoint  string
		apiKey     string
		timeout   int
		wantModel  string
		wantEndpoint string
	}{
		{
			name:        "default endpoint",
			model:       "mistral-tiny",
			endpoint:   "",
			apiKey:      "key123",
			timeout:    0,
			wantModel:   "mistral-tiny",
			wantEndpoint: "https://api.mistral.ai/v1",
		},
		{
			name:        "custom endpoint",
			model:       "mistral-small",
			endpoint:   "https://custom.mistral.ai/v1",
			apiKey:      "custom-key",
			timeout:    60,
			wantModel:   "mistral-small",
			wantEndpoint: "https://custom.mistral.ai/v1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := New(tt.model, tt.endpoint, tt.apiKey, tt.timeout)
			if p.model != tt.wantModel {
				t.Errorf("model: got %q, want %q", p.model, tt.wantModel)
			}
			if p.endpoint != tt.wantEndpoint {
				t.Errorf("endpoint: got %q, want %q", p.endpoint, tt.wantEndpoint)
			}
			if p.apiKey != tt.apiKey {
				t.Errorf("apiKey: got %q, want %q", p.apiKey, tt.apiKey)
			}
		})
	}
}

func TestBuildMessages_EmptyToolCallArgs(t *testing.T) {
	// When a tool call has nil/empty Input, the arguments field must still be
	// present in the wire payload — Mistral returns 422 if it's omitted.
	req := llm.Request{
		Messages: []llm.Message{
			{Role: "assistant", ToolCalls: []llm.ToolCall{
				{ID: "call_1", Name: "parameterless_tool", Input: nil},
			}},
		},
	}
	msgs := buildMessages(req)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	raw, _ := json.Marshal(msgs[0])
	s := string(raw)
	if !strings.Contains(s, `"arguments"`) {
		t.Fatalf("arguments field missing from tool call — Mistral would return 422\n%s", s)
	}
}

func TestComplete_ToolDefNilParameters(t *testing.T) {
	// A tool with no InputSchema must still send a parameters field.
	var captured chatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}

`))
	}))
	defer server.Close()

	provider := New("mistral-tiny", server.URL, "test-key", 30)
	_, err := provider.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: "user", Text: "hi"}},
		Tools:    []llm.ToolDef{{Name: "noop", Description: "no args", InputSchema: nil}},
	})
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if len(captured.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(captured.Tools))
	}
	raw, _ := json.Marshal(captured.Tools[0])
	s := string(raw)
	if !strings.Contains(s, `"parameters"`) {
		t.Fatalf("parameters field missing from tool def — Mistral would return 422\n%s", s)
	}
}

func TestMapFinishReason(t *testing.T) {
	tests := []struct {
		reason  string
		calls   []llm.ToolCall
		want    string
	}{
		{"stop", nil, "end_turn"},
		{"length", nil, "max_tokens"},
		{"tool_calls", nil, "tool_use"},
		{"stop", []llm.ToolCall{{ID: "1", Name: "test", Input: []byte("{}")}}, "tool_use"},
		{"unknown", nil, "end_turn"},
	}

	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			got := mapFinishReason(tt.reason, tt.calls)
			if got != tt.want {
				t.Errorf("mapFinishReason(%q, %v) = %q, want %q", tt.reason, tt.calls, got, tt.want)
			}
		})
	}
}
