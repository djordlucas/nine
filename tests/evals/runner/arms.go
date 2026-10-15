package runner

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// An arm is a tool-exposition mode a live run compares
// (adr/deferred-tool-schemas.md §7.1). Each case runs once per requested arm,
// with the arm set as a session.config override, so a case that passes in one
// arm and fails in another is a regression the mode caused.

// ArmKey is the session.config key an arm sets, as [context] tool_exposition.
const ArmKey = "context.tool_exposition"

// ArmRanked advertises the pinned tools and the best-ranked rest with their
// schemas: the default.
const ArmRanked = "ranked"

// ArmDeferred advertises a resident set and lists the other tools by name.
const ArmDeferred = "deferred"

// ArmHybrid is deferred plus the few indexed tools best-ranked against the
// turn's request, advertised with their schemas.
const ArmHybrid = "hybrid"

var knownArms = map[string]bool{ArmRanked: true, ArmDeferred: true, ArmHybrid: true}

// sizeKeys are the session.config keys an arm's size sets: "ranked:10" runs
// ranked with tool_top_n 10, "hybrid:5" hybrid with hybrid_picks 5. Deferred
// has no size.
var sizeKeys = map[string]string{ArmRanked: "context.tool_top_n", ArmHybrid: "context.hybrid_picks"}

// splitArm splits an arm into its mode and its size, 0 when it has none.
func splitArm(arm string) (mode string, size int) {
	mode, n, ok := strings.Cut(arm, ":")
	if !ok {
		return arm, 0
	}
	size, _ = strconv.Atoi(n)
	return mode, size
}

// KnownArms returns the arms a run can request, sorted.
func KnownArms() []string { return slices.Sorted(maps.Keys(knownArms)) }

// ParseArms parses NINE_EVAL_ARMS: comma-separated arm names, each a mode with
// an optional size ("ranked:10"). Empty means one unnamed arm, the configured
// default, so a plain run costs what it did.
func ParseArms(s string) ([]string, error) {
	var arms []string
	for _, a := range strings.Split(s, ",") {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		mode, n, sized := strings.Cut(a, ":")
		if !knownArms[mode] {
			return nil, fmt.Errorf("unknown arm %q (known: %s)", a, strings.Join(KnownArms(), ", "))
		}
		if sized {
			if sizeKeys[mode] == "" {
				return nil, fmt.Errorf("arm %q: %s takes no size", a, mode)
			}
			if size, err := strconv.Atoi(n); err != nil || size <= 0 {
				return nil, fmt.Errorf("arm %q: the size must be a positive integer", a)
			}
		}
		if slices.Contains(arms, a) {
			return nil, fmt.Errorf("arm %q listed twice", a)
		}
		arms = append(arms, a)
	}
	return arms, nil
}

// PinnedArms returns the arms the case sets in its own session.config: one
// name, or a list of them for a case that tests behavior several modes share
// (deferred and hybrid both index). A case pins arms when it tests behavior
// the others do not have.
func (c *Case) PinnedArms() []string {
	switch v := c.Session.Config[ArmKey].(type) {
	case string:
		return []string{v}
	case []any:
		var arms []string
		for _, a := range v {
			if s, ok := a.(string); ok {
				arms = append(arms, s)
			}
		}
		return arms
	case []string:
		return v
	}
	return nil
}

// PinnedArm returns the first arm the case pins, or "": the one it runs in
// when no arms are requested.
func (c *Case) PinnedArm() string {
	if arms := c.PinnedArms(); len(arms) > 0 {
		return arms[0]
	}
	return ""
}

// ApplicableArms returns the arms the case runs in: every requested arm, or
// only the requested ones whose mode it pins. No requested arms is a single unnamed run,
// in the case's first pinned arm if it has one.
func (c *Case) ApplicableArms(requested []string) []string {
	pinned := c.PinnedArms()
	switch {
	case len(requested) == 0:
		return []string{""}
	case len(pinned) == 0:
		return requested
	}
	var arms []string
	for _, a := range requested {
		if mode, _ := splitArm(a); slices.Contains(pinned, mode) {
			arms = append(arms, a)
		}
	}
	return arms
}

// withArm returns a copy of c that runs in arm, its size set when it has one;
// "" runs it in its first pinned arm, or leaves it as it is.
func (c *Case) withArm(arm string) *Case {
	if arm == "" {
		arm = c.PinnedArm()
	}
	mode, size := splitArm(arm)
	if mode == "" || (c.Session.Config[ArmKey] == mode && size == 0) {
		return c
	}
	cp := *c
	cp.Session.Config = maps.Clone(c.Session.Config)
	if cp.Session.Config == nil {
		cp.Session.Config = map[string]any{}
	}
	cp.Session.Config[ArmKey] = mode
	if size > 0 {
		cp.Session.Config[sizeKeys[mode]] = size
	}
	return &cp
}
