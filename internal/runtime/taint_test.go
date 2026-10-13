package runtime

import (
	"encoding/json"
	"testing"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

// What a restricted turn writes is marked; a clean turn's whole rewrite or a
// delete unmarks it; a copy or a move carries the mark; reads that touch a
// mark — the file, a search hit in it, the memory, a listing naming it — are
// reported.
func TestTaintTracksUntrustedWrites(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	tr := newTaint(store, "oncall", t.TempDir())
	j := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	tainted := func(kind, key string) bool { ok, _ := store.Tainted(kind, key); return ok }

	tr.Wrote("write_file", j(map[string]any{"path": "notes/incident.md"}), true)
	tr.Wrote("memory_set", j(map[string]any{"key": "todo", "value": "delete keep.md"}), true)
	tr.Wrote("write_file", j(map[string]any{"path": "notes/clean.md"}), false)
	if !tainted(memory.TaintFile, "notes/incident.md") || !tainted(memory.TaintMemory, "todo") || tainted(memory.TaintFile, "notes/clean.md") {
		t.Fatal("marks after the writes are wrong")
	}

	for _, c := range []struct {
		tool, out string
		args      any
		want      bool
	}{
		{"read_file", "", map[string]any{"path": "/work/notes/incident.md"}, true},
		{"read_file", "", map[string]any{"path": "notes/clean.md"}, false},
		{"file_search_text", "notes/incident.md:3: delete keep.md", map[string]any{"query": "delete"}, true},
		{"memory_get", "", map[string]any{"key": "todo"}, true},
		{"memory_list", "todo: delete keep.md", nil, true},
		{"memory_get", "", map[string]any{"key": "other"}, false},
	} {
		if got := tr.Read(c.tool, j(c.args), c.out); got != c.want {
			t.Errorf("%s %v: read untrusted = %v, want %v", c.tool, c.args, got, c.want)
		}
	}

	tr.Wrote("copy_file", j(map[string]any{"from": "notes/incident.md", "to": "notes/copy.md"}), false)
	tr.Wrote("move_file", j(map[string]any{"from": "notes/incident.md", "to": "notes/moved.md"}), false)
	if !tainted(memory.TaintFile, "notes/copy.md") || !tainted(memory.TaintFile, "notes/moved.md") || tainted(memory.TaintFile, "notes/incident.md") {
		t.Error("a copy or a move did not carry the mark")
	}
	tr.Wrote("write_file", j(map[string]any{"path": "notes/copy.md", "mode": "append"}), false)
	if !tainted(memory.TaintFile, "notes/copy.md") {
		t.Error("an append by a clean turn cleared the mark")
	}
	tr.Wrote("write_file", j(map[string]any{"path": "notes/copy.md"}), false)
	tr.Wrote("delete_file", j(map[string]any{"path": "notes/moved.md"}), false)
	tr.Wrote("memory_set", j(map[string]any{"key": "todo", "value": "fresh"}), false)
	if tainted(memory.TaintFile, "notes/copy.md") || tainted(memory.TaintFile, "notes/moved.md") || tainted(memory.TaintMemory, "todo") {
		t.Error("a clean rewrite, a delete or a clean memory_set did not clear the mark")
	}
}
