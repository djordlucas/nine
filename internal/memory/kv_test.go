package memory_test

import (
	"testing"

	"nine/internal/memory/memtest"
)

func TestKVRoundTrip(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}

	// Missing key: found=false, no error.
	if _, found, err := store.Get("nope"); err != nil || found {
		t.Fatalf("missing key: found=%v err=%v, want false/nil", found, err)
	}

	if err := store.Set("self/name", "nine"); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.Get("self/name")
	if err != nil || !found || got != "nine" {
		t.Fatalf("Get = %q found=%v err=%v, want \"nine\"/true", got, found, err)
	}

	// Set overwrites (upsert on key).
	if err := store.Set("self/name", "NINE"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := store.Get("self/name"); got != "NINE" {
		t.Errorf("after overwrite Get = %q, want NINE", got)
	}

	// Delete removes.
	if err := store.Delete("self/name"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := store.Get("self/name"); found {
		t.Error("key should be gone after Delete")
	}
}

func TestKVListPrefix(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"self/a", "self/b", "other/c"} {
		if err := store.Set(k, "v"); err != nil {
			t.Fatal(err)
		}
	}
	keys, err := store.List("self/")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("List(self/) = %v, want 2 keys", keys)
	}
	for _, k := range keys {
		if k == "other/c" {
			t.Errorf("prefix scan leaked %q", k)
		}
	}
	// Empty prefix lists all.
	if all, _ := store.List(""); len(all) != 3 {
		t.Errorf("List(\"\") = %d keys, want 3", len(all))
	}
	// KVGetString convenience: empty string for a missing key.
	if s := store.KVGetString("missing"); s != "" {
		t.Errorf("KVGetString(missing) = %q, want empty", s)
	}
}
