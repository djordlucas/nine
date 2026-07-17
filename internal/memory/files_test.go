package memory_test

import (
	"strings"
	"testing"

	"nine/internal/memory/memtest"
)

func TestFileSearchText(t *testing.T) {
	s, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FileStore("notes/db.md", "PostgreSQL is a powerful relational database engine"); err != nil {
		t.Fatal(err)
	}
	if err := s.FileStore("notes/cats.md", "cats are small furry animals"); err != nil {
		t.Fatal(err)
	}

	res, err := s.FileSearchText("database", 10)
	if err != nil {
		t.Fatalf("FileSearchText: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("got %d results, want 1: %+v", len(res), res)
	}
	if res[0].Path != "notes/db.md" {
		t.Errorf("hit path = %q, want notes/db.md", res[0].Path)
	}
	if !strings.Contains(res[0].Snippet, "[") || !strings.Contains(res[0].Snippet, "]") {
		t.Errorf("snippet not highlighted: %q", res[0].Snippet)
	}

	// Updating content re-derives the generated tsvector column.
	if err := s.FileStore("notes/cats.md", "cats enjoy a good database of napping spots"); err != nil {
		t.Fatal(err)
	}
	res, err = s.FileSearchText("database", 10)
	if err != nil {
		t.Fatalf("FileSearchText after update: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("after update got %d results, want 2", len(res))
	}
}
