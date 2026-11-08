package runtime

import (
	"context"
	"encoding/json"
	"fmt"

	"nine/internal/agent"
	"nine/internal/memory"
	"nine/internal/protocol"
)

// The capability surface (grants_list, the API's /capabilities, the web Inbox)
// at store level. The sandbox that grants conferred is gone (adr/modular_nine.md
// §9.10 — the toolvm is deleted), so the table stays for history and the API
// shape stays, but nothing derives grants any more and the capability_request
// tool refuses: there is no generated tier left to widen a ceiling on.

// CapabilityService answers and settles capability requests from the store.
type CapabilityService struct {
	store *memory.Store
}

// NewCapabilityService wires the capability surface. A nil store yields nil.
func NewCapabilityService(store *memory.Store) *CapabilityService {
	if store == nil {
		return nil
	}
	return &CapabilityService{store: store}
}

// State reports every grant in force and every request the agent has made.
func (s *CapabilityService) State() (protocol.CapabilityState, error) {
	grants, err := s.store.CapabilityGrantList()
	if err != nil {
		return protocol.CapabilityState{}, fmt.Errorf("read capability grants: %w", err)
	}
	requests, err := s.store.CapabilityRequestList(false)
	if err != nil {
		return protocol.CapabilityState{}, fmt.Errorf("read capability requests: %w", err)
	}

	out := protocol.CapabilityState{
		Grants:   make([]protocol.CapabilityGrantInfo, 0, len(grants)),
		Requests: make([]protocol.CapabilityRequestInfo, 0, len(requests)),
	}
	for _, g := range grants {
		out.Grants = append(out.Grants, protocol.CapabilityGrantInfo{
			ID: g.ID, Source: g.Source, Capability: g.Capability,
			Params: g.Params, RequestID: g.RequestID, CreatedAt: g.CreatedAt,
		})
	}
	for _, r := range requests {
		out.Requests = append(out.Requests, protocol.CapabilityRequestInfo{
			ID: r.ID, AgentID: r.AgentID, ToolName: r.ToolName,
			Capability: r.Capability, Params: r.Params, Reason: r.Reason,
			Status: r.Status, CreatedAt: r.CreatedAt, DecidedAt: r.DecidedAt,
		})
	}
	return out, nil
}

// Decide settles a request ("approve"/"deny") or revokes a grant ("revoke"). The
// decision is recorded in the store; with no sandbox to confer on, that is the
// whole effect.
func (s *CapabilityService) Decide(ctx context.Context, id, action string) (string, error) {
	switch action {
	case "approve":
		ok, err := s.store.CapabilityRequestDecide(id, memory.CapabilityRequestApproved, memory.NewID())
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("no pending capability request %q — it may already be decided", id)
		}
		return "approved " + id + " (recorded; the sandbox that would confer it is gone)", nil
	case "deny":
		ok, err := s.store.CapabilityRequestDecide(id, memory.CapabilityRequestDenied, "")
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("no pending capability request %q — it may already be decided", id)
		}
		return "denied " + id + "; nothing was granted", nil
	case "revoke":
		ok, err := s.store.CapabilityGrantRevoke(id)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("no revocable grant %q", id)
		}
		return "revoked " + id, nil
	default:
		return "", fmt.Errorf("capability action %q is not \"approve\", \"deny\" or \"revoke\"", action)
	}
}

// PendingCount is how many requests await a decision.
func (s *CapabilityService) PendingCount() (int, error) {
	pending, err := s.store.CapabilityRequestList(true)
	return len(pending), err
}

// NewCapabilityRequester returns the capability_request handler factory. The
// generated-tools tier the tool used to widen is gone (adr/modular_nine.md
// §9.10), so a request is recorded for the audit trail and then refused.
func NewCapabilityRequester(store *memory.Store, notify func(agentID, text string)) func(string) agent.CapabilityRequester {
	return func(agentID string) agent.CapabilityRequester {
		return func(ctx context.Context, capability, reason, toolName string, params json.RawMessage) (string, error) {
			id := memory.NewID()
			if store != nil {
				if err := store.CapabilityRequestCreate(id, agentID, toolName, capability, string(params), reason); err != nil {
					return "", err
				}
				if notify != nil {
					notify(agentID, fmt.Sprintf("capability request %s (%s) recorded but refused: the generated-tools tier is gone (adr/modular_nine.md §9.10)", id, capability))
				}
			}
			return "", fmt.Errorf("capability_request: no sandbox tier exists to grant %q on any more (adr/modular_nine.md §9.10); the request was recorded as %s and denied", capability, id)
		}
	}
}
