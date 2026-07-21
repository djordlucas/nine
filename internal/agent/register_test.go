package agent_test

import (
	"context"
	"encoding/json"
	"testing"

	"nine/internal/agent"
	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

func newTestStore(t *testing.T) *memory.Store {
	t.Helper()
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestRegisterGapReport(t *testing.T) {
	d := agent.New()
	var received string
	agent.RegisterGapReport(d, func(desc string) { received = desc })

	res, err := d.Dispatch(context.Background(), "gap_report", json.RawMessage(`{"description":"missing tool"}`))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if res.Output == "" {
		t.Error("expected non-empty output")
	}
	if received != "missing tool" {
		t.Errorf("received = %q, want 'missing tool'", received)
	}
}

func TestRegisterMultipleToolsEachTakeEffect(t *testing.T) {
	d := agent.New()
	store := newTestStore(t)
	agent.RegisterMemoryTools(d, store, nil, nil, false)

	if _, err := d.Dispatch(context.Background(), "memory_set",
		json.RawMessage(`{"key":"k","value":"v"}`)); err != nil {
		t.Errorf("memory_set: unexpected error: %v", err)
	}
	if _, err := d.Dispatch(context.Background(), "memory_get",
		json.RawMessage(`{"key":"k"}`)); err != nil {
		t.Errorf("memory_get: unexpected error: %v", err)
	}
}
