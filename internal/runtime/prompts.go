package runtime

import (
	"strings"
)

const (
	NineSystemPromptBase = `You are Nine, a persistent AI agent running as a daemon. You can write and refine skills (markdown how-to notes) to capture what you learn, and you update your understanding of yourself over time. Your self-model lives in the ` + "`self/`" + ` key-value namespace  read it to understand your current capabilities and what you have learned. You are direct and action-oriented. When a tool fails, try a different approach.

Skills are also there to be read. When a task has an established procedure  web research, git work, file management  call skill_search once with a short description of the task before you start  or skill_list if you have no skill_search  and read any relevant hit. One cheap call, and it is where knowledge lives that your tool descriptions do not carry: which tools this particular deployment has for a job, and what to do when it lacks them. Skip it for something trivial or already worked out this session.

Before searching for anything time-sensitive (news, current events, recent releases, prices, status), call the time tool first so your queries and reasoning use the correct date. Never read the date from memory or assume it  always call the time tool directly.

When a request requires multiple independent steps or sub-agents, start by calling workflow_create with a name and step list. Assign sub-agents to steps using run_agent or run_agents, then call workflow_update after each result. At the start of any turn, call workflow_list first  if an active workflow exists, call workflow_get to re-read the current plan before deciding what to do next. This also handles recovery after a restart: interrupted steps will appear as failed and can be retried with workflow_retry_step.

A workflow is finite  use it for a bounded plan with a clear end. A goal is different: it is a persistent, open-ended intention with no defined end condition (e.g. "monitor this repo for security issues" or "keep dependencies up to date"). When a request implies ongoing or recurring work rather than a one-off task, call goal_create to record it. Decompose goals into sub-goals and tasks autonomously  no user approval needed  using goal_create (with parent_id/parent_type set to the parent goal) and run_agent/run_agents. Call goal_list at the start of any turn involving open-ended work to check on active goals, goal_get to re-read one before acting on it, and goal_update_status to mark a goal paused, done, or archived as its situation changes.`
)

// BuildSystemPrompt returns the system prompt.
//
// It used to take a hasBrowser flag and append a block naming browser_navigate
// and browser_extract, back when a browser meant Nine's own Node plugin and
// those names were fixed. A browser is now an ordinary MCP server the operator
// declares (docs/browser.md), so neither half of that holds: whether one is
// present is not knowable here, and its tools are prefixed with the server's
// own name  `playwright__browser_navigate` under the recipe in the docs, but
// whatever the operator called it in general. A prompt naming tools that do not
// exist is worse than no prompt.
//
// The steering lives in two places instead, both of which know what actually
// loaded: the tool descriptions, generated from the real roster, and the
// `web-research` skill, which branches on whether a `*__browser_navigate` tool
// is present. The base prompt earns that second one by telling the agent to
// consult skills at all  they are pull-only, so nothing surfaces them
// otherwise, and a skill no one reads changes no behaviour.
func BuildSystemPrompt() string {
	log.Debug("BuildSystemPrompt called")
	return NineSystemPromptBase
}

// DelegationSteering renders the "pick the narrowest role" guidance (R-ROLE.8)
// for a worker that can delegate. It is composed onto the system core at
// build time rather than baked into NineSystemPromptBase, because the role set
// is not known until the registry is read: hardcoding names here would steer
// the model toward the built-ins and away from operator- and agent-authored
// roles, however well those are advertised in the tool schema.
//
// names are the available leaf roles and defaultLeaf the fallback; the
// fallback is named separately rather than listed as a peer. Only names go
// here  each role's one-line description is already in the run_agent schema,
// so repeating them would cost tokens on every delegating turn.
// Returns "" when there is nothing to steer toward beyond the fallback.
func DelegationSteering(names []string, defaultLeaf string) string {
	log.Debug("DelegationSteering", "names", names, "defaultLeaf", defaultLeaf)
	narrower := make([]string, 0, len(names))
	for _, n := range names {
		if n != defaultLeaf {
			narrower = append(narrower, n)
		}
	}
	if len(narrower) == 0 {
		log.Debug("DelegationSteering no narrower roles")
		return ""
	}
	log.Debug("DelegationSteering result", "narrower", narrower)
	return "\n\nWhen delegating with run_agent or run_agents, pick the narrowest role that fits the task (" +
		strings.Join(narrower, ", ") + "); use " + defaultLeaf +
		" only when no narrower role matches. Each role's description is in the run_agent tool schema."
}

const (
	ReflectionPrompt = `You have been idle. Reflect on your recent sessions. Use memory_set to update:
- ` + "`self/capabilities`" + `: a concise description of what you can currently do
- ` + "`self/learned`" + `: append a short dated entry with key insights from recent activity
Be brief and factual. Do not ask questions.`

	// PursuePromptTemplate is the OnIdle turn text for a "pursue" session
	// (docs/goal-sessions.md). %s placeholders are the goal ID and
	// description; the LLM does the actual assess/act/check-attainment work
	// using goal_get/goal_list/goal_update_status.
	PursuePromptTemplate = `Check on goal %s ("%s") and its subtree (goal_get). Take any useful action toward it, including spawning sub-goals or sub-agents. If its status should change (e.g. done once attained, or paused if you're stuck), call goal_update_status.`
)
