package agent

import "testing"

// maybeWarnContext fires a notice when usage crosses contextWarnFraction of the
// budget and re-arms only after usage falls back below it, so a session that
// repeatedly brushes the ceiling is warned on each upward crossing.
func TestMaybeWarnContext(t *testing.T) {
	var l Loop
	var notices []string
	l.onNotice = func(s string) { notices = append(notices, s) }

	budget := 1000
	warn := int(contextWarnFraction*float64(budget)) + 1 // safely over the threshold
	below := budget / 2                                  // safely under

	// Below threshold: silent.
	l.maybeWarnContext(below, budget)
	if len(notices) != 0 {
		t.Fatalf("below threshold fired %d notices, want 0", len(notices))
	}

	// Cross up: one notice, then latched silent while it stays high.
	l.maybeWarnContext(warn, budget)
	l.maybeWarnContext(warn, budget)
	if len(notices) != 1 {
		t.Fatalf("staying above threshold fired %d notices, want 1 (latched)", len(notices))
	}

	// Drop below re-arms; crossing up again warns a second time.
	l.maybeWarnContext(below, budget)
	l.maybeWarnContext(warn, budget)
	if len(notices) != 2 {
		t.Fatalf("re-crossing fired %d notices, want 2", len(notices))
	}

	// A zero budget must never warn or divide by zero.
	l.contextWarnLatched = false
	l.maybeWarnContext(0, 0)
	if len(notices) != 2 {
		t.Fatalf("zero budget fired a notice, want none")
	}
}
