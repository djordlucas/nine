package api

import "strings"
import "testing"

func TestSafeLogValueStripsLineStructure(t *testing.T) {
	// The attack: end the current record, then write a convincing one.
	forged := "/health\nlevel=INFO msg=\"admin login\" user=root"
	got := SafeLogValue(forged)
	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("line breaks survived: %q", got)
	}
	if strings.Contains(got, "admin login") == false {
		t.Errorf("content was dropped rather than flattened: %q", got)
	}
}

func TestSafeLogValueRemovesControlCharacters(t *testing.T) {
	for _, in := range []string{"a\tb", "a\x00b", "a\x1bb", "a\x7fb"} {
		got := SafeLogValue(in)
		if got != "ab" {
			t.Errorf("SafeLogValue(%q) = %q, want \"ab\"", in, got)
		}
	}
}

func TestSafeLogValueKeepsPrintableUnicode(t *testing.T) {
	const in = "/søk/café/日本"
	if got := SafeLogValue(in); got != in {
		t.Errorf("SafeLogValue(%q) = %q, want it unchanged", in, got)
	}
}

func TestSafeLogValueTruncates(t *testing.T) {
	got := SafeLogValue(strings.Repeat("x", maxLogValueLen*3))
	if len(got) > maxLogValueLen+len("…") {
		t.Errorf("length = %d, want it capped near %d", len(got), maxLogValueLen)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncation is not marked: %q", got[len(got)-10:])
	}
}

func TestSafeLogValueLeavesOrdinaryValuesAlone(t *testing.T) {
	for _, in := range []string{"/v1/conversations", "GET", "127.0.0.1:54321", ""} {
		if got := SafeLogValue(in); got != in {
			t.Errorf("SafeLogValue(%q) = %q, want it unchanged", in, got)
		}
	}
}
