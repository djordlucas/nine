package runtime

import (
	"context"
	"fmt"

	"nine/internal/agent"
)

// RunSubAgentSync runs loop with description, blocks until the sub-agent
// produces a final answer, then tears down the runner. It is safe to call
// concurrently from multiple goroutines. sink, when non-nil, journals the
// sub-agent's own execution trajectory under agentID (the sub-agent's ID), so it
// can be surfaced by `nine trace --sub-agents`; pass nil to disable journaling.
func RunSubAgentSync(ctx context.Context, agentID, description string, loop *agent.Loop, sink EventSink) (string, error) {
	r := newAgentWorker(agentID, loop, nil, nil, StallConfig{}, nil, sink)
	result, err := r.turn(ctx, description)
	r.stop()
	return result, err
}

// SubAgentWorker wraps a AgentWorker for a background sub-agent. It runs until the
// agent loop produces a final answer, then posts a notification.
type SubAgentWorker struct {
	worker    *AgentWorker
	notif     NotifStore
	spawnerID string
}

// SpawnSubAgent starts a background sub-agent goroutine and returns its runner.
// loop should already be configured with the sub-agent system prompt and tools.
// When the sub-agent completes, a notification is posted to spawnerID via notif
// (both may be nil to skip notification).
func SpawnSubAgent(ctx context.Context, agentID string, description string, loop *agent.Loop, notif NotifStore, spawnerID string) *SubAgentWorker {
	var saveFn func(string, []byte) error
	var notifFn func(string) ([]string, error)

	sr := &SubAgentWorker{notif: notif, spawnerID: spawnerID}

	r := newAgentWorker(agentID, loop, saveFn, notifFn, StallConfig{}, nil, nil)
	sr.worker = r

	go func() {
		ch := make(chan turnResp, 1)
		select {
		case r.inbox <- turnReq{ctx: ctx, text: description, respCh: ch}:
		case <-ctx.Done():
			return
		case <-r.quit:
			return
		}
		var res turnResp
		select {
		case res = <-ch:
		case <-ctx.Done():
			return
		case <-r.quit:
			return
		}

		if notif != nil && spawnerID != "" {
			msg := fmt.Sprintf("Task completed: %s", res.text)
			if res.err != nil {
				msg = fmt.Sprintf("Task failed: %v", res.err)
			}
			if ms, ok := notif.(*InMemoryNotifStore); ok {
				ms.Add(spawnerID, msg)
			}
		}
		r.stop()
	}()

	return sr
}
