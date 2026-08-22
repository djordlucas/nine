package memory_test

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

func openStore(t *testing.T) *memory.Store {
	t.Helper()
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestToolStateRoundTrip(t *testing.T) {
	store := openStore(t)
	var q memory.ToolStateQuota

	if _, found, err := store.ToolStateGet("geocode", "", "nope"); err != nil || found {
		t.Fatalf("missing key: found=%v err=%v, want false/nil", found, err)
	}

	if err := store.ToolStateSet("geocode", "", "etag", "abc123", q); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.ToolStateGet("geocode", "", "etag")
	if err != nil || !found || got != "abc123" {
		t.Fatalf("Get = %q found=%v err=%v, want abc123/true", got, found, err)
	}

	// Replacing a key keeps one row rather than accumulating.
	if err := store.ToolStateSet("geocode", "", "etag", "def456", q); err != nil {
		t.Fatal(err)
	}
	keys, _, err := store.ToolStateUsage("geocode", "")
	if err != nil || keys != 1 {
		t.Fatalf("after replace: keys=%d err=%v, want 1", keys, err)
	}

	if err := store.ToolStateDelete("geocode", "", "etag"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := store.ToolStateGet("geocode", "", "etag"); found {
		t.Fatal("key survived delete")
	}
	// Deleting an absent key is not an error.
	if err := store.ToolStateDelete("geocode", "", "etag"); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

// Namespaces are keyed by (tool, scope). Two tools, and two scopes of one tool,
// must not see each other — this is the isolation the capability rests on.
func TestToolStateNamespacesAreIsolated(t *testing.T) {
	store := openStore(t)
	var q memory.ToolStateQuota

	for _, c := range []struct{ tool, scope, val string }{
		{"alpha", "", "alpha-tool-scope"},
		{"beta", "", "beta-tool-scope"},
		{"alpha", "conv-1", "alpha-conv-1"},
		{"alpha", "conv-2", "alpha-conv-2"},
	} {
		if err := store.ToolStateSet(c.tool, c.scope, "k", c.val, q); err != nil {
			t.Fatal(err)
		}
	}

	for _, c := range []struct{ tool, scope, want string }{
		{"alpha", "", "alpha-tool-scope"},
		{"beta", "", "beta-tool-scope"},
		{"alpha", "conv-1", "alpha-conv-1"},
		{"alpha", "conv-2", "alpha-conv-2"},
	} {
		got, found, err := store.ToolStateGet(c.tool, c.scope, "k")
		if err != nil || !found || got != c.want {
			t.Fatalf("(%s,%s) = %q found=%v, want %q", c.tool, c.scope, got, found, c.want)
		}
	}
}

func TestToolStateListPrefix(t *testing.T) {
	store := openStore(t)
	var q memory.ToolStateQuota

	for _, k := range []string{"seen:a", "seen:b", "cursor", "seen_other"} {
		if err := store.ToolStateSet("watch", "", k, "v", q); err != nil {
			t.Fatal(err)
		}
	}

	all, err := store.ToolStateList("watch", "", "")
	if err != nil || len(all) != 4 {
		t.Fatalf("empty prefix listed %v (err=%v), want all 4", all, err)
	}

	got, err := store.ToolStateList("watch", "", "seen:")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "seen:a" || got[1] != "seen:b" {
		t.Fatalf("prefix seen: = %v, want [seen:a seen:b]", got)
	}

	// The wildcard in a literal prefix must not match: "seen_" is a key prefix,
	// not "seen" followed by any character.
	under, err := store.ToolStateList("watch", "", "seen_")
	if err != nil {
		t.Fatal(err)
	}
	if len(under) != 1 || under[0] != "seen_other" {
		t.Fatalf("prefix seen_ = %v, want [seen_other] — LIKE wildcard leaked", under)
	}
}

func TestToolStateQuotas(t *testing.T) {
	store := openStore(t)

	t.Run("max keys", func(t *testing.T) {
		q := memory.ToolStateQuota{MaxKeys: 2}
		for _, k := range []string{"a", "b"} {
			if err := store.ToolStateSet("keyed", "", k, "v", q); err != nil {
				t.Fatal(err)
			}
		}
		err := store.ToolStateSet("keyed", "", "c", "v", q)
		var qe *memory.ToolStateQuotaError
		if !errors.As(err, &qe) {
			t.Fatalf("third key: err=%v, want a ToolStateQuotaError", err)
		}
		// Rewriting an existing key is not a new key and must still be allowed.
		if err := store.ToolStateSet("keyed", "", "a", "v2", q); err != nil {
			t.Fatalf("rewriting an existing key at the cap: %v", err)
		}
	})

	t.Run("max value size", func(t *testing.T) {
		q := memory.ToolStateQuota{MaxValueKB: 1}
		err := store.ToolStateSet("sized", "", "big", strings.Repeat("x", 2048), q)
		var qe *memory.ToolStateQuotaError
		if !errors.As(err, &qe) {
			t.Fatalf("oversized value: err=%v, want a ToolStateQuotaError", err)
		}
		if err := store.ToolStateSet("sized", "", "ok", strings.Repeat("x", 512), q); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("max total", func(t *testing.T) {
		q := memory.ToolStateQuota{MaxTotalKB: 1}
		if err := store.ToolStateSet("total", "", "a", strings.Repeat("x", 600), q); err != nil {
			t.Fatal(err)
		}
		err := store.ToolStateSet("total", "", "b", strings.Repeat("x", 600), q)
		var qe *memory.ToolStateQuotaError
		if !errors.As(err, &qe) {
			t.Fatalf("over total: err=%v, want a ToolStateQuotaError", err)
		}
	})
}

func TestToolStateExpiry(t *testing.T) {
	store := openStore(t)

	// A TTL that lapses while the test runs: the value must become invisible on
	// time, not only once the sweeper next ticks. (A zero or negative TTL means
	// "never expires", so an elapsed one is the only way to reach this path.)
	brief := memory.ToolStateQuota{TTL: time.Millisecond}
	if err := store.ToolStateSet("ttl", "", "gone", "v", brief); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, found, _ := store.ToolStateGet("ttl", "", "gone"); found {
		t.Fatal("expired value is readable before the sweep; the read filter is missing")
	}
	if keys, err := store.ToolStateList("ttl", "", ""); err != nil || len(keys) != 0 {
		t.Fatalf("expired key listed: %v (err=%v)", keys, err)
	}

	live := memory.ToolStateQuota{TTL: time.Hour}
	if err := store.ToolStateSet("ttl", "", "stays", "v", live); err != nil {
		t.Fatal(err)
	}

	// The lapsed row is swept; the live one is not.
	n, err := store.ToolStateExpire()
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("ToolStateExpire reclaimed nothing, want the expired row")
	}
	if _, found, _ := store.ToolStateGet("ttl", "", "stays"); !found {
		t.Fatal("the sweep took a live row")
	}
}

func TestToolStateSwap(t *testing.T) {
	store := openStore(t)
	var q memory.ToolStateQuota
	expect := func(s string) *string { return &s }

	// nil expected means create-if-absent.
	ok, err := store.ToolStateSwap("cas", "", "k", nil, "first", q)
	if err != nil || !ok {
		t.Fatalf("create-if-absent: ok=%v err=%v", ok, err)
	}
	ok, err = store.ToolStateSwap("cas", "", "k", nil, "second", q)
	if err != nil || ok {
		t.Fatalf("create-if-absent on an existing key: ok=%v err=%v, want false", ok, err)
	}

	// A stale expectation does not take.
	ok, err = store.ToolStateSwap("cas", "", "k", expect("wrong"), "third", q)
	if err != nil || ok {
		t.Fatalf("stale swap: ok=%v err=%v, want false", ok, err)
	}
	if got, _, _ := store.ToolStateGet("cas", "", "k"); got != "first" {
		t.Fatalf("value changed on a failed swap: %q", got)
	}

	ok, err = store.ToolStateSwap("cas", "", "k", expect("first"), "third", q)
	if err != nil || !ok {
		t.Fatalf("matching swap: ok=%v err=%v", ok, err)
	}
	if got, _, _ := store.ToolStateGet("cas", "", "k"); got != "third" {
		t.Fatalf("value after swap = %q, want third", got)
	}
}

// The reason swap exists: two calls to one tool run concurrently, so exactly one
// of N racing compare-and-sets must win. A read-modify-write would lose writes
// here, silently.
func TestToolStateSwapIsAtomicUnderConcurrency(t *testing.T) {
	store := openStore(t)
	var q memory.ToolStateQuota

	if err := store.ToolStateSet("race", "", "k", "start", q); err != nil {
		t.Fatal(err)
	}

	const racers = 8
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)
	start := make(chan struct{})
	expected := "start"
	for i := range racers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ok, err := store.ToolStateSwap("race", "", "k", &expected, "winner", q)
			if err != nil {
				t.Errorf("racer %d: %v", i, err)
				return
			}
			if ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if wins != 1 {
		t.Fatalf("%d racers won the compare-and-set, want exactly 1", wins)
	}
}

func TestToolStateDropScope(t *testing.T) {
	store := openStore(t)
	var q memory.ToolStateQuota

	for _, scope := range []string{"conv-1", "conv-2"} {
		for _, k := range []string{"a", "b"} {
			if err := store.ToolStateSet("scoped", scope, k, "v", q); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := store.ToolStateDropScope("scoped", "conv-1"); err != nil {
		t.Fatal(err)
	}
	if keys, _ := store.ToolStateList("scoped", "conv-1", ""); len(keys) != 0 {
		t.Fatalf("conv-1 kept %v after drop", keys)
	}
	if keys, _ := store.ToolStateList("scoped", "conv-2", ""); len(keys) != 2 {
		t.Fatalf("conv-2 lost keys to another scope's drop: %v", keys)
	}
}
