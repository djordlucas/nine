package runtime

import (
	"context"
	"fmt"

	"nine/internal/agent"
	"nine/internal/protocol"
)

// RunSubAgentSync runs loop with description, blocks until the sub-agent
// produces a final answer, then tears down the runner. It is safe to call
// concurrently from multiple goroutines. sink, when non-nil, journals the
// sub-agent's own execution trajectory under agentID (the sub-agent's ID), so it
// can be surfaced by `nine trace --sub-agents`; pass nil to disable journaling.
// onProgress, when non-nil, is called for each tool/thinking event emitted by the
// sub-agent, so the parent can forward them to its own progress stream.
func RunSubAgentSync(ctx context.Context, agentID, description string, loop *agent.Loop, sink EventSink, onProgress func(protocol.Msg)) (string, error) {
	log.Debug("RunSubAgentSync", "agentID", agentID, "description", description)
	r := newAgentWorker(agentID, loop, nil, nil, StallConfig{}, nil, sink)
	if onProgress != nil {
		r.setProgress(onProgress)
	}
	result, err := r.turn(ctx, description)
	r.stop()
	if err != nil {
		log.Warn("RunSubAgentSync error", "agentID", agentID, "err", err)
	} else {
		log.Debug("RunSubAgentSync completed", "agentID", agentID, "result_len", len(result))
	}
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
	log.Debug("SpawnSubAgent", "agentID", agentID, "description", description, "spawnerID", spawnerID)
	var saveFn func(string, []byte) error
	var notifFn func(string) ([]string, error)

	sr := &SubAgentWorker{notif: notif, spawnerID: spawnerID}

	r := newAgentWorker(agentID, loop, saveFn, notifFn, StallConfig{}, nil, nil)
	sr.worker = r

	go func() {
		log.Debug("SpawnSubAgent goroutine started", "agentID", agentID)
		ch := make(chan turnResp, 1)
		select {
		case r.inbox <- turnReq{ctx: ctx, text: description, respCh: ch}:
			log.Debug("SpawnSubAgent sent turn request", "agentID", agentID)
		case <-ctx.Done():
			log.Debug("SpawnSubAgent context done before send", "agentID", agentID)
			return
		case <-r.quit:
			log.Debug("SpawnSubAgent quit before send", "agentID", agentID)
			return
		}
		var res turnResp
		select {
		case res = <-ch:
			log.Debug("SpawnSubAgent got response", "agentID", agentID)
		case <-ctx.Done():
			log.Debug("SpawnSubAgent context done waiting for response", "agentID", agentID)
			return
		case <-r.quit:
			log.Debug("SpawnSubAgent quit waiting for response", "agentID", agentID)
			return
		}

		if res.err != nil {
			log.Warn("SpawnSubAgent sub-agent error", "agentID", agentID, "err", res.err)
		} else {
			log.Debug("SpawnSubAgent sub-agent completed", "agentID", agentID, "result_len", len(res.text))
		}
		if sr.notif != nil && sr.spawnerID != "" {
			msg := fmt.Sprintf("Sub-agent %s finished: %s", agentID, res.text)
			if res.err != nil {
				msg = fmt.Sprintf("Sub-agent %s failed: %v", agentID, res.err)
			}
			sr.notif.Add(sr.spawnerID, msg)
			log.Debug("SpawnSubAgent notification sent", "spawnerID", sr.spawnerID)
		}
	}()
	return sr
}
