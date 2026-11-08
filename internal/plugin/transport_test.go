package plugin_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"nine/internal/plugin"
)

// callN issues n concurrent "sleep" calls and returns the wall-clock duration.
func callN(t *testing.T, m *plugin.Manager, p *plugin.Plugin, n int) time.Duration {
	t.Helper()
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Go(func() {
			if _, err := m.Call(context.Background(), p, "sleep", json.RawMessage(`{}`)); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("sleep call: %v", err)
	}
	return time.Since(start)
}

// TestConcurrencyUnbounded: unix-socket HTTP is concurrent, so N concurrent calls
// to a 150ms sleep handler complete in roughly one sleep, not N×.
func TestConcurrencyUnbounded(t *testing.T) {
	path := buildBinary(t, "./internal/plugin/testplugin")
	m := plugin.NewManager("")
	p, err := m.Start(path)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop(p) //nolint:errcheck

	const n = 6
	elapsed := callN(t, m, p, n)
	// One sleep is 150ms; serial would be ~900ms. Allow generous slack.
	if elapsed > 450*time.Millisecond {
		t.Errorf("unbounded: %d concurrent calls took %v, expected ~one sleep", n, elapsed)
	}
}

// TestConcurrencyCapped: the stdio transport is serial by construction
// (max_concurrent 1 — one client-side write lock owns the stream), so the same
// calls serialize and wall time grows to roughly N× one sleep.
func TestConcurrencyCapped(t *testing.T) {
	path := buildBinary(t, "./internal/plugin/testplugin")
	m := plugin.NewManager("")
	p, err := m.StartStdio(path)
	if err != nil {
		t.Fatalf("StartStdio: %v", err)
	}
	defer m.Stop(p) //nolint:errcheck

	const n = 6
	elapsed := callN(t, m, p, n)
	// Serialized: ~6×150ms = 900ms. Require clearly more than the unbounded case.
	if elapsed < 600*time.Millisecond {
		t.Errorf("serial stdio: %d calls took %v, expected serial (~%v)", n, elapsed, n*150*time.Millisecond)
	}
}

// TestContextCancellation: a call whose context deadline fires mid-handler
// returns promptly with a context error — HTTP carries the ctx per request, so
// the daemon stops waiting even though the (150ms) sleep handler keeps running.
func TestContextCancellation(t *testing.T) {
	path := buildBinary(t, "./internal/plugin/testplugin")
	m := plugin.NewManager("")
	p, err := m.Start(path)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop(p) //nolint:errcheck

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = m.Call(ctx, p, "sleep", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected a context error, got nil")
	}
	if elapsed := time.Since(start); elapsed > 120*time.Millisecond {
		t.Errorf("call returned after %v, expected cancellation near the 30ms deadline", elapsed)
	}
}

// TestTraceIDPropagation: a trace ID set on the call context reaches the plugin
// over the socket transport — daemon ctx → X-Nine-Request-ID header → handler.
func TestTraceIDPropagation(t *testing.T) {
	path := buildBinary(t, "./internal/plugin/testplugin")
	m := plugin.NewManager("")
	p, err := m.Start(path)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop(p) //nolint:errcheck

	ctx := plugin.ContextWithRequestID(context.Background(), "trace-abc-123")
	r, err := m.Call(ctx, p, "traceid", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if r.Output != "trace-abc-123" {
		t.Errorf("handler saw request id %q, want %q", r.Output, "trace-abc-123")
	}
}

// TestCrashIsolation: a plugin that exits mid-call fails that call and any
// follow-up call, and the manager reports the failure without crashing the host.
func TestCrashIsolation(t *testing.T) {
	path := buildBinary(t, "./internal/plugin/testplugin")
	m := plugin.NewManager("")
	p, err := m.Start(path)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop(p) //nolint:errcheck

	if _, err := m.Call(context.Background(), p, "crash", json.RawMessage(`{}`)); err == nil {
		t.Error("expected error from crashed plugin, got nil")
	}
	// The connection is gone; a follow-up call also errors cleanly (no panic/hang).
	if _, err := m.Call(context.Background(), p, "sleep", json.RawMessage(`{}`)); err == nil {
		t.Error("expected error after crash, got nil")
	}
}
