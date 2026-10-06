package runtime

import "nine/internal/agent"

// Agent decides what a session is: its persona, tools and context, and what
// follows each turn (adr/agent-boundary.md). The daemon owns conversations,
// their workers, checkpoints and the journal, and asks the agent for each
// session it creates or resumes.
type Agent interface {
	// NewSession builds the loop for conversation id and the hooks its worker
	// runs around turns. It is called once per new conversation and once per
	// resume from a checkpoint.
	NewSession(id string, p RoleParams) Session
}

// Session is what an agent hands the daemon for one conversation.
type Session struct {
	Loop *agent.Loop
	// Stall fires OnStall after Limit consecutive turns without a tool call.
	// The zero value detects nothing.
	Stall StallConfig
	// Notifications returns the texts to prepend to the conversation's next
	// user turn. Nil prepends nothing.
	Notifications func(id string) ([]string, error)
	// OnComplete runs after each completed turn. Nil does nothing.
	OnComplete func(id string)
}

// InternalAgent is Nine's own agent: sessions built by the role-aware loop
// factory, with the notification feed and the supervisor around their turns.
type InternalAgent struct {
	// Build creates the loop for a session; the agent builder's BuildForRole
	// in production.
	Build LoopFactory
	// Notifications feeds the "[Notifications]" block prepended to a user
	// turn. Nil disables it.
	Notifications NotifStore
	// Supervisor receives stall and turn-completion events. Nil disables both.
	Supervisor *Supervisor
	// Stall overrides the supervisor's stall detection when its Limit is set.
	Stall StallConfig
}

// supervisorStallLimit is how many consecutive turns without a tool call count
// as a stall.
const supervisorStallLimit = 5

// NewSession implements Agent.
func (a InternalAgent) NewSession(id string, p RoleParams) Session {
	s := Session{Loop: a.Build(id, p), Stall: a.Stall}
	if a.Notifications != nil {
		notif := a.Notifications
		s.Notifications = func(id string) ([]string, error) { return notif.Fetch(id) }
	}
	if sup := a.Supervisor; sup != nil {
		if s.Stall.Limit == 0 {
			s.Stall = StallConfig{
				Limit: supervisorStallLimit,
				OnStall: func(agentID string) {
					sup.Post(Event{Kind: EventGoalStalls, AgentID: agentID})
				},
			}
		}
		s.OnComplete = func(agentID string) {
			sup.Post(Event{Kind: EventAgentCompletes, AgentID: agentID})
		}
	}
	return s
}
