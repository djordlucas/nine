package runner

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// An arm is a tool-exposition mode a live run compares
// (adr/deferred-tool-schemas.md §7.1). Each case runs once per requested arm,
// with the arm set as a session.config override, so a case that passes in one
// arm and fails in another is a regression the mode caused.
//
// Only ranked exists so far; deferred joins knownArms with the mode itself, so
// a run cannot ask for an arm that would silently run as ranked.

// ArmKey is the session.config key an arm sets, as [context] tool_exposition.
const ArmKey = "context.tool_exposition"

// ArmRanked advertises every tool with its full schema: the current behavior.
const ArmRanked = "ranked"

var knownArms = map[string]bool{ArmRanked: true}

// KnownArms returns the arms a run can request, sorted.
func KnownArms() []string { return slices.Sorted(maps.Keys(knownArms)) }

// ParseArms parses NINE_EVAL_ARMS: comma-separated arm names. Empty means one
// unnamed arm, the configured default, so a plain run costs what it did.
func ParseArms(s string) ([]string, error) {
	var arms []string
	for _, a := range strings.Split(s, ",") {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if !knownArms[a] {
			return nil, fmt.Errorf("unknown arm %q (known: %s)", a, strings.Join(KnownArms(), ", "))
		}
		if slices.Contains(arms, a) {
			return nil, fmt.Errorf("arm %q listed twice", a)
		}
		arms = append(arms, a)
	}
	return arms, nil
}

// PinnedArm returns the arm the case sets in its own session.config, or "".
// A case pins one when it tests that mode's behavior, which the other arm
// does not have.
func (c *Case) PinnedArm() string {
	arm, _ := c.Session.Config[ArmKey].(string)
	return arm
}

// ApplicableArms returns the arms the case runs in: every requested arm, or
// only its pinned one. No requested arms is a single unnamed run, in the
// case's pinned arm if it has one.
func (c *Case) ApplicableArms(requested []string) []string {
	pinned := c.PinnedArm()
	switch {
	case len(requested) == 0:
		return []string{""}
	case pinned == "":
		return requested
	case slices.Contains(requested, pinned):
		return []string{pinned}
	}
	return nil
}

// withArm returns a copy of c that runs in arm; "" leaves c as it is.
func (c *Case) withArm(arm string) *Case {
	if arm == "" || c.PinnedArm() == arm {
		return c
	}
	cp := *c
	cp.Session.Config = maps.Clone(c.Session.Config)
	if cp.Session.Config == nil {
		cp.Session.Config = map[string]any{}
	}
	cp.Session.Config[ArmKey] = arm
	return &cp
}
