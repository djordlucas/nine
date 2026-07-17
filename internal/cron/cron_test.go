package cron

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, expr string) *Schedule {
	t.Helper()
	s, err := Parse(expr)
	if err != nil {
		t.Fatalf("Parse(%q): %v", expr, err)
	}
	return s
}

func TestParseErrors(t *testing.T) {
	bad := []string{
		"",
		"* * * *",     // 4 fields
		"* * * * * *", // 6 fields
		"60 * * * *",  // minute out of range
		"* 24 * * *",  // hour out of range
		"* * 0 * *",   // dom below range
		"* * * 13 *",  // month out of range
		"* * * * 8",   // dow above range (7 aliases to 0, 8 invalid)
		"5-1 * * * *", // inverted range
		"*/0 * * * *", // zero step
		"a * * * *",   // non-numeric
		"* * */x * *", // bad step
	}
	for _, expr := range bad {
		if _, err := Parse(expr); err == nil {
			t.Errorf("Parse(%q) = nil error, want error", expr)
		}
	}
}

func TestNextBasic(t *testing.T) {
	loc := time.UTC
	cases := []struct {
		expr string
		from time.Time
		want time.Time
	}{
		{
			// Weekdays at 09:00 — from a Saturday, next is Monday 09:00.
			"0 9 * * 1-5",
			time.Date(2026, 7, 4, 12, 0, 0, 0, loc), // Sat
			time.Date(2026, 7, 6, 9, 0, 0, 0, loc),  // Mon
		},
		{
			// Every 15 minutes.
			"*/15 * * * *",
			time.Date(2026, 7, 6, 9, 7, 0, 0, loc),
			time.Date(2026, 7, 6, 9, 15, 0, 0, loc),
		},
		{
			// Top of every hour: from 9:00 exactly, strictly-after ⇒ 10:00.
			"0 * * * *",
			time.Date(2026, 7, 6, 9, 0, 0, 0, loc),
			time.Date(2026, 7, 6, 10, 0, 0, 0, loc),
		},
		{
			// Specific date/time: midnight on the 1st of the month.
			"0 0 1 * *",
			time.Date(2026, 7, 6, 9, 0, 0, 0, loc),
			time.Date(2026, 8, 1, 0, 0, 0, 0, loc),
		},
	}
	for _, tc := range cases {
		got := mustParse(t, tc.expr).Next(tc.from)
		if !got.Equal(tc.want) {
			t.Errorf("Parse(%q).Next(%v) = %v, want %v", tc.expr, tc.from, got, tc.want)
		}
	}
}

// TestNextIsStrictlyAfter ensures Next never returns its input even when the
// input already matches.
func TestNextIsStrictlyAfter(t *testing.T) {
	from := time.Date(2026, 7, 6, 9, 0, 0, 30, time.UTC) // matches "0 9 * * *"
	got := mustParse(t, "0 9 * * *").Next(from)
	if !got.After(from) {
		t.Fatalf("Next(%v) = %v, want strictly after", from, got)
	}
	if got.Hour() != 9 || got.Minute() != 0 {
		t.Errorf("Next = %v, want 09:00 the next day", got)
	}
}

// TestDayOfMonthOrDayOfWeek exercises the Vixie-cron OR rule when both day
// fields are restricted.
func TestDayOfMonthOrDayOfWeek(t *testing.T) {
	// "on the 15th, OR on any Monday".
	s := mustParse(t, "0 0 15 * 1")
	// From July 6 2026 (Mon), next matching midnight is July 13 (next Monday),
	// which comes before the 15th.
	got := s.Next(time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC))
	want := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("Next = %v, want %v (Monday before the 15th)", got, want)
	}
}

func TestSevenIsSunday(t *testing.T) {
	s := mustParse(t, "0 0 * * 7")
	got := s.Next(time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)) // Mon
	if got.Weekday() != time.Sunday {
		t.Errorf("dow=7 should match Sunday; Next = %v (%s)", got, got.Weekday())
	}
}
