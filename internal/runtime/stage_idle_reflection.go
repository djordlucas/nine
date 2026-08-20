package runtime

import (
	"context"
	"encoding/json"
)

// idleReflectionStage implements the "idle-reflection" StageHandler: OnIdle
// always has work — the reflection prompt — and the turn's outcome is recorded
// by the journal like any other turn's.
//
// It is a stage kind, not a session kind: any session may carry it as an aspect
// beside its own work (docs/concept-consolidation.md C3/C4).
type idleReflectionStage struct{}

// NewIdleReflectionStage creates the "idle-reflection" StageHandler.
func NewIdleReflectionStage() StageHandler {
	return &idleReflectionStage{}
}

func (s *idleReflectionStage) Init(context.Context, string, json.RawMessage) error { return nil }

// OnTurnEnd does nothing. A reflection turn used to be copied into a dedicated
// `reflections` table, which recorded no agent id — survivable while exactly one
// session reflected, and wrong as soon as several can. The journal already
// records every turn under its own agent_id (`turn_end.result`), and the durable
// product of a reflection is the `self/*` KV write the prompt asks for, not the
// transcript. So there is nothing left for this hook to do
// (docs/concept-consolidation.md C5).
func (s *idleReflectionStage) OnTurnEnd(context.Context, string, string, error) error { return nil }

// OnIdle always has work to do: prompt the session to reflect on its recent
// activity and update its self-model.
func (s *idleReflectionStage) OnIdle(context.Context, string) (string, bool) {
	return ReflectionPrompt, true
}
