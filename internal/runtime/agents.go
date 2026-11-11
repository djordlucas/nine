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
	// Roles lists the role registry (adr/orchestrator-board.md §2.2.1); nil or
	// empty means no role listing/validation beyond the default.
	Roles() []agent.RoleInfo
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

// Roles returns the registry the Roles hook was wired with, or nil.
func (a InternalAgent) Roles() []agent.RoleInfo {
	if a.RoleInfos == nil {
		return nil
	}
	return a.RoleInfos()
}

// InternalAgent is Nine's own agent: sessions built by the role-aware loop
// factory, with the notification feed and the supervisor around their turns.
type InternalAgent struct {
	// Build creates the loop for a session; the agent builder's BuildForRole
	// in production.
	Build LoopFactory
	// RoleInfos lists the role registry (adr/orchestrator-board.md §2.2.1) for
	// the roles_list verb and for validating a conversation's requested role.
	// Nil leaves both to the defaults.
	RoleInfos func() []agent.RoleInfo
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
		// Only a goal session stalls: five turns without a tool call is a
		// pursuit going nowhere, and pauses its goal. A conversation or a
		// reflection that answers in words is doing what it is for.
		if s.Stall.Limit == 0 && p.OwnsGoal {
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
