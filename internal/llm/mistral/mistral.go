// Package mistral implements llm.Provider for Mistral's OpenAI-compatible API.
package mistral

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"nine/internal/llm"
)

const defaultEndpoint = "https://api.mistral.ai/v1"

// Provider calls the Mistral chat API.
type Provider struct {
	model    string
	endpoint string
	apiKey   string
	client   *http.Client
	timeout  time.Duration
}

// New creates a Mistral provider.
// apiKey is required; endpoint defaults to Mistral's API if empty.
// timeoutSecs is the HTTP client timeout in seconds; 0 uses 120s default,
// negative disables timeout entirely.
func New(model, endpoint, apiKey string, timeoutSecs int) *Provider {
	timeout := 120 * time.Second
	switch {
	case timeoutSecs > 0:
		timeout = time.Duration(timeoutSecs) * time.Second
	case timeoutSecs < 0:
		timeout = 0
	}
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	return &Provider{
		model:    model,
		endpoint: endpoint,
		apiKey:   apiKey,
		client:   &http.Client{Timeout: timeout},
		timeout:  timeout,
	}
}

// Mistral wire types (OpenAI-compatible)

type chatRequest struct {
	Model    string     `json:"model"`
	Messages []message  `json:"messages"`
	Tools    []toolDef  `json:"tools,omitempty"`
	Stream   bool       `json:"stream"`
	MaxTokens int        `json:"max_tokens,omitempty"`
}

type message struct {
	Role      string     `json:"role"` // user | assistant | system | tool
	Content   string     `json:"content,omitempty"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
}

type toolCall struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"` // "function"
	Function functionCall   `json:"function"`
}

type functionCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type toolDef struct {
	Type     string      `json:"type"` // "function"
	Function toolFuncDef `json:"function"`
}

type toolFuncDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Stream chunk
type streamChunk struct {
	Choices []choice `json:"choices"`
	Usage   *usage   `json:"usage,omitempty"`
}

type choice struct {
	Delta       delta   `json:"delta"`
	FinishReason string `json:"finish_reason,omitempty"`
	Index        int    `json:"index,omitempty"`
}

type delta struct {
	Role      string   `json:"role,omitempty"`
	Content   string   `json:"content,omitempty"`
	ToolCalls []tDelta `json:"tool_calls,omitempty"`
}

type tDelta struct {
	Index    int     `json:"index"`
	ID       string  `json:"id,omitempty"`
	Type     string  `json:"type,omitempty"`
	Function fDelta  `json:"function,omitempty"`
}

type fDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// errorResponse is the Mistral error envelope.
type errorResponse struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Param   string `json:"param,omitempty"`
		Code    string `json:"code,omitempty"`
	} `json:"error"`
}

// Complete implements llm.Provider.
func (p *Provider) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	messages := buildMessages(req)

	tools := make([]toolDef, 0, len(req.Tools))
	for _, t := range req.Tools {
		tools = append(tools, toolDef{
			Type: "function",
			Function: toolFuncDef{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}

	body := chatRequest{
		Model:    p.model,
		Messages: messages,
		Tools:    tools,
		Stream:   true,
	}

	if req.MaxTokens > 0 {
		body.MaxTokens = req.MaxTokens
	}

	data, err := json.Marshal(body)
	if err != nil {
		return llm.Response{}, err
	}

	url := p.endpoint + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return llm.Response{}, err
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	httpResp, err := p.client.Do(httpReq)
	if err != nil {
		return llm.Response{}, err
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		var errBody errorResponse
		body, _ := io.ReadAll(httpResp.Body)
		if err := json.Unmarshal(body, &errBody); err == nil && errBody.Error.Message != "" {
			return llm.Response{}, fmt.Errorf("mistral: %s", errBody.Error.Message)
		}
		return llm.Response{}, fmt.Errorf("mistral: HTTP %d", httpResp.StatusCode)
	}

	var (
		content    strings.Builder
		toolCalls  []llm.ToolCall
		doneReason string
		usage      llm.Usage
	)

	scanner := bufio.NewScanner(httpResp.Body)
	for scanner.Scan() {
		line := scanner.Bytes()
		// Mistral sends SSE: strip "data: " prefix
		if bytes.HasPrefix(line, []byte("data: ")) {
			line = line[6:]
		}
		// Skip empty lines and [DONE] markers
		if len(line) == 0 || bytes.Equal(line, []byte("[DONE]")) {
			continue
		}

		var chunk streamChunk
		if err := json.Unmarshal(line, &chunk); err != nil {
			continue
		}

		for _, c := range chunk.Choices {
			// Handle content deltas
			if c.Delta.Content != "" {
				content.WriteString(c.Delta.Content)
				if req.OnChunk != nil {
					req.OnChunk(c.Delta.Content)
				}
			}

			// Reconstruct tool calls from deltas
			// Mistral streams tool calls as partial deltas; we accumulate them
			for _, tc := range c.Delta.ToolCalls {
				if tc.Function.Name != "" {
					toolCalls = append(toolCalls, llm.ToolCall{
						ID:    tc.ID,
						Name:  tc.Function.Name,
						Input: json.RawMessage(tc.Function.Arguments),
					})
				}
			}

			if c.FinishReason != "" {
				doneReason = c.FinishReason
			}

			// Usage is only on the final chunk
			if chunk.Usage != nil {
				usage = llm.Usage{
					InputTokens:  chunk.Usage.PromptTokens,
					OutputTokens: chunk.Usage.CompletionTokens,
				}
			}
		}
	}

	if err := scanner.Err(); err != nil && err != io.EOF {
		return llm.Response{}, fmt.Errorf("stream read: %w", err)
	}

	resp := llm.Response{
		Text:       content.String(),
		ToolCalls:  toolCalls,
		Usage:      usage,
		StopReason: mapFinishReason(doneReason, toolCalls),
	}

	return resp, nil
}

// buildMessages converts llm.Message slice to Mistral message format.
func buildMessages(req llm.Request) []message {
	var msgs []message

	if req.System != "" {
		msgs = append(msgs, message{Role: "system", Content: req.System})
	}

	for _, m := range req.Messages {
		// Tool results become "tool" role messages
		if len(m.ToolResults) > 0 {
			for _, tr := range m.ToolResults {
				content := tr.Content
				if tr.IsError {
					content = "error: " + content
				}
				msgs = append(msgs, message{
					Role:    "tool",
					Content: content,
				})
			}
			continue
		}

		msg := message{
			Role:    m.Role,
			Content: m.Text,
		}

		// Add tool calls to the message
		if len(m.ToolCalls) > 0 {
			msg.ToolCalls = make([]toolCall, len(m.ToolCalls))
			for i, tc := range m.ToolCalls {
				msg.ToolCalls[i] = toolCall{
					ID:   tc.ID,
					Type: "function",
					Function: functionCall{
						Name:      tc.Name,
						Arguments: tc.Input,
					},
				}
			}
		}

		msgs = append(msgs, msg)
	}

	return msgs
}

// mapFinishReason converts Mistral finish reasons to Nine's stop reasons.
func mapFinishReason(reason string, calls []llm.ToolCall) string {
	if len(calls) > 0 {
		return "tool_use"
	}
	switch reason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	default:
		return "end_turn"
	}
}
