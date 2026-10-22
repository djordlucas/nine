package cron

import (
	"fmt"
	"strings"
	"time"
)

// MinEvery is the shortest fixed interval a cadence may name: a process that
// wakes more often than this spends model calls faster than anything it could
// find would change.
const MinEvery = time.Minute

// ParseCadence reads how often something recurs, as a goal's schedule is
// given: a Go duration ("24h", "90m") is a fixed interval, anything else must
// be a 5-field cron expression ("0 8 * * *"). Exactly one of every and
// schedule is set on success; an empty string is no cadence.
func ParseCadence(s string) (every time.Duration, schedule string, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, "", nil
	}
	if d, derr := time.ParseDuration(s); derr == nil {
		if d < MinEvery {
			return 0, "", fmt.Errorf("cadence %q is shorter than %s", s, MinEvery)
		}
		return d, "", nil
	}
	if _, cerr := Parse(s); cerr != nil {
		return 0, "", fmt.Errorf("cadence %q is neither a duration (24h, 90m) nor a 5-field cron expression: %w", s, cerr)
	}
	return 0, s, nil
}

// FormatEvery writes an interval as a cadence reads it: 24h, 90m, 45s.
func FormatEvery(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	default:
		return fmt.Sprintf("%ds", d/time.Second)
	}
}
