package memory_test

import (
	"fmt"
	"testing"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

// Every rewrite keeps the version it replaces, with its process definition;
// the newest MaxToolVersions are kept; deleting the tool deletes them.
func TestToolVersions(t *testing.T) {
	s, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	write := func(src, prev string, probation bool) int {
		t.Helper()
		v, err := s.GeneratedToolWrite(memory.GeneratedTool{Name: "digest", Source: src, Live: true,
			Probation: probation, WrittenBy: "conv-1"}, prev)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if v := write("v1", "", false); v != 1 {
		t.Fatalf("first write is version %d", v)
	}
	if v := write("v2", `{"id":"gen:digest","every":"1h"}`, true); v != 2 {
		t.Fatalf("second write is version %d", v)
	}
	cur, _, _ := s.GeneratedToolGet("digest")
	if cur.Version != 2 || !cur.Probation || cur.Source != "v2" || cur.WrittenBy != "conv-1" {
		t.Errorf("current = %+v", cur)
	}
	prev, ok, err := s.GeneratedToolVersion("digest", 1)
	if err != nil || !ok || prev.Source != "v1" || prev.Process != `{"id":"gen:digest","every":"1h"}` {
		t.Errorf("version 1 = %+v, %v, %v", prev, ok, err)
	}
	if err := s.GeneratedToolProve("digest"); err != nil {
		t.Fatal(err)
	}
	if cur, _, _ = s.GeneratedToolGet("digest"); cur.Probation {
		t.Error("probation not cleared")
	}

	for i := 3; i <= 15; i++ {
		write(fmt.Sprintf("v%d", i), "", false)
	}
	vs, err := s.GeneratedToolVersions("digest")
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != memory.MaxToolVersions || vs[0].Version != 14 || vs[len(vs)-1].Version != 5 {
		t.Errorf("kept %d versions, %d..%d; want the newest %d", len(vs), vs[0].Version, vs[len(vs)-1].Version, memory.MaxToolVersions)
	}

	if err := s.GeneratedToolDelete("digest"); err != nil {
		t.Fatal(err)
	}
	if vs, _ := s.GeneratedToolVersions("digest"); len(vs) != 0 {
		t.Errorf("%d versions outlived the tool", len(vs))
	}
}
