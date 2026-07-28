package memory_test

import (
	"strings"
	"testing"
	"time"

	"nine/internal/memory/memtest"
)

func TestFileFetchRangeWindows(t *testing.T) {
	s, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	const content = "0123456789abcdefghij" // 20 chars
	if err := s.FileStore("big.txt", content); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name          string
		offset, limit int
		want          string
		wantChars     int
	}{
		{"head", 0, 5, "01234", 5},
		{"middle", 10, 5, "abcde", 5},
		{"limit past end clamps", 15, 100, "fghij", 5},
		{"limit zero reads to end", 12, 0, "cdefghij", 8},
		{"negative offset clamps to 0", -7, 4, "0123", 4},
		{"offset past end is empty, not an error", 500, 10, "", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, found, err := s.FileFetchRange("big.txt", tc.offset, tc.limit)
			if err != nil {
				t.Fatalf("FileFetchRange: %v", err)
			}
			if !found {
				t.Fatal("file not found")
			}
			if got.Content != tc.want {
				t.Errorf("content = %q, want %q", got.Content, tc.want)
			}
			if got.Chars != tc.wantChars {
				t.Errorf("chars = %d, want %d", got.Chars, tc.wantChars)
			}
			if got.Total != len(content) {
				t.Errorf("total = %d, want %d", got.Total, len(content))
			}
		})
	}
}

// Paging the whole file back one window at a time must reconstruct it exactly,
// which is what an agent reading a large spill actually does.
func TestFileFetchRangePagingReassembles(t *testing.T) {
	s, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	content := strings.Repeat("abcdefghij", 500) // 5000 chars
	if err := s.FileStore("spill/a1/tool-01.txt", content); err != nil {
		t.Fatal(err)
	}

	var b strings.Builder
	for offset := 0; ; offset += 700 {
		slice, found, err := s.FileFetchRange("spill/a1/tool-01.txt", offset, 700)
		if err != nil {
			t.Fatalf("FileFetchRange at %d: %v", offset, err)
		}
		if !found {
			t.Fatal("file not found")
		}
		if slice.Chars == 0 {
			break
		}
		b.WriteString(slice.Content)
	}
	if b.String() != content {
		t.Errorf("paged read reassembled %d chars, want %d", b.Len(), len(content))
	}
}

// Offsets are in characters, so a window can never split a multi-byte rune.
func TestFileFetchRangeIsCharacterOriented(t *testing.T) {
	s, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	const content = "héllo wörld" // 11 chars, 13 bytes
	if err := s.FileStore("uni.txt", content); err != nil {
		t.Fatal(err)
	}

	got, found, err := s.FileFetchRange("uni.txt", 0, 5)
	if err != nil || !found {
		t.Fatalf("FileFetchRange: %v found=%v", err, found)
	}
	if got.Content != "héllo" {
		t.Errorf("content = %q, want %q (character-oriented, not byte)", got.Content, "héllo")
	}
	if got.Total != 11 {
		t.Errorf("total = %d, want 11 characters", got.Total)
	}
}

func TestFileFetchRangeMissingFile(t *testing.T) {
	s, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	_, found, err := s.FileFetchRange("nope.txt", 0, 10)
	if err != nil {
		t.Fatalf("FileFetchRange: %v", err)
	}
	if found {
		t.Error("found = true for a missing path")
	}
}

func TestFileSearchTextScoped(t *testing.T) {
	s, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		"spill/a1/http-01.txt": "the deployment failed with a timeout error",
		"spill/a2/http-02.txt": "another deployment succeeded cleanly",
		"notes/deploy.md":      "deployment notes for the platform",
	} {
		if err := s.FileStore(path, body); err != nil {
			t.Fatal(err)
		}
	}

	all, err := s.FileSearchTextScoped("deployment", "", 10)
	if err != nil {
		t.Fatalf("scoped search (all): %v", err)
	}
	if len(all) != 3 {
		t.Errorf("unscoped search found %d, want 3", len(all))
	}

	// A directory prefix narrows to a session's spills.
	scoped, err := s.FileSearchTextScoped("deployment", "spill/", 10)
	if err != nil {
		t.Fatalf("scoped search (prefix): %v", err)
	}
	if len(scoped) != 2 {
		t.Errorf("prefix-scoped search found %d, want 2", len(scoped))
	}

	// An exact path narrows to one large output — the spill-reading case.
	one, err := s.FileSearchTextScoped("timeout", "spill/a1/http-01.txt", 10)
	if err != nil {
		t.Fatalf("scoped search (exact): %v", err)
	}
	if len(one) != 1 || one[0].Path != "spill/a1/http-01.txt" {
		t.Errorf("exact-path search = %+v, want the single a1 hit", one)
	}

	// The scope must exclude, not merely rank.
	none, err := s.FileSearchTextScoped("timeout", "notes/", 10)
	if err != nil {
		t.Fatalf("scoped search (excluding): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("search outside the scope returned %+v, want none", none)
	}
}

func TestFileSearchTextJSONScoped(t *testing.T) {
	s, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FileStore("spill/a1/x.txt", "a rare pangolin sighting"); err != nil {
		t.Fatal(err)
	}
	if err := s.FileStore("notes/y.md", "another rare pangolin"); err != nil {
		t.Fatal(err)
	}

	out, err := s.FileSearchTextJSON("pangolin", "spill/", 10)
	if err != nil {
		t.Fatalf("FileSearchTextJSON: %v", err)
	}
	if !strings.Contains(out, "spill/a1/x.txt") || strings.Contains(out, "notes/y.md") {
		t.Errorf("scoped JSON search = %s, want only the spill hit", out)
	}
}

func TestFileDeleteOlderThan(t *testing.T) {
	s, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FileStore("spill/a1/fresh.txt", "keep me"); err != nil {
		t.Fatal(err)
	}
	if err := s.FileStore("notes/keep.md", "not a spill"); err != nil {
		t.Fatal(err)
	}

	// Nothing is old enough yet.
	n, err := s.FileDeleteOlderThan("spill/", time.Hour)
	if err != nil {
		t.Fatalf("FileDeleteOlderThan: %v", err)
	}
	if n != 0 {
		t.Errorf("deleted %d fresh files, want 0", n)
	}

	// A negative-age sweep would be a footgun; a zero/negative age is rejected
	// rather than deleting everything.
	if _, err := s.FileDeleteOlderThan("spill/", 0); err == nil {
		t.Error("expected an error for a non-positive age")
	}
	// An empty prefix must never be able to clear the store.
	if _, err := s.FileDeleteOlderThan("", time.Hour); err == nil {
		t.Error("expected an error for an empty prefix")
	}

	// With an effectively zero cutoff, the spill goes and the note stays.
	n, err = s.FileDeleteOlderThan("spill/", time.Nanosecond)
	if err != nil {
		t.Fatalf("FileDeleteOlderThan: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted %d, want 1", n)
	}
	if _, found, _ := s.FileFetch("notes/keep.md"); !found {
		t.Error("the sweep deleted a file outside the prefix")
	}
}
