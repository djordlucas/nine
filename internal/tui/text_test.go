package tui

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The TUI carried the same defect as the CLI in ten places: a fixed byte offset
// cuts free text mid-rune and emits invalid UTF-8. Descriptions, names, and step
// labels are model- or user-authored, so non-ASCII is ordinary input.
func TestClipNeverProducesInvalidUTF8(t *testing.T) {
	inputs := []string{
		"日本語のゴールの説明でありこれは非常に長いテキストです",
		"Überwachung — Sicherheitsprüfung für alle Abhängigkeiten",
		"🎯 monitor the repository for security issues and triage CVEs",
		strings.Repeat("é", 200),
	}
	for _, in := range inputs {
		for _, max := range []int{1, 2, 8, 12, 38, 48, 49, 61, 73} {
			got := clip(in, max)
			if !utf8.ValidString(got) {
				t.Errorf("clip(%q, %d) = %q, not valid UTF-8", in, max, got)
			}
			if n := utf8.RuneCountInString(got); n > max {
				t.Errorf("clip(%q, %d) returned %d runes", in, max, n)
			}
		}
	}
}

func TestClipKeepsShortStringsWhole(t *testing.T) {
	for _, s := range []string{"", "short", "日本語"} {
		if got := clip(s, 40); got != s {
			t.Errorf("clip(%q, 40) = %q, want unchanged", s, got)
		}
	}
}

// The TUI marks truncation with a single-rune ellipsis, not "..." — keeping that
// exact is what makes this a fix rather than a visual change.
func TestClipUsesSingleRuneEllipsis(t *testing.T) {
	got := clip(strings.Repeat("a", 100), 20)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("clip = %q, want the … ellipsis the TUI has always used", got)
	}
	if strings.HasSuffix(got, "...") {
		t.Errorf("clip = %q, want one ellipsis rune, not three dots", got)
	}
}

// `id[:8]` panicked on a shorter id. Ids arrive from the daemon, so a malformed
// one took the whole TUI down.
func TestShortIDDoesNotPanic(t *testing.T) {
	for _, s := range []string{"", "a", "1234567", "12345678", "123456789", "日本語"} {
		got := shortID(s, 8)
		if utf8.RuneCountInString(got) > 8 {
			t.Errorf("shortID(%q, 8) = %q, too long", s, got)
		}
		if !utf8.ValidString(got) {
			t.Errorf("shortID(%q, 8) = %q, not valid UTF-8", s, got)
		}
	}
}
