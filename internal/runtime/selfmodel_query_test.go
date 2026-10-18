package runtime_test

import (
	"testing"

	"nine/internal/llm"
	"nine/internal/memory/memtest"
	"nine/internal/protocol"
)

// The self_model query returns the keys every turn's block carries, in its
// order, leaving out one never written and a self/ key the block does not read.
func TestSelfModelQuery(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"self/learned":       "2026-10-18: prefers short replies",
		"self/identity":      "Nine, a daemon",
		"self/_bootstrapped": "1",
	} {
		if err := store.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	d, sock := startDaemon(t, makeFactory(seqProvider([]llm.Response{})), nil, nil)
	d.ConfigureMemory(store)

	got, err := dial(t, sock).SelfModel()
	if err != nil {
		t.Fatal(err)
	}
	want := []protocol.SelfModelEntry{
		{Key: "self/identity", Value: "Nine, a daemon"},
		{Key: "self/learned", Value: "2026-10-18: prefers short replies"},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("self_model = %+v, want %+v", got, want)
	}
}
