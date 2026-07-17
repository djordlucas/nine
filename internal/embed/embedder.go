// Package embed defines the provider-agnostic text embedding interface.
package embed

import (
	"context"

	"nine/internal/embed/keyword"
	"nine/internal/embed/ollama"
)

// Embedder converts text into a float32 vector.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// EmbedderFunc adapts a plain function to the Embedder interface.
type EmbedderFunc func(ctx context.Context, text string) ([]float32, error)

func (f EmbedderFunc) Embed(ctx context.Context, text string) ([]float32, error) {
	return f(ctx, text)
}

// Build constructs an Embedder from the given provider, model, and endpoint.
// provider may be "ollama", "none", or "" / "keyword" for the default keyword embedder.
func Build(provider, model, endpoint string) Embedder {
	switch provider {
	case "ollama":
		if model == "" {
			model = "nomic-embed-text"
		}
		return ollama.New(model, endpoint)
	case "none":
		return nil
	default:
		return keyword.New()
	}
}
