package anthropic_test

import (
	"context"
	"os"
	"testing"

	"nine/internal/llm"
	"nine/internal/llm/anthropic"
)

func TestAnthropicIntegration(t *testing.T) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		t.Skip("ANTHROPIC_API_KEY not set")
	}

	p := anthropic.New(apiKey, "claude-haiku-4-5-20251001", "", 0)
	resp, err := p.Complete(context.Background(), llm.Request{
		Messages:  []llm.Message{{Role: "user", Text: "say hello"}},
		MaxTokens: 64,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text == "" {
		t.Error("response text is empty")
	}
	if resp.StopReason == "" {
		t.Error("stop_reason is empty")
	}
	t.Logf("response: %q  stop_reason: %s", resp.Text, resp.StopReason)
}

func TestAnthropicToolCall(t *testing.T) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		t.Skip("ANTHROPIC_API_KEY not set")
	}

	tool := llm.ToolDef{
		Name:        "get_weather",
		Description: "Get the current weather for a city.",
		InputSchema: []byte(`{"type":"object","required":["city"],"properties":{"city":{"type":"string"}}}`),
	}

	p := anthropic.New(apiKey, "claude-haiku-4-5-20251001", "", 0)
	resp, err := p.Complete(context.Background(), llm.Request{
		Messages:  []llm.Message{{Role: "user", Text: "What is the weather in Paris?"}},
		Tools:     []llm.ToolDef{tool},
		MaxTokens: 256,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.ToolCalls) == 0 {
		t.Errorf("expected at least one tool call, got none (text: %q)", resp.Text)
	} else {
		tc := resp.ToolCalls[0]
		if tc.Name != "get_weather" {
			t.Errorf("tool name = %q, want get_weather", tc.Name)
		}
		t.Logf("tool call: %s %s", tc.Name, tc.Input)
	}
}
