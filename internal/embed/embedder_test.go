package embed_test

import (
	"context"
	"testing"

	"nine/internal/embed"
)

// R-EMB.2: providers are constructible from [embeddings] config; `none` disables
// ranking (nil embedder), and the default/keyword produce a usable vector with
// no network.
func TestBuildDispatch(t *testing.T) {
	cases := []struct {
		provider string
		wantNil  bool
	}{
		{"", false},        // default → keyword
		{"keyword", false}, // explicit keyword
		{"none", true},     // disables ranking
		{"ollama", false},  // constructed (no network call at build time)
	}
	for _, c := range cases {
		e := embed.Build(c.provider, "", "http://localhost:11434")
		if c.wantNil {
			if e != nil {
				t.Errorf("Build(%q) = non-nil, want nil (ranking disabled)", c.provider)
			}
			continue
		}
		if e == nil {
			t.Errorf("Build(%q) = nil, want an embedder", c.provider)
		}
	}
}

// The default and "keyword" providers are the built-in embedder and produce a
// deterministic, non-empty vector without any network access.
func TestBuildKeywordIsUsableOffline(t *testing.T) {
	for _, p := range []string{"", "keyword"} {
		e := embed.Build(p, "", "")
		v, err := e.Embed(context.Background(), "hello world")
		if err != nil {
			t.Fatalf("Build(%q).Embed: %v", p, err)
		}
		if len(v) == 0 {
			t.Errorf("Build(%q) produced an empty vector", p)
		}
	}
}

// EmbedderFunc adapts a plain function to the interface.
func TestEmbedderFunc(t *testing.T) {
	called := false
	var e embed.Embedder = embed.EmbedderFunc(func(_ context.Context, _ string) ([]float32, error) {
		called = true
		return []float32{1, 2, 3}, nil
	})
	v, err := e.Embed(context.Background(), "x")
	if err != nil || !called || len(v) != 3 {
		t.Fatalf("EmbedderFunc adapter failed: v=%v err=%v called=%v", v, err, called)
	}
}
