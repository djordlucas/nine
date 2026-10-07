package memory_test

import (
	"testing"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

func openTestStore(t *testing.T) *memory.Store {
	t.Helper()
	s, err := memtest.Open(t)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}
