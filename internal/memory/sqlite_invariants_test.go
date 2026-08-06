package memory_test

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

// storedTimeRE is the shape every timestamp nine writes must have: fixed-width
// RFC3339 UTC microseconds.
var storedTimeRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}Z$`)

// TestStoredTimestampShape is the guard for the migration's sharpest edge.
//
// SQLite compares TEXT bytewise, so a single column written in a different shape
// silently corrupts ORDER BY and every range predicate against it, with no error
// anywhere. A stray CURRENT_TIMESTAMP yields "2026-08-04 12:34:56" — 19 chars,
// a space instead of 'T', no fraction — which sorts before *every* correctly
// written value. Nothing in the type system prevents that, so assert the bytes.
func TestStoredTimestampShape(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}

	// One write through each domain that stamps a timestamp.
	if err := store.Set("k", "v"); err != nil {
		t.Fatal(err)
	}
	if err := store.FileStore("notes/a.md", "body"); err != nil {
		t.Fatal(err)
	}
	if err := store.VectorStore("v1", "ns", "key", []float32{1, 0}); err != nil {
		t.Fatal(err)
	}
	if err := store.ConversationCreate("c1"); err != nil {
		t.Fatal(err)
	}
	if err := store.ConversationUpdateHistory("c1", json.RawMessage(`[]`)); err != nil {
		t.Fatal(err)
	}
	if err := store.GoalCreate("g1", "desc", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.GoalUpdateStatus("g1", "done"); err != nil {
		t.Fatal(err)
	}
	if err := store.SkillUpsert(memory.Skill{Name: "s1", Description: "d"}); err != nil {
		t.Fatal(err)
	}
	if err := store.RelatedSessionAdd("a1", "a2", 0.9); err != nil {
		t.Fatal(err)
	}
	if err := store.SessionEventsAppend([]memory.SessionEvent{
		{AgentID: "a1", Turn: 1, SpanID: "s", Type: "turn_start"},
	}); err != nil {
		t.Fatal(err)
	}

	// Every timestamp column that a write path populates.
	columns := []struct{ table, column string }{
		{"kv", "updated_at"},
		{"files", "stored_at"},
		{"vectors", "stored_at"},
		{"conversations", "created_at"},
		{"conversations", "updated_at"},
		{"goals", "created_at"},
		{"goals", "updated_at"},
		{"skills", "updated_at"},
		{"related_sessions", "updated_at"},
		{"session_events", "ts"},
	}
	for _, c := range columns {
		vals, err := store.RawColumn(`SELECT ` + c.column + ` FROM ` + c.table)
		if err != nil {
			t.Fatalf("read %s.%s: %v", c.table, c.column, err)
		}
		if len(vals) == 0 {
			t.Fatalf("%s.%s: no rows — the test did not exercise this table", c.table, c.column)
		}
		for _, v := range vals {
			if !storedTimeRE.MatchString(v) {
				t.Errorf("%s.%s = %q, want fixed-width RFC3339 UTC microseconds; "+
					"a differing shape breaks ORDER BY and range predicates silently",
					c.table, c.column, v)
			}
		}
	}
}

// TestDDLTimestampDefaultMatchesGoShape checks the other writer of timestamps:
// the schema's own DEFAULT. It must produce the identical shape, or a
// SQL-defaulted row would sort wrongly against a Go-written one.
func TestDDLTimestampDefaultMatchesGoShape(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	// Insert without supplying the timestamp, so the DDL default fires.
	if err := store.ExecRaw(`INSERT INTO kv(key, value) VALUES('defaulted','v')`); err != nil {
		t.Fatal(err)
	}
	vals, err := store.RawColumn(`SELECT updated_at FROM kv WHERE key = 'defaulted'`)
	if err != nil {
		t.Fatal(err)
	}
	if len(vals) != 1 || !storedTimeRE.MatchString(vals[0]) {
		t.Fatalf("DDL default produced %q, want the same shape writeTime emits", vals)
	}
}

// TestFileDeleteOlderThanActuallyDeletes asserts a non-zero count on purpose.
//
// The failure mode this exists for returns no error at all: binding a time.Time
// directly makes the comparison permanently false, so the sweep deletes nothing
// forever and the files table grows without bound. A test asserting only
// err == nil passes against that broken version.
func TestFileDeleteOlderThanActuallyDeletes(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FileStore("spill/old.md", "stale"); err != nil {
		t.Fatal(err)
	}
	// Backdate it well past the retention window.
	old := time.Now().Add(-48 * time.Hour).UTC().Format("2006-01-02T15:04:05.000000Z07:00")
	if err := store.ExecRaw(`UPDATE files SET stored_at = ? WHERE path = ?`, old, "spill/old.md"); err != nil {
		t.Fatal(err)
	}
	if err := store.FileStore("spill/fresh.md", "keep"); err != nil {
		t.Fatal(err)
	}

	n, err := store.FileDeleteOlderThan("spill/", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("FileDeleteOlderThan deleted %d rows, want 1 — the retention sweep is not matching stored timestamps", n)
	}
	paths, err := store.FileList("spill/")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "spill/fresh.md" {
		t.Fatalf("remaining = %v, want only spill/fresh.md", paths)
	}
}

// TestTimeArgumentPanics locks in the guard that makes the above bug
// unreproducible: a time.Time bound as a query argument is a programming error,
// caught loudly rather than silently producing a false predicate.
func TestTimeArgumentPanics(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("binding a time.Time did not panic; the guard against silently-false timestamp predicates is gone")
		}
		if msg, ok := r.(string); !ok || !strings.Contains(msg, "writeTime") {
			t.Errorf("panic = %v, want a message pointing at writeTime", r)
		}
	}()
	store.ExecRaw(`SELECT 1 WHERE ? > ''`, time.Now()) //nolint:errcheck // expected to panic
}

// TestSessionEventSeqNeverReused is the AUTOINCREMENT guard.
//
// A plain rowid alias reuses ids after a delete. Since scrub deletes aggressively
// and subscribers persist absolute seq values as cursors, a reused seq means an
// event silently never reaches a subscriber whose cursor already passed that
// number. Nothing errors; the event simply vanishes.
func TestSessionEventSeqNeverReused(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}

	var evs []memory.SessionEvent
	for turn := 1; turn <= 5; turn++ {
		evs = append(evs, memory.SessionEvent{
			AgentID: "a1", Turn: turn, SpanID: "s", Type: "turn_end",
			Payload: json.RawMessage(`{}`),
		})
	}
	if err := store.SessionEventsAppend(evs); err != nil {
		t.Fatal(err)
	}
	before, err := store.SessionEventsByAgent("a1")
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 5 {
		t.Fatalf("appended 5 events, read back %d", len(before))
	}
	maxSeq := before[len(before)-1].Seq

	// Scrub away everything but the last turn — including the row holding maxSeq
	// is not required; what matters is that the high-water mark survives deletes.
	if _, err := store.SessionEventsScrub(1, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.ExecRaw(`DELETE FROM session_events`); err != nil {
		t.Fatal(err)
	}

	if err := store.SessionEventsAppend([]memory.SessionEvent{
		{AgentID: "a1", Turn: 99, SpanID: "s", Type: "turn_end", Payload: json.RawMessage(`{}`)},
	}); err != nil {
		t.Fatal(err)
	}
	after, err := store.SessionEventsByAgent("a1")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 {
		t.Fatalf("after scrub + append, got %d events, want 1", len(after))
	}
	if after[0].Seq <= maxSeq {
		t.Fatalf("seq %d was reused after deleting up to %d; a subscriber cursor past that "+
			"point would never see this event", after[0].Seq, maxSeq)
	}
}

// TestVectorRoundTripPrecision checks the float32 blob encoding survives storage
// exactly, and that ranking still orders by similarity.
func TestVectorRoundTripPrecision(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	near := []float32{1, 0, 0}
	far := []float32{0, 1, 0}
	if err := store.VectorStore("v-near", "ns", "near", near); err != nil {
		t.Fatal(err)
	}
	if err := store.VectorStore("v-far", "ns", "far", far); err != nil {
		t.Fatal(err)
	}
	// A different dimensionality must be excluded, not crash the scan.
	if err := store.VectorStore("v-other", "ns", "other", []float32{1, 0}); err != nil {
		t.Fatal(err)
	}

	hits, err := store.VectorQuery("ns", []float32{1, 0, 0}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2 (the 2-dim vector must be filtered out)", len(hits))
	}
	if hits[0].Key != "near" {
		t.Errorf("best match = %q, want near", hits[0].Key)
	}
	if hits[0].Score < 0.999 {
		t.Errorf("identical vector scored %v, want ~1.0", hits[0].Score)
	}
	if hits[1].Score > 0.001 {
		t.Errorf("orthogonal vector scored %v, want ~0", hits[1].Score)
	}
}

// TestFileStoreStripsNULBytes guards a truncation SQLite performs silently.
// length() and substr() treat a NUL as end-of-value, so a stored NUL would make
// every windowed read past it return short — and a paging loop terminate early —
// with no error. Postgres rejected the insert outright.
func TestFileStoreStripsNULBytes(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FileStore("spill/nul.txt", "before\x00after"); err != nil {
		t.Fatal(err)
	}
	slice, found, err := store.FileFetchRange("spill/nul.txt", 0, 0)
	if err != nil || !found {
		t.Fatalf("fetch: found=%v err=%v", found, err)
	}
	if strings.Contains(slice.Content, "\x00") {
		t.Error("stored content still contains a NUL byte")
	}
	if !strings.Contains(slice.Content, "after") {
		t.Errorf("content = %q, want the text past the NUL preserved", slice.Content)
	}
	if slice.Total != len([]rune(slice.Content)) {
		t.Errorf("Total = %d but content has %d runes; length() truncated at the NUL",
			slice.Total, len([]rune(slice.Content)))
	}
}
