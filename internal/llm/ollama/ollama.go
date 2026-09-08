// Package ollama implements the llm.Provider interface using Ollama's /api/chat endpoint.
package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"nine/internal/llm"
)

const (
	defaultEndpoint = "http://localhost:11434"

	// defaultTimeout bounds a single /api/chat call when the operator sets no
	// [llm].timeout_seconds. It is generous because a local model on CPU can
	// take minutes to answer a long prompt.
	defaultTimeout = 300 * time.Second
)

// Provider calls the Ollama chat API.
type Provider struct {
	model    string
	endpoint string
	numCtx   int  // 0 means use Ollama's default
	thinking bool // when true, request and stream extended thinking traces
	client   *http.Client

	// For model-native thinking mode detection.
	// "thinkCap" is set by probeThinking via thinkOnce.
	thinkOnce sync.Once
	thinkCap  bool
}

// New creates a Provider. Pass empty endpoint to use the default Ollama address.
// numCtx sets the model context window size; 0 uses Ollama's default. When
// thinking is true, the provider asks Ollama for extended thinking (think:true)
// and streams reasoning tokens via Request.OnThinkingChunk; when false, thinking
// is suppressed with /no_think as before. timeoutSecs is the HTTP client timeout
// in seconds ([llm].timeout_seconds); 0 uses defaultTimeout and a negative value
// disables the timeout entirely, leaving cancellation to the caller's context.
func New(model, endpoint string, numCtx int, thinking bool, timeoutSecs int) *Provider {
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	timeout := defaultTimeout
	switch {
	case timeoutSecs > 0:
		timeout = time.Duration(timeoutSecs) * time.Second
	case timeoutSecs < 0:
		timeout = 0
	}
	return &Provider{
		model:    model,
		endpoint: endpoint,
		numCtx:   numCtx,
		thinking: thinking,
		client:   &http.Client{Timeout: timeout},
	}
}

// --- Ollama /api/chat wire types ---

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Tools    []chatTool    `json:"tools,omitempty"`
	Stream   bool          `json:"stream"`
	Think    *bool         `json:"think,omitempty"` // enable extended thinking on capable models
	Options  *chatOptions  `json:"options,omitempty"`
}

type chatOptions struct {
	NumCtx int `json:"num_ctx,omitempty"`
}

type chatMessage struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	Thinking  string     `json:"thinking,omitempty"` // streamed reasoning when think:true
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
}

type toolCall struct {
	ID       string           `json:"id"`
	Function toolCallFunction `json:"function"`
}

type toolCallFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type chatTool struct {
	Type     string      `json:"type"`
	Function toolFuncDef `json:"function"`
}

type toolFuncDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type streamChunk struct {
	Message    chatMessage `json:"message"`
	DoneReason string      `json:"done_reason"`
	Done       bool        `json:"done"`
	Error      string      `json:"error,omitempty"`
	// Token accounting. Ollama reports these only on the final (done) chunk;
	// they are absent and therefore zero on every delta.
	PromptEvalCount int `json:"prompt_eval_count"`
	EvalCount       int `json:"eval_count"`
}

// Response from /api/show, used to detect model capabilities (e.g. thinking support).
type showResponse struct {
	Capabilities []string `json:"capabilities"`
}

// Complete sends a streaming request to the Ollama chat API and accumulates the result.
// It always streams because stream:false hangs on thinking models like Qwen3.
//
// When the provider has thinking enabled, it sets think:true and surfaces the
// model's reasoning tokens via req.OnThinkingChunk (never folded into the reply
// text). When thinking is disabled, /no_think is prepended to the last user
// message to suppress the extended thinking phase entirely.
func (p *Provider) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	var messages []chatMessage
	if req.System != "" {
		messages = append(messages, chatMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		messages = append(messages, convertMessage(m)...)
	}

	chatRequestBody := chatRequest{
		Model:    p.model,
		Messages: messages,
		Stream:   true,
	}

	thinkingOn := false
	if p.SupportsThinking(ctx) {
		think := p.thinking // use the provider's default thinking behavior
		if req.Think != nil {
			think = *req.Think // per-request override
		}
		chatRequestBody.Think = &think
		thinkingOn = think
	}

	// Set the model context window size if specified. Ollama's default is used when numCtx is 0.
	if p.numCtx > 0 {
		chatRequestBody.Options = &chatOptions{NumCtx: p.numCtx}
	}

	if len(req.Tools) > 0 {
		chatRequestBody.Tools = make([]chatTool, len(req.Tools))
		for i, t := range req.Tools {
			params := t.InputSchema
			if len(params) == 0 {
				params = json.RawMessage(`{"type":"object"}`)
			}
			chatRequestBody.Tools[i] = chatTool{
				Type: "function",
				Function: toolFuncDef{
					Name:        t.Name,
					Description: escapeXMLChars(t.Description),
					Parameters:  params,
				},
			}
		}
	}

	data, err := json.Marshal(chatRequestBody)
	if err != nil {
		return llm.Response{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.endpoint+"/api/chat", bytes.NewReader(data))
	if err != nil {
		return llm.Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := p.client.Do(httpReq)
	if err != nil {
		return llm.Response{}, err
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		var errResp struct {
			Error string `json:"error"`
		}
		json.NewDecoder(httpResp.Body).Decode(&errResp) //nolint:errcheck
		if errResp.Error != "" {
			return llm.Response{}, fmt.Errorf("ollama %d: %s", httpResp.StatusCode, errResp.Error)
		}
		return llm.Response{}, fmt.Errorf("ollama %d", httpResp.StatusCode)
	}

	var (
		content    strings.Builder
		doneReason string
		toolCalls  []llm.ToolCall
		usage      llm.Usage
	)

	// Process streaming chunks from the Ollama API. Each chunk may contain text, tool calls, and/or thinking tokens.
	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		var chunk streamChunk
		if err := json.Unmarshal(scanner.Bytes(), &chunk); err != nil {
			continue
		}
		if chunk.Error != "" {
			return llm.Response{}, fmt.Errorf("ollama: %s", chunk.Error)
		}

		// emit thinking chunks to TUI
		if chunk.Message.Thinking != "" && req.OnThinkingChunk != nil {
			req.OnThinkingChunk(chunk.Message.Thinking)
		}

		if chunk.Message.Content != "" {
			content.WriteString(chunk.Message.Content)
			if req.OnChunk != nil {
				req.OnChunk(chunk.Message.Content)
			}
		}
		// Tool calls can appear in any chunk, not just the final one.
		for i, tc := range chunk.Message.ToolCalls {
			id := tc.ID
			if id == "" {
				id = fmt.Sprintf("call_%d", i)
			}
			// Sanitize Input to avoid journal marshal errors from invalid JSON
			toolCalls = append(toolCalls, llm.ToolCall{
				ID:    id,
				Name:  tc.Function.Name,
				Input: llm.SanitizeRawMessage(tc.Function.Arguments),
			})
		}
		if chunk.Done {
			doneReason = chunk.DoneReason
			// Counts ride on the final chunk only. eval_count covers generated
			// tokens including thinking, which is what the model was charged for.
			usage = llm.Usage{
				InputTokens:  chunk.PromptEvalCount,
				OutputTokens: chunk.EvalCount,
			}
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return llm.Response{}, fmt.Errorf("read stream: %w", err)
	}

	result := llm.Response{
		Text:         content.String(),
		ToolCalls:    toolCalls,
		ThinkingUsed: thinkingOn,
		Usage:        usage,
	}

	// Stop reason detection
	switch doneReason {
	case "tool_calls":
		result.StopReason = "tool_use"
	case "length":
		result.StopReason = "max_tokens"
	default:
		result.StopReason = "end_turn"
	}
	if len(result.ToolCalls) > 0 {
		result.StopReason = "tool_use"
	}

	return result, nil
}

// escapeXMLChars replaces angle brackets and ampersands so that tool
// descriptions don't produce malformed XML when Ollama renders them into
// Qwen3-family prompt templates that use XML for function definitions.
func escapeXMLChars(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// convertMessage maps a llm.Message to one or more Ollama chat messages.
// Tool results each become their own "tool" role message.
func convertMessage(m llm.Message) []chatMessage {
	if len(m.ToolResults) > 0 {
		out := make([]chatMessage, len(m.ToolResults))
		for i, tr := range m.ToolResults {
			content := tr.Content
			if tr.IsError {
				content = "error: " + content
			}
			out[i] = chatMessage{Role: "tool", Content: content}
		}
		return out
	}

	if len(m.ToolCalls) > 0 {
		tcs := make([]toolCall, len(m.ToolCalls))
		for i, tc := range m.ToolCalls {
			// Sanitize Input: nil/invalid → {} so arguments is
			// always present and valid JSON for the API.
			args := llm.SanitizeRawMessage(tc.Input)
			if args == nil {
				args = json.RawMessage(`{}`)
			}
			tcs[i] = toolCall{Function: toolCallFunction{Name: tc.Name, Arguments: args}}
		}
		return []chatMessage{{Role: m.Role, Content: m.Text, ToolCalls: tcs}}
	}

	return []chatMessage{{Role: m.Role, Content: m.Text}}
}

const thinkingCapabilityName = "thinking"

// SupportsThinking reports whether the model supports native thinking mode.
// It probes the model once and caches the result.
func (p *Provider) SupportsThinking(ctx context.Context) bool {
	p.thinkOnce.Do(func() {
		body, _ := json.Marshal(map[string]string{"model": p.model})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint+"/api/show", bytes.NewReader(body))
		if err != nil {
			return
		}

		req.Header.Set("Content-Type", "application/json")
		resp, err := p.client.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return
		}

		var showResp showResponse
		if err := json.NewDecoder(resp.Body).Decode(&showResp); err != nil {
			return
		}

		if slices.Contains(showResp.Capabilities, thinkingCapabilityName) {
			p.thinkCap = true
			return
		}
	})

	// return the cached result of the probe, which is set by the thinkOnce.Do above.
	return p.thinkCap
}
