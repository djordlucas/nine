// Package ollama implements embed.Embedder using Ollama's /api/embeddings endpoint.
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const defaultEndpoint = "http://localhost:11434"

// Embedder calls Ollama's embedding API.
type Embedder struct {
	model    string
	endpoint string
	client   *http.Client
}

// New creates an Embedder. Pass empty endpoint to use the default Ollama address.
func New(model, endpoint string) *Embedder {
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	return &Embedder{
		model:    model,
		endpoint: endpoint,
		client:   &http.Client{Timeout: 30 * time.Second},
	}
}

// Embed sends text to Ollama and returns the embedding vector.
func (e *Embedder) Embed(ctx context.Context, text string) ([]float32, error) {
	body, _ := json.Marshal(map[string]string{
		"model":  e.model,
		"prompt": text,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		e.endpoint+"/api/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama %d: %s", resp.StatusCode, raw)
	}

	var result struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("parse embedding: %w", err)
	}
	if len(result.Embedding) == 0 {
		return nil, fmt.Errorf("ollama returned empty embedding")
	}
	return result.Embedding, nil
}
