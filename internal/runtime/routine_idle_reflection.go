package runtime

import (
	"context"
	"encoding/json"
)

// idleReflectionRoutine implements the "idle-reflection" RoutineHandler: OnIdle
// always has work — the reflection prompt — and the turn's outcome is recorded
// by the journal like any other turn's.
//
// It is a routine kind, not a session kind: any session may carry it as a routine
// beside its own work (docs/concept-consolidation.md C3/C4).
type idleReflectionRoutine struct{}

// NewIdleReflectionRoutine creates the "idle-reflection" RoutineHandler.
func NewIdleReflectionRoutine() RoutineHandler {
	return &idleReflectionRoutine{}
}

func (s *idleReflectionRoutine) Init(context.Context, string, json.RawMessage) error { return nil }

// OnTurnEnd does nothing. A reflection turn used to be copied into a dedicated
// `reflections` table, which recorded no agent id — survivable while exactly one
// session reflected, and wrong as soon as several can. The journal already
// records every turn under its own agent_id (`turn_end.result`), and the durable
// product of a reflection is the `self/*` KV write the prompt asks for, not the
// transcript. So there is nothing left for this hook to do
// (docs/concept-consolidation.md C5).
func (s *idleReflectionRoutine) OnTurnEnd(context.Context, string, string, error) error { return nil }

// OnIdle always has work to do: prompt the session to reflect on its recent
// activity and update its self-model.
func (s *idleReflectionRoutine) OnIdle(context.Context, string) (string, bool) {
	return ReflectionPrompt, true
}
