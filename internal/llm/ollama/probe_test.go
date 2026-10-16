package ollama

// In-package so the tests can wait out probeBackoff by name: the retry it paces
// is only observable by waiting, and a copy of the constant in the external test
// package would be a second thing to keep in sync.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// An inconclusive probe must not stand as a verdict: a Nine that starts before
// Ollama is up, or before the model is pulled, has to pick the capability up
// later rather than report a thinking model as incapable for the rest of its life.
func TestSupportsThinkingRetriesAfterAFailedProbe(t *testing.T) {
	t.Parallel()

	var probes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// The first probe finds nothing answering; later ones find Ollama.
		if probes.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"capabilities":["completion","thinking"]}`))
	}))
	t.Cleanup(srv.Close)

	p := New("qwen3", srv.URL, 0, true, 0)
	if p.SupportsThinking(context.Background()) {
		t.Error("SupportsThinking = true while the probe is failing, want false")
	}

	// Inside the backoff window the retry is suppressed, so the endpoint is
	// asked once however often the loop and Complete ask.
	_ = p.SupportsThinking(context.Background())
	if n := probes.Load(); n != 1 {
		t.Errorf("probes inside the backoff window = %d, want 1", n)
	}

	time.Sleep(probeBackoff + 100*time.Millisecond)
	if !p.SupportsThinking(context.Background()) {
		t.Errorf("SupportsThinking = false once Ollama answers, want true (probes: %d)", probes.Load())
	}

	// The answer is now known, so it is cached: the next call returns it without
	// asking again, and is not subject to the backoff that gates a retry.
	before := probes.Load()
	if !p.SupportsThinking(context.Background()) || probes.Load() != before {
		t.Errorf("a known capability was re-probed: probes %d -> %d", before, probes.Load())
	}
}

// The capability belongs to the model, not to the turn that happened to ask
// first: a cancelled turn must not be what decides it.
func TestSupportsThinkingIgnoresCallerCancellation(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"capabilities":["completion","thinking"]}`))
	}))
	t.Cleanup(srv.Close)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	p := New("qwen3", srv.URL, 0, true, 0)
	if !p.SupportsThinking(cancelled) {
		t.Error("SupportsThinking = false for a cancelled caller, want true: the probe detaches from it")
	}
}
