package logsafe

import "strings"
import "testing"

func TestValueStripsLineStructure(t *testing.T) {
	// The attack: end the current record, then write a convincing one.
	forged := "/health\nlevel=INFO msg=\"admin login\" user=root"
	got := Value(forged)
	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("line breaks survived: %q", got)
	}
	if strings.Contains(got, "admin login") == false {
		t.Errorf("content was dropped rather than flattened: %q", got)
	}
}

func TestValueRemovesControlCharacters(t *testing.T) {
	for _, in := range []string{"a\tb", "a\x00b", "a\x1bb", "a\x7fb"} {
		got := Value(in)
		if got != "ab" {
			t.Errorf("Value(%q) = %q, want \"ab\"", in, got)
		}
	}
}

func TestValueKeepsPrintableUnicode(t *testing.T) {
	const in = "/søk/café/日本"
	if got := Value(in); got != in {
		t.Errorf("Value(%q) = %q, want it unchanged", in, got)
	}
}

func TestValueTruncates(t *testing.T) {
	got := Value(strings.Repeat("x", maxLogValueLen*3))
	if len(got) > maxLogValueLen+len("…") {
		t.Errorf("length = %d, want it capped near %d", len(got), maxLogValueLen)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncation is not marked: %q", got[len(got)-10:])
	}
}

func TestValueLeavesOrdinaryValuesAlone(t *testing.T) {
	for _, in := range []string{"/v1/conversations", "GET", "127.0.0.1:54321", ""} {
		if got := Value(in); got != in {
			t.Errorf("Value(%q) = %q, want it unchanged", in, got)
		}
	}
}
