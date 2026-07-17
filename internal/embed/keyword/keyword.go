// Package keyword implements a zero-dependency text embedder using feature
// hashing. Each term is projected into a fixed 512-dimensional space via
// FNV-1a hashing, stop words are discarded, and the result is L2-normalised.
// Cosine similarity over these vectors gives a reasonable keyword-overlap score
// suitable for tool selection and semantic search when no embedding model is
// available.
package keyword

import (
	"context"
	"hash/fnv"
	"math"
	"strings"
	"unicode"
)

const dims = 512

// Embedder projects text into a fixed vector space using feature hashing.
// It requires no external model or corpus and never returns an error.
type Embedder struct{}

// New returns an Embedder ready for use.
func New() *Embedder { return &Embedder{} }

func (e *Embedder) Embed(_ context.Context, text string) ([]float32, error) {
	vec := make([]float32, dims)
	for _, term := range tokenize(text) {
		if stopWords[term] {
			continue
		}
		h := fnv.New32a()
		h.Write([]byte(term)) //nolint:errcheck
		vec[h.Sum32()%dims] += 1.0
	}
	l2normalize(vec)
	return vec, nil
}

func tokenize(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func l2normalize(vec []float32) {
	var sum float64
	for _, v := range vec {
		sum += float64(v) * float64(v)
	}
	if sum == 0 {
		return
	}
	scale := float32(1.0 / math.Sqrt(sum))
	for i := range vec {
		vec[i] *= scale
	}
}

var stopWords = map[string]bool{
	"a": true, "an": true, "the": true, "and": true, "or": true, "but": true,
	"in": true, "on": true, "at": true, "to": true, "for": true, "of": true,
	"with": true, "by": true, "from": true, "is": true, "are": true, "was": true,
	"be": true, "been": true, "being": true, "have": true, "has": true, "had": true,
	"do": true, "does": true, "did": true, "will": true, "would": true, "could": true,
	"should": true, "may": true, "might": true, "can": true, "it": true, "its": true,
	"this": true, "that": true, "these": true, "those": true, "not": true, "no": true,
	"as": true, "if": true, "so": true, "up": true, "out": true, "any": true,
	"all": true, "each": true, "which": true, "when": true, "then": true,
}
