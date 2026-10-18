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

// The memory queries list every key with its size, and read one; a missing
// key is an error naming it.
func TestMemoryQueries(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"notes/a": "hello", "self/identity": "Nine"} {
		if err := store.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	d, sock := startDaemon(t, makeFactory(seqProvider([]llm.Response{})), nil, nil)
	d.ConfigureMemory(store)
	c := dial(t, sock)

	list, err := c.MemoryList()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Key != "notes/a" || list[0].Bytes != 5 || list[0].UpdatedAt == "" {
		t.Errorf("memory_list = %+v", list)
	}
	v, err := c.MemoryGet("self/identity")
	if err != nil || v.Value != "Nine" {
		t.Errorf("memory_get = %+v, %v", v, err)
	}
	if _, err := c.MemoryGet("nope"); err == nil {
		t.Error("memory_get of a missing key: want an error")
	}
}
