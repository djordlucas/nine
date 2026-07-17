// Package cron parses standard 5-field cron expressions and computes the next
// firing time. It is intentionally small — just enough to schedule pre-defined
// agents (docs/predefined-agents.md, docs/scheduling.md) — and has no external
// dependencies.
//
// Fields, in order: minute (0-59), hour (0-23), day-of-month (1-31),
// month (1-12), day-of-week (0-6, Sunday=0; 7 is also accepted for Sunday).
// Each field supports "*", single values, ranges "a-b", lists "a,b,c", and
// steps "*/n" or "a-b/n". Month and day-of-week names are not supported.
//
// Day-of-month / day-of-week follow the common Vixie-cron rule: when both are
// restricted (neither is "*"), a day matches if EITHER field matches; when one
// is "*", the other is ANDed normally.
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed cron expression. The zero value is not usable; obtain one
// from Parse.
type Schedule struct {
	minute uint64 // bit i set ⇒ minute i matches (0-59)
	hour   uint64 // 0-23
	dom    uint64 // 1-31
	month  uint64 // 1-12
	dow    uint64 // 0-6 (Sunday=0)

	domRestricted bool // day-of-month field was not "*"
	dowRestricted bool // day-of-week field was not "*"
}

type fieldSpec struct {
	name     string
	min, max int
}

var fields = []fieldSpec{
	{"minute", 0, 59},
	{"hour", 0, 23},
	{"day-of-month", 1, 31},
	{"month", 1, 12},
	{"day-of-week", 0, 6},
}

// Parse compiles a 5-field cron expression. It returns an error for the wrong
// field count or any field that is out of range or malformed.
func Parse(expr string) (*Schedule, error) {
	parts := strings.Fields(strings.TrimSpace(expr))
	if len(parts) != 5 {
		return nil, fmt.Errorf("cron: expected 5 fields, got %d in %q", len(parts), expr)
	}

	masks := make([]uint64, 5)
	restricted := make([]bool, 5)
	for i, spec := range fields {
		raw := parts[i]
		// day-of-week: accept 7 as an alias for Sunday(0) before parsing.
		if i == 4 {
			raw = normalizeDOW(raw)
		}
		mask, err := parseField(raw, spec.min, spec.max)
		if err != nil {
			return nil, fmt.Errorf("cron: %s field: %w", spec.name, err)
		}
		masks[i] = mask
		restricted[i] = raw != "*"
	}

	return &Schedule{
		minute:        masks[0],
		hour:          masks[1],
		dom:           masks[2],
		month:         masks[3],
		dow:           masks[4],
		domRestricted: restricted[2],
		dowRestricted: restricted[4],
	}, nil
}

// normalizeDOW rewrites the day-of-week token so a standalone 7 (or 7 inside a
// range/list/step) is treated as Sunday (0). Only exact "7" boundaries are
// rewritten to avoid corrupting multi-digit numbers (none valid here anyway).
func normalizeDOW(raw string) string {
	replaceToken := func(tok string) string {
		if tok == "7" {
			return "0"
		}
		return tok
	}
	// Handle lists and ranges token by token.
	var out []string
	for item := range strings.SplitSeq(raw, ",") {
		if strings.Contains(item, "-") {
			bounds := strings.SplitN(item, "-", 2)
			// A step suffix may hang off the upper bound (a-b/n).
			hi := bounds[1]
			step := ""
			if idx := strings.Index(hi, "/"); idx >= 0 {
				step = hi[idx:]
				hi = hi[:idx]
			}
			out = append(out, replaceToken(bounds[0])+"-"+replaceToken(hi)+step)
			continue
		}
		out = append(out, replaceToken(item))
	}
	return strings.Join(out, ",")
}

// parseField parses one cron field into a bitmask over [min,max].
func parseField(raw string, min, max int) (uint64, error) {
	var mask uint64
	for item := range strings.SplitSeq(raw, ",") {
		if item == "" {
			return 0, fmt.Errorf("empty term in %q", raw)
		}
		lo, hi, step, err := parseTerm(item, min, max)
		if err != nil {
			return 0, err
		}
		for v := lo; v <= hi; v += step {
			mask |= uint64(1) << v
		}
	}
	return mask, nil
}

// parseTerm parses a single comma-free term: "*", "*/n", "a", "a-b", "a-b/n".
func parseTerm(item string, min, max int) (lo, hi, step int, err error) {
	step = 1
	if idx := strings.Index(item, "/"); idx >= 0 {
		stepStr := item[idx+1:]
		item = item[:idx]
		s, e := strconv.Atoi(stepStr)
		if e != nil || s <= 0 {
			return 0, 0, 0, fmt.Errorf("invalid step %q", stepStr)
		}
		step = s
	}

	switch {
	case item == "*":
		lo, hi = min, max
	case strings.Contains(item, "-"):
		bounds := strings.SplitN(item, "-", 2)
		lo, err = atoiRange(bounds[0], min, max)
		if err != nil {
			return 0, 0, 0, err
		}
		hi, err = atoiRange(bounds[1], min, max)
		if err != nil {
			return 0, 0, 0, err
		}
		if lo > hi {
			return 0, 0, 0, fmt.Errorf("range %q is inverted", item)
		}
	default:
		lo, err = atoiRange(item, min, max)
		if err != nil {
			return 0, 0, 0, err
		}
		hi = lo
	}
	return lo, hi, step, nil
}

func atoiRange(s string, min, max int) (int, error) {
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid value %q", s)
	}
	if v < min || v > max {
		return 0, fmt.Errorf("value %d out of range [%d,%d]", v, min, max)
	}
	return v, nil
}

// Next returns the earliest time strictly after `after` that matches the
// schedule, computed in after's location and truncated to the minute. If no
// match is found within five years it returns the zero Time (a malformed
// schedule that can never fire, e.g. Feb 30).
func (s *Schedule) Next(after time.Time) time.Time {
	// Start from the top of the next minute.
	t := after.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		if !bitSet(s.month, int(t.Month())) {
			// Jump to the first day of the next month.
			t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location()).AddDate(0, 1, 0)
			continue
		}
		if !s.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()).AddDate(0, 0, 1)
			continue
		}
		if !bitSet(s.hour, t.Hour()) {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, t.Location()).Add(time.Hour)
			continue
		}
		if !bitSet(s.minute, t.Minute()) {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}

// dayMatches applies the Vixie-cron day-of-month / day-of-week rule.
func (s *Schedule) dayMatches(t time.Time) bool {
	domOK := bitSet(s.dom, t.Day())
	dowOK := bitSet(s.dow, int(t.Weekday()))
	switch {
	case s.domRestricted && s.dowRestricted:
		return domOK || dowOK
	case s.domRestricted:
		return domOK
	case s.dowRestricted:
		return dowOK
	default:
		return true
	}
}

func bitSet(mask uint64, i int) bool { return mask&(uint64(1)<<i) != 0 }
