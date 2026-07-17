// Package anthropic implements the llm.Provider interface for the Anthropic
// Messages API with prompt-caching support.
package anthropic

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

const (
	defaultEndpoint = "https://api.anthropic.com/v1/messages"
	apiVersion      = "2023-06-01"
)

// Provider calls the Anthropic Messages API.
type Provider struct {
	apiKey   string
	model    string
	endpoint string
	client   *http.Client
}

// New creates a Provider. Pass empty endpoint to use the default API URL.
// timeoutSecs is the HTTP client timeout in seconds; 0 means no timeout.
func New(apiKey, model, endpoint string, timeoutSecs int) *Provider {
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	var httpTimeout time.Duration
	if timeoutSecs > 0 {
		httpTimeout = time.Duration(timeoutSecs) * time.Second
	}
	return &Provider{
		apiKey:   apiKey,
		model:    model,
		endpoint: endpoint,
		client:   &http.Client{Timeout: httpTimeout},
	}
}

// --- Anthropic API wire types ---

type apiRequest struct {
	Model     string       `json:"model"`
	MaxTokens int          `json:"max_tokens"`
	System    []sysPart    `json:"system,omitempty"`
	Messages  []apiMessage `json:"messages"`
	Tools     []apiTool    `json:"tools,omitempty"`
	Stream    bool         `json:"stream,omitempty"`
}

// sseBlock accumulates content for one block in the streaming response.
type sseBlock struct {
	typ     string // "text" | "tool_use"
	id      string
	name    string
	jsonBuf strings.Builder
}

type sysPart struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

type cacheControl struct {
	Type string `json:"type"` // "ephemeral"
}

// apiMessage is serialised differently per role, so we marshal it manually.
type apiMessage struct {
	Role    string      `json:"role"`
	Content any `json:"content"` // string or []apiContentBlock
}

type apiContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   any             `json:"content,omitempty"` // tool_result: string or blocks
	IsError   bool            `json:"is_error,omitempty"`
}

type apiTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type apiError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// Complete sends a streaming request to the Anthropic Messages API and
// accumulates the result. If req.OnChunk is set, it is called with each
// text token as it arrives.
func (p *Provider) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	maxToks := req.MaxTokens
	if maxToks <= 0 {
		maxToks = 1024
	}

	body := apiRequest{
		Model:     p.model,
		MaxTokens: maxToks,
		Stream:    true,
		Messages:  make([]apiMessage, len(req.Messages)),
	}

	if req.System != "" {
		body.System = []sysPart{{
			Type:         "text",
			Text:         req.System,
			CacheControl: &cacheControl{Type: "ephemeral"},
		}}
	}

	for i, m := range req.Messages {
		body.Messages[i] = marshalMessage(m)
	}

	if len(req.Tools) > 0 {
		body.Tools = make([]apiTool, len(req.Tools))
		for i, t := range req.Tools {
			body.Tools[i] = apiTool{
				Name:        t.Name,
				Description: t.Description,
				InputSchema: t.InputSchema,
			}
		}
	}

	data, err := json.Marshal(body)
	if err != nil {
		return llm.Response{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(data))
	if err != nil {
		return llm.Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.apiKey)
	httpReq.Header.Set("anthropic-version", apiVersion)
	httpReq.Header.Set("anthropic-beta", "prompt-caching-2024-07-31")

	httpResp, err := p.client.Do(httpReq)
	if err != nil {
		return llm.Response{}, err
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(httpResp.Body)
		var errResp struct{ Error *apiError `json:"error,omitempty"` }
		json.Unmarshal(raw, &errResp) //nolint:errcheck
		if errResp.Error != nil {
			return llm.Response{}, fmt.Errorf("anthropic %d: %s", httpResp.StatusCode, errResp.Error.Message)
		}
		return llm.Response{}, fmt.Errorf("anthropic %d: %s", httpResp.StatusCode, raw)
	}

	var (
		blocks     []*sseBlock
		blockByIdx = map[int]int{} // SSE index → position in blocks slice
		textBuf    strings.Builder
		stopReason string
		curEvent   string
	)

	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			curEvent = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			dataStr := strings.TrimPrefix(line, "data: ")
			switch curEvent {
			case "content_block_start":
				var evt struct {
					Index        int `json:"index"`
					ContentBlock struct {
						Type string `json:"type"`
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"content_block"`
				}
				if json.Unmarshal([]byte(dataStr), &evt) == nil {
					blockByIdx[evt.Index] = len(blocks)
					blocks = append(blocks, &sseBlock{
						typ:  evt.ContentBlock.Type,
						id:   evt.ContentBlock.ID,
						name: evt.ContentBlock.Name,
					})
				}
			case "content_block_delta":
				var evt struct {
					Index int `json:"index"`
					Delta struct {
						Type        string `json:"type"`
						Text        string `json:"text"`
						PartialJSON string `json:"partial_json"`
					} `json:"delta"`
				}
				if json.Unmarshal([]byte(dataStr), &evt) == nil {
					switch evt.Delta.Type {
					case "text_delta":
						textBuf.WriteString(evt.Delta.Text)
						if req.OnChunk != nil && evt.Delta.Text != "" {
							req.OnChunk(evt.Delta.Text)
						}
					case "input_json_delta":
						if pos, ok := blockByIdx[evt.Index]; ok {
							blocks[pos].jsonBuf.WriteString(evt.Delta.PartialJSON)
						}
					}
				}
			case "message_delta":
				var evt struct {
					Delta struct {
						StopReason string `json:"stop_reason"`
					} `json:"delta"`
				}
				if json.Unmarshal([]byte(dataStr), &evt) == nil && evt.Delta.StopReason != "" {
					stopReason = evt.Delta.StopReason
				}
			case "error":
				var evt struct {
					Error struct {
						Message string `json:"message"`
					} `json:"error"`
				}
				if json.Unmarshal([]byte(dataStr), &evt) == nil {
					return llm.Response{}, fmt.Errorf("anthropic stream error: %s", evt.Error.Message)
				}
			}
			curEvent = ""
		}
	}
	if err := scanner.Err(); err != nil {
		return llm.Response{}, fmt.Errorf("read stream: %w", err)
	}

	var toolCalls []llm.ToolCall
	for _, b := range blocks {
		if b.typ == "tool_use" {
			input := json.RawMessage(b.jsonBuf.String())
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			toolCalls = append(toolCalls, llm.ToolCall{ID: b.id, Name: b.name, Input: input})
		}
	}

	result := llm.Response{
		Text:       textBuf.String(),
		ToolCalls:  toolCalls,
		StopReason: stopReason,
	}
	if len(result.ToolCalls) > 0 && result.StopReason != "max_tokens" {
		result.StopReason = "tool_use"
	}
	return result, nil
}

// marshalMessage converts a llm.Message into the Anthropic wire format.
func marshalMessage(m llm.Message) apiMessage {
	// Simple text-only message (most common case).
	if len(m.ToolCalls) == 0 && len(m.ToolResults) == 0 {
		return apiMessage{Role: m.Role, Content: m.Text}
	}

	var blocks []apiContentBlock

	// Text part (if present alongside tool calls/results).
	if m.Text != "" {
		blocks = append(blocks, apiContentBlock{Type: "text", Text: m.Text})
	}

	// Tool calls in assistant messages.
	for _, tc := range m.ToolCalls {
		blocks = append(blocks, apiContentBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Name,
			Input: tc.Input,
		})
	}

	// Tool results in user messages.
	for _, tr := range m.ToolResults {
		blocks = append(blocks, apiContentBlock{
			Type:      "tool_result",
			ToolUseID: tr.ToolCallID,
			Content:   tr.Content,
			IsError:   tr.IsError,
		})
	}

	return apiMessage{Role: m.Role, Content: blocks}
}
