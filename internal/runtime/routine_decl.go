package runtime

import (
	"errors"
	"fmt"
	"time"
)

// RoutineDecl is one additional routine requested on a standing agent's
// session, resolved from an [[agent.routine]] entry. It becomes a process
// attached to the agent's session (adr/process-sessions.md §4), so its turns
// run there, with the agent's history and under the agent's role. Exactly one
// of Interval or Schedule is set.
type RoutineDecl struct {
	Kind     string
	Interval time.Duration
	Schedule string
}

// routineTools maps a routine kind to the shipped process that runs it.
var routineTools = map[string]string{
	"idle-reflection": "reflect",
}

// ValidateRoutineDecls checks operator-declared routines before they become
// processes. Every failure here is one that would otherwise be silent: an
// unknown kind has no process to run it, a missing cadence never fires, and a
// duplicate kind would be two processes under one name.
func ValidateRoutineDecls(routines []RoutineDecl) error {
	seen := map[string]bool{}
	for _, a := range routines {
		if a.Kind == "" {
			return errors.New("routine has no kind")
		}
		// Checked before the lookup so the specific reason wins.
		if a.Kind == "pursue" {
			return errors.New(`routine "pursue" duplicates the session's own pursue process`)
		}
		if _, ok := routineTools[a.Kind]; !ok {
			return fmt.Errorf("routine %q is not a known routine kind", a.Kind)
		}
		if seen[a.Kind] {
			return fmt.Errorf("routine %q declared twice", a.Kind)
		}
		seen[a.Kind] = true
		if a.Interval <= 0 && a.Schedule == "" {
			return fmt.Errorf("routine %q has neither interval nor schedule, so it would never wake", a.Kind)
		}
		if a.Interval > 0 && a.Schedule != "" {
			return fmt.Errorf("routine %q sets both interval and schedule; they are mutually exclusive", a.Kind)
		}
	}
	return nil
}
