package ollama_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nine/internal/llm"
	"nine/internal/llm/ollama"
)

// ndjsonServer returns an httptest server that answers /api/chat by writing the
// given lines as newline-delimited JSON (the Ollama streaming format), and
// captures the raw request body it received.
// an optional request counter can be passed in to assert on the number of requests made to the server.
func ndjsonServer(t *testing.T, gotBody *[]byte, status int, lines ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"capabilities":["completion","tools","thinking"]}`))
			return
		}

		if r.URL.Path != "/api/chat" {
			t.Errorf("unexpected path %q, want /api/chat", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		*gotBody = b
		w.Header().Set("Content-Type", "application/x-ndjson")
		if status != 0 {
			w.WriteHeader(status)
		}
		for _, ln := range lines {
			_, _ = w.Write([]byte(ln + "\n"))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// showServer returns an httptest server that answers /api/show with the given capabilities JSON, and counts the number of requests made to it.
func showServer(t *testing.T, counter *int, capsJSON string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/show" {
			t.Errorf("unexpected path %q, want /api/show", r.URL.Path)
		}
		if counter != nil {
			*counter++
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(capsJSON))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// decodedRequest is the subset of the Ollama /api/chat request body the tests
// assert on.
type decodedRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Think    *bool  `json:"think"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Type     string `json:"type"`
		Function struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"function"`
	} `json:"tools"`
	Options *struct {
		NumCtx int `json:"num_ctx"`
	} `json:"options"`
}

func TestOllamaCompleteStreaming(t *testing.T) {
	var body []byte
	srv := ndjsonServer(t, &body, 0,
		`{"message":{"role":"assistant","content":"Hello"},"done":false}`,
		`{"message":{"role":"assistant","content":", world"},"done":false}`,
		`{"message":{"role":"assistant","content":""},"done":true,"done_reason":"stop"}`,
	)

	p := ollama.New("qwen3", srv.URL, 0, false, 0)
	var chunks []string
	resp, err := p.Complete(context.Background(), llm.Request{
		System:   "be brief",
		Messages: []llm.Message{{Role: "user", Text: "hi"}},
		OnChunk:  func(s string) { chunks = append(chunks, s) },
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Streamed content is accumulated and mirrored to OnChunk in order.
	if resp.Text != "Hello, world" {
		t.Errorf("Text = %q, want \"Hello, world\"", resp.Text)
	}
	if strings.Join(chunks, "") != "Hello, world" || len(chunks) != 2 {
		t.Errorf("OnChunk = %v, want two chunks joining to the full text", chunks)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want end_turn", resp.StopReason)
	}

	// Request shape: model, streaming, system message, user message, no /no_think (thinking disabled).
	var req decodedRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if req.Model != "qwen3" || !req.Stream {
		t.Errorf("request model/stream = %q/%v", req.Model, req.Stream)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" {
		t.Fatalf("messages = %+v, want [system, user]", req.Messages)
	}
	if req.Messages[0].Content != "be brief" {
		t.Errorf("system content = %q", req.Messages[0].Content)
	}
	if req.Messages[1].Content != "hi" {
		t.Errorf("user content = %q, want plain \"hi\" (no /no_think)", req.Messages[1].Content)
	}
	// detect if the request has an explicit false for think, which is required for a capable model with thinking off
	if req.Think != nil && *req.Think {
		t.Errorf("think = %v, want explicit false for a capable model with thinking off", req.Think)
	}
}

func TestOllamaThinkingEnabled(t *testing.T) {
	var body []byte
	srv := ndjsonServer(t, &body, 0,
		`{"message":{"role":"assistant","thinking":"Let me "},"done":false}`,
		`{"message":{"role":"assistant","thinking":"consider..."},"done":false}`,
		`{"message":{"role":"assistant","content":"Answer"},"done":false}`,
		`{"message":{"role":"assistant","content":""},"done":true,"done_reason":"stop"}`,
	)

	p := ollama.New("qwen3", srv.URL, 0, true, 0)
	var thinking, chunks []string
	resp, err := p.Complete(context.Background(), llm.Request{
		Messages:        []llm.Message{{Role: "user", Text: "hi"}},
		OnChunk:         func(s string) { chunks = append(chunks, s) },
		OnThinkingChunk: func(s string) { thinking = append(thinking, s) },
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Reasoning tokens go to OnThinkingChunk only; the reply text excludes them.
	if strings.Join(thinking, "") != "Let me consider..." {
		t.Errorf("OnThinkingChunk = %v, want the reasoning stream", thinking)
	}
	if resp.Text != "Answer" {
		t.Errorf("Text = %q, want just the reply (no thinking)", resp.Text)
	}
	if strings.Join(chunks, "") != "Answer" {
		t.Errorf("OnChunk = %v, want just the reply text", chunks)
	}

	// Request enables thinking and does NOT prepend /no_think.
	var req decodedRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if req.Think == nil || !*req.Think {
		t.Errorf("think = %v, want true", req.Think)
	}
	if len(req.Messages) != 1 || strings.Contains(req.Messages[0].Content, "/no_think") {
		t.Errorf("user content = %q, want no /no_think when thinking enabled", req.Messages[0].Content)
	}
}

func TestOllamaToolCall(t *testing.T) {
	var body []byte
	srv := ndjsonServer(t, &body, 0,
		`{"message":{"role":"assistant","tool_calls":[{"id":"c1","function":{"name":"get_weather","arguments":{"city":"Paris"}}}]},"done":false}`,
		`{"message":{"role":"assistant","content":""},"done":true,"done_reason":"tool_calls"}`,
	)

	p := ollama.New("qwen3", srv.URL, 0, false, 0)
	resp, err := p.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: "user", Text: "weather in Paris?"}},
		Tools: []llm.ToolDef{{
			Name:        "get_weather",
			Description: "Get weather for a <city>.", // angle brackets must be escaped in the wire tool def
			InputSchema: []byte(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %+v, want 1", resp.ToolCalls)
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "c1" || tc.Name != "get_weather" {
		t.Errorf("tool call = %+v, want id=c1 name=get_weather", tc)
	}
	if !strings.Contains(string(tc.Input), "Paris") {
		t.Errorf("tool call input = %s, want it to carry the arguments", tc.Input)
	}
	if resp.StopReason != "tool_use" {
		t.Errorf("StopReason = %q, want tool_use", resp.StopReason)
	}

	// The tool definition is forwarded, with angle brackets XML-escaped.
	var req decodedRequest
	_ = json.Unmarshal(body, &req)
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "get_weather" {
		t.Fatalf("tools = %+v, want get_weather", req.Tools)
	}
	if strings.ContainsAny(req.Tools[0].Function.Description, "<>") {
		t.Errorf("tool description not XML-escaped: %q", req.Tools[0].Function.Description)
	}
}

func TestOllamaStopReasonLength(t *testing.T) {
	var body []byte
	srv := ndjsonServer(t, &body, 0,
		`{"message":{"content":"cut off"},"done":true,"done_reason":"length"}`,
	)
	p := ollama.New("qwen3", srv.URL, 0, false, 0)
	resp, err := p.Complete(context.Background(), llm.Request{Messages: []llm.Message{{Role: "user", Text: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != "max_tokens" {
		t.Errorf("StopReason = %q, want max_tokens", resp.StopReason)
	}
}

func TestOllamaNumCtxOption(t *testing.T) {
	var body []byte
	srv := ndjsonServer(t, &body, 0, `{"message":{"content":"ok"},"done":true}`)
	p := ollama.New("qwen3", srv.URL, 4096, false, 0)
	if _, err := p.Complete(context.Background(), llm.Request{Messages: []llm.Message{{Role: "user", Text: "x"}}}); err != nil {
		t.Fatal(err)
	}
	var req decodedRequest
	_ = json.Unmarshal(body, &req)
	if req.Options == nil || req.Options.NumCtx != 4096 {
		t.Errorf("options = %+v, want num_ctx=4096", req.Options)
	}
}

func TestOllamaHTTPErrorStatus(t *testing.T) {
	var body []byte
	srv := ndjsonServer(t, &body, http.StatusInternalServerError, `{"error":"model not found"}`)
	p := ollama.New("qwen3", srv.URL, 0, false, 0)
	_, err := p.Complete(context.Background(), llm.Request{Messages: []llm.Message{{Role: "user", Text: "x"}}})
	if err == nil || !strings.Contains(err.Error(), "model not found") {
		t.Errorf("err = %v, want it to surface the ollama error body", err)
	}
}

func TestOllamaErrorInStream(t *testing.T) {
	var body []byte
	srv := ndjsonServer(t, &body, 0, `{"error":"context canceled"}`)
	p := ollama.New("qwen3", srv.URL, 0, false, 0)
	_, err := p.Complete(context.Background(), llm.Request{Messages: []llm.Message{{Role: "user", Text: "x"}}})
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("err = %v, want the in-stream error surfaced", err)
	}
}

// The adapter satisfies llm.Provider.
func TestOllamaImplementsProvider(t *testing.T) {
	var _ llm.Provider = ollama.New("m", "", 0, false, 0)
}

// [llm].timeout_seconds bounds a single call: a model that stops answering must
// fail the turn rather than hang the agent loop forever.
func TestOllamaHonorsTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("test server response writer does not flush")
			return
		}
		_, _ = w.Write([]byte(`{"message":{"role":"assistant","content":"partial"},"done":false}` + "\n"))
		flusher.Flush()
		// Never send the terminating done chunk: the client must give up.
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	p := ollama.New("qwen3", srv.URL, 0, false, 1)
	start := time.Now()
	_, err := p.Complete(context.Background(), llm.Request{Messages: []llm.Message{{Role: "user", Text: "x"}}})
	if err == nil {
		t.Fatal("want a timeout error, got nil")
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("took %s, want the 1s client timeout to cut it off", elapsed)
	}
}

func TestOllamaSupportThinking(t *testing.T) {
	var counter int

	// The test server responds to /api/show with a capabilities list that includes "thinking".
	thinkSrv := showServer(t, &counter, `{"capabilities":["completion","thinking"]}`)
	p1 := ollama.New("qwen3", thinkSrv.URL, 0, true, 0)

	if !p1.SupportsThinking(context.Background()) {
		t.Errorf("SupportsThinking = false, want true for a model with thinking capability")
	}

	// The second call should hit the cached result and not make a new request.
	_ = p1.SupportsThinking(context.Background())
	if counter != 1 {
		t.Errorf("SupportsThinking = false on second call, want true (cached)")
	}

	// The test server responds to /api/show with a capabilities list that does not include "thinking".
	noThinkSrv := showServer(t, nil, `{"capabilities":["completion"]}`)
	p2 := ollama.New("qwen3", noThinkSrv.URL, 0, false, 0)

	if p2.SupportsThinking(context.Background()) {
		t.Errorf("SupportsThinking = true, want false for a model without thinking capability")
	}

	// Inability to reach the server should be treated as "thinking not supported" (false).
	p3 := ollama.New("qwen3", "invalid!", 0, true, 0)
	if p3.SupportsThinking(context.Background()) {
		t.Errorf("SupportsThinking = true, want false when the provider URL cannot be reached")
	}
}

// ptr returns a pointer to the given value.
func ptr[T any](v T) *T { return &v }

func TestOllamaPerRequestThinkOverride(t *testing.T) {
	var body []byte
	srv := ndjsonServer(t, &body, 0,
		`{"message":{"role":"assistant","tool_calls":[{"id":"c1","function":{"name":"get_weather","arguments":{"city":"Montreal"}}}]},"done":false}`,
		`{"message":{"role":"assistant","content":""},"done":true,"done_reason":"tool_calls"}`,
	) // The provider is configured with thinking disabled.

	p := ollama.New("qwen3", srv.URL, 0, false, 0)
	resp, err := p.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: "user", Text: "weather in Montreal?"}},
		Think:    ptr(true), // per-request override to enable thinking
		Tools: []llm.ToolDef{{
			Name:        "get_weather",
			Description: "Get weather for a <city>.", // angle brackets must be escaped in the wire tool def
			InputSchema: []byte(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if resp.ThinkingUsed != true {
		t.Errorf("ThinkingUsed = %v, want true for a per-request override to enable thinking", resp.ThinkingUsed)
	}

	var decoded decodedRequest
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Think == nil || !*decoded.Think {
		t.Errorf("wire think = %v, want true", decoded.Think)
	}

	// Test that a per-request override to disable thinking works even when the provider has thinking enabled.
	p2 := ollama.New("qwen3", srv.URL, 0, true, 0)
	resp2, err := p2.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: "user", Text: "weather in Montreal?"}},
		Think:    ptr(false), // per-request override to disable thinking
		Tools: []llm.ToolDef{{
			Name:        "get_weather",
			Description: "Get weather for a <city>.", // angle brackets must be escaped in the wire tool def
			InputSchema: []byte(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if resp2.ThinkingUsed != false {
		t.Errorf("ThinkingUsed = %v, want false for a per-request override to disable thinking", resp2.ThinkingUsed)
	}

	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Think == nil || *decoded.Think {
		t.Errorf("wire think = %v, want false", decoded.Think)
	}
}
