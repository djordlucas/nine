package ollama_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"nine/internal/embed/ollama"
)

// --- unit tests (always run, no Ollama required) ---

func TestEmbedUnit(t *testing.T) {
	want := []float32{0.1, 0.2, 0.3}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embeddings" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var body struct {
			Model  string `json:"model"`
			Prompt string `json:"prompt"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Model != "test-model" {
			t.Errorf("model = %q, want test-model", body.Model)
		}
		if body.Prompt != "hello world" {
			t.Errorf("prompt = %q, want 'hello world'", body.Prompt)
		}
		json.NewEncoder(w).Encode(map[string]any{"embedding": want})
	}))
	defer srv.Close()

	e := ollama.New("test-model", srv.URL)
	got, err := e.Embed(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if math.Abs(float64(got[i]-want[i])) > 1e-6 {
			t.Errorf("[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestEmbedUnitHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not found", http.StatusNotFound)
	}))
	defer srv.Close()

	e := ollama.New("bad-model", srv.URL)
	_, err := e.Embed(context.Background(), "text")
	if err == nil {
		t.Error("expected error for HTTP 404, got nil")
	}
}

func TestEmbedUnitEmptyVector(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"embedding":[]}`)
	}))
	defer srv.Close()

	e := ollama.New("m", srv.URL)
	_, err := e.Embed(context.Background(), "text")
	if err == nil {
		t.Error("expected error for empty embedding, got nil")
	}
}

// --- integration tests (skipped when Ollama is not available) ---

func ollamaEndpoint() string {
	if e := os.Getenv("OLLAMA_ENDPOINT"); e != "" {
		return e
	}
	return "http://localhost:11434"
}

func ollamaAvailable(endpoint string) bool {
	resp, err := http.Get(endpoint + "/api/tags")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func TestOllamaIntegration(t *testing.T) {
	endpoint := ollamaEndpoint()
	if !ollamaAvailable(endpoint) {
		t.Skip("Ollama not available at " + endpoint)
	}
	model := os.Getenv("OLLAMA_EMBED_MODEL")
	if model == "" {
		model = "nomic-embed-text"
	}

	e := ollama.New(model, endpoint)
	vec, err := e.Embed(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) == 0 {
		t.Error("embedding vector is empty")
	}
	vec2, err := e.Embed(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("second Embed: %v", err)
	}
	if len(vec) != len(vec2) {
		t.Errorf("dimension mismatch: %d vs %d", len(vec), len(vec2))
	}
	t.Logf("embedding dimension: %d", len(vec))
}

func TestOllamaIntegrationSemantics(t *testing.T) {
	endpoint := ollamaEndpoint()
	if !ollamaAvailable(endpoint) {
		t.Skip("Ollama not available at " + endpoint)
	}
	model := os.Getenv("OLLAMA_EMBED_MODEL")
	if model == "" {
		model = "nomic-embed-text"
	}

	e := ollama.New(model, endpoint)
	v1, _ := e.Embed(context.Background(), "cat")
	v2, _ := e.Embed(context.Background(), "dog")
	v3, _ := e.Embed(context.Background(), "quantum physics")
	if len(v1) == 0 {
		t.Fatal("empty embedding")
	}
	sim12 := cosineSim(v1, v2)
	sim13 := cosineSim(v1, v3)
	t.Logf("cos(cat,dog)=%.4f  cos(cat,quantum)=%.4f", sim12, sim13)
	if sim12 <= sim13 {
		t.Errorf("expected cat~dog > cat~quantum, got %.4f <= %.4f", sim12, sim13)
	}
}

func cosineSim(a, b []float32) float32 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(na) * math.Sqrt(nb)))
}
