package runtime

import (
	"context"
	"encoding/json"

	"nine/internal/memory"
)

// idleReflectionStage implements the "idle-reflection" StageHandler (see
// docs/session-plans.md, "Pilot: idle-reflection as a dedicated session"):
// OnIdle always has work — the reflection prompt — and OnTurnEnd records
// each reflection turn's result in the reflections table.
type idleReflectionStage struct {
	store *memory.Store
}

// NewIdleReflectionStage creates the "idle-reflection" StageHandler.
func NewIdleReflectionStage(store *memory.Store) StageHandler {
	return &idleReflectionStage{store: store}
}

func (s *idleReflectionStage) Init(context.Context, string, json.RawMessage) error { return nil }

// OnTurnEnd records a successful reflection turn's result. Errors (including
// ErrStall) are ignored — a failed or stalled reflection just tries again
// next idle interval.
func (s *idleReflectionStage) OnTurnEnd(_ context.Context, _ string, result string, err error) error {
	if err != nil || result == "" {
		return nil
	}
	return s.store.ReflectionCreate(newUUID(), result)
}

// OnIdle always has work to do: prompt the session to reflect on its recent
// activity and update its self-model.
func (s *idleReflectionStage) OnIdle(context.Context, string) (string, bool) {
	return ReflectionPrompt, true
}
