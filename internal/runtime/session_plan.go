package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"nine/internal/cron"
	"nine/internal/memory"
)

// ErrStall is passed to RoutineHandler.OnTurnEnd as err when the session's
// stall detector fires (AgentWorker.checkStall), in place of a real turn
// error. Handlers that care about stalls (e.g. pursue) check
// errors.Is(err, ErrStall); others ignore it.
var ErrStall = errors.New("session stalled")

// RoutineHandler is the extension point for session-plan routines. See
// docs/session-plans.md for the full design and rationale for these three
// methods.
type RoutineHandler interface {
	// Init is called once when a routine is loaded (whether freshly created or
	// restored from a session_plans row), so handlers can validate cfg or
	// capture any dependencies they need.
	Init(ctx context.Context, agentID string, cfg json.RawMessage) error
	// OnTurnEnd is called after every turn completes, and also when the
	// session's stall detector fires (result == "", err == ErrStall). Routines
	// use it to sync their own state from domain data and to react to
	// stalls. A routine that mutates its own SessionRoutine.Status/Result does so
	// by reading and writing its session_plans row directly.
	OnTurnEnd(ctx context.Context, agentID string, result string, err error) error
	// OnIdle is called by the per-routine idle scheduler when this routine's
	// idle_interval elapses. If the routine has work to do, it returns the text
	// for AgentWorker to submit as the next turn (ok=true). If there's
	// nothing to do, ok=false and no turn is submitted.
	OnIdle(ctx context.Context, agentID string) (turnText string, ok bool)
}

// RoutineFactory creates a new RoutineHandler instance for a routine Kind.
type RoutineFactory func() RoutineHandler

// RoutineRegistry maps a SessionRoutine.Kind to the factory for its handler.
var RoutineRegistry = map[string]RoutineFactory{
	"active": func() RoutineHandler { return activeRoutine{} },
}

// activeRoutine is the trivial routine every ordinary [active] conversation gets:
// it never has work of its own, but gives loadOrCreatePlan something to seed
// and is the slot future per-conversation routines get added alongside.
type activeRoutine struct{}

func (activeRoutine) Init(context.Context, string, json.RawMessage) error    { return nil }
func (activeRoutine) OnTurnEnd(context.Context, string, string, error) error { return nil }
func (activeRoutine) OnIdle(context.Context, string) (string, bool)          { return "", false }

// defaultProfile is the routine list for an ordinary user conversation.
var defaultProfile = []string{"active"}

// PlanStore is the persistence seam loadOrCreatePlan and the daemon-startup
// resume pass depend on. The concrete implementation is *memory.Store.
type PlanStore interface {
	SessionPlanGet(id string) (*memory.SessionPlan, error)
	SessionPlanSave(plan *memory.SessionPlan) error
	SessionPlanListActive() ([]memory.SessionPlan, error)
}

// sessionPlanState bundles a session's plan row with its instantiated stage
// handlers and the persistence callbacks a AgentWorker uses to keep them in
// sync.
type sessionPlanState struct {
	plan      *memory.SessionPlan
	handlers  map[string]RoutineHandler // keyed by SessionRoutine.Name
	persisted bool
	save      func(*memory.SessionPlan) error
	load      func(string) (*memory.SessionPlan, error)
}

// initRoutines instantiates a RoutineHandler for each stage from RoutineRegistry
// and calls Init on it.
func initRoutines(ctx context.Context, agentID string, stages []memory.SessionRoutine) (map[string]RoutineHandler, error) {
	handlers := make(map[string]RoutineHandler, len(stages))
	for _, st := range stages {
		factory, ok := RoutineRegistry[st.Kind]
		if !ok {
			return nil, fmt.Errorf("unknown routine kind %q", st.Kind)
		}
		h := factory()
		if err := h.Init(ctx, agentID, st.Config); err != nil {
			return nil, fmt.Errorf("init stage %q: %w", st.Name, err)
		}
		handlers[st.Name] = h
	}
	return handlers, nil
}

// loadOrCreatePlan loads agentID's session_plans row, or seeds a fresh
// in-memory plan from profile if none exists. Newly-seeded stages start
// Status: "active" — loadOrCreatePlan always has something to give the idle
// scheduler and OnTurnEnd/OnIdle hooks immediately.
//
// eager profiles (those with an idle-capable routine, e.g. idle-reflection or
// pursue) are persisted immediately, since creating the row is itself the
// signal that the session exists. Lazy profiles (ordinary [active]
// conversations) are returned unpersisted; the worker persists them on its
// first checkpoint, so short-lived conversations never write a row.
//
// store may be nil, in which case the plan is purely in-memory (no
// persistence, no resume).
// roleBearingKinds are the routine kinds that decide a session's role by kind
// alone. A session has exactly one role, fixed at loop-build time
// (docs/roles.md), so at most one stage may claim it.
//
// Only `pursue` is here, and deliberately: for every other stage the role is
// *data* (routineConfig.Role), not an implication of the kind. That is what lets
// the same kind be a session's whole purpose in one plan and a passenger in
// another — a reflection routine carries the reflection role when it is the
// session, and carries none when it rides alongside a pursue shell. Encoding
// the role in the kind made those two cases indistinguishable, so a reflecting
// pursue session was unrepresentable.
var roleBearingKinds = map[string]bool{"pursue": true}

// validateRoutines rejects a plan whose stages would make role, delegation, or
// goal ownership depend on their order in the array.
//
// roleNameForPlan returns the role of the *first* role-bearing routine it finds, so
// with two of them the session's role — and therefore its tool boundary — is
// decided by JSON serialization order. That is not a rule anyone could infer from
// the code, and it is not one worth having: it makes an operator's profile behave
// differently depending on how it was written down.
//
// Goal ownership is bounded for a stronger reason: a pursue session owns its goal
// 1:1 (agentID == goalID), so two pursue routines describe a session that owns two
// goals, which the identity relation cannot express.
//
// Checked at both construction and load. Load matters most — a plan predating this
// rule, or one written directly to the store, reaches the same code paths.
func validateRoutines(agentID string, stages []memory.SessionRoutine) error {
	var roleBearing, pursue []string
	for _, st := range stages {
		if st.Kind == "pursue" {
			pursue = append(pursue, st.Name)
		}
		if roleBearingKinds[st.Kind] || routineRole(st.Config) != "" {
			roleBearing = append(roleBearing, st.Name)
		}
	}
	if len(pursue) > 1 {
		return fmt.Errorf("session plan %s has %d goal-owning routines (%s); a pursue session owns exactly one goal",
			agentID, len(pursue), strings.Join(pursue, ", "))
	}
	if len(roleBearing) > 1 {
		return fmt.Errorf("session plan %s has %d role-bearing routines (%s); at most one stage may set the session's role",
			agentID, len(roleBearing), strings.Join(roleBearing, ", "))
	}
	return nil
}

func loadOrCreatePlan(ctx context.Context, store PlanStore, agentID string, profile []string, eager bool) (*sessionPlanState, error) {
	if store != nil {
		existing, err := store.SessionPlanGet(agentID)
		if err != nil {
			return nil, fmt.Errorf("load session plan %s: %w", agentID, err)
		}
		if existing != nil {
			if err := validateRoutines(agentID, existing.Routines); err != nil {
				return nil, err
			}
			handlers, err := initRoutines(ctx, agentID, existing.Routines)
			if err != nil {
				return nil, err
			}
			return &sessionPlanState{
				plan:      existing,
				handlers:  handlers,
				persisted: true,
				save:      store.SessionPlanSave,
				load:      store.SessionPlanGet,
			}, nil
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	stages := make([]memory.SessionRoutine, 0, len(profile))
	for _, kind := range profile {
		stages = append(stages, memory.SessionRoutine{
			Name:      kind,
			Kind:      kind,
			Status:    "active",
			UpdatedAt: now,
		})
	}
	// Reject a bad profile at construction, where the error names the caller's
	// mistake, rather than at the next load when its origin is gone.
	if err := validateRoutines(agentID, stages); err != nil {
		return nil, err
	}
	handlers, err := initRoutines(ctx, agentID, stages)
	if err != nil {
		return nil, err
	}
	plan := &memory.SessionPlan{
		ID:        agentID,
		Status:    "active",
		Routines:  stages,
		CreatedAt: now,
		UpdatedAt: now,
	}

	state := &sessionPlanState{plan: plan, handlers: handlers}
	if store != nil {
		state.save = store.SessionPlanSave
		state.load = store.SessionPlanGet
		if eager {
			if err := store.SessionPlanSave(plan); err != nil {
				return nil, fmt.Errorf("create session plan %s: %w", agentID, err)
			}
			state.persisted = true
		}
	}
	return state, nil
}

// routineConfig is the common shape of SessionRoutine.Config that the idle
// scheduler and role resolver understand. Stage-specific fields live alongside
// it and are read by the stage's own handler. Role and Delegates are set on a
// pursue routine seeded for a pre-defined standing agent
// (docs/predefined-agents.md §5 piece 5): they carry the configured work role
// and delegation opt-in into the pursue shell.
type routineConfig struct {
	IdleIntervalSeconds int    `json:"idle_interval_seconds,omitempty"`
	Schedule            string `json:"schedule,omitempty"` // cron expr — XOR IdleIntervalSeconds
	Role                string `json:"role,omitempty"`
	Delegates           bool   `json:"delegates,omitempty"`
}

// routineRole returns the configured work-role name carried by a stage's config,
// or "" when none is set.
func routineRole(cfg json.RawMessage) string {
	if len(cfg) == 0 {
		return ""
	}
	var c routineConfig
	if err := json.Unmarshal(cfg, &c); err != nil {
		return ""
	}
	return c.Role
}

// planOwnsGoal reports whether plan is a pursue shell — a session that steers
// its own goal and therefore gets the goal self-management tools always-on
// (docs/predefined-agents.md §3.1).
func planOwnsGoal(plan *sessionPlanState) bool {
	if plan == nil || plan.plan == nil {
		return false
	}
	for _, st := range plan.plan.Routines {
		if st.Kind == "pursue" {
			return true
		}
	}
	return false
}

// planDelegates reports whether a pursue shell was seeded with delegation
// opted in (a standing agent's [[agent]].delegates flag). Ordinary pursue
// sessions carry no such flag and rely on the pursue role's own Delegates.
func planDelegates(plan *sessionPlanState) bool {
	if plan == nil || plan.plan == nil {
		return false
	}
	for _, st := range plan.plan.Routines {
		if st.Kind != "pursue" || len(st.Config) == 0 {
			continue
		}
		var c routineConfig
		if err := json.Unmarshal(st.Config, &c); err == nil && c.Delegates {
			return true
		}
	}
	return false
}

// routineNextWake reports how long until an idle-capable routine is next due to
// wake, given lastFire (when it last fired) and the current time. A stage is
// scheduled either by a fixed idle interval (remaining = interval − elapsed) or
// by a cron expression (remaining = nextCronTime − now); scheduled is false for
// a non-idle stage or an unparseable schedule. Duration is clamped at 0 when the
// stage is already due (docs/scheduling.md).
func routineNextWake(cfg json.RawMessage, lastFire, now time.Time) (remaining time.Duration, scheduled bool) {
	d, ok := routineWakeDelta(cfg, lastFire, now)
	return max(d, 0), ok
}

// routineOverdueBy reports how long a stage has been due, and whether it is due at
// all. A stage due exactly now is overdue by zero and still due.
//
// It exists because routineNextWake clamps: every overdue stage reports 0 there, so
// two stages that are both late are indistinguishable, and "which is later" — the
// question fairness turns on — cannot be asked. This reads the same schedule
// unclamped.
func routineOverdueBy(cfg json.RawMessage, lastFire, now time.Time) (overdue time.Duration, due bool) {
	d, ok := routineWakeDelta(cfg, lastFire, now)
	if !ok || d > 0 {
		return 0, false
	}
	return -d, true
}

// routineWakeDelta is the shared, unclamped schedule reading: the signed time until
// the stage is next due, negative meaning overdue by that much. Callers take
// either the clamped view (routineNextWake, for arming a timer, which cannot be
// negative) or the overdue view (routineOverdueBy, for choosing between stages).
func routineWakeDelta(cfg json.RawMessage, lastFire, now time.Time) (delta time.Duration, scheduled bool) {
	if len(cfg) == 0 {
		return 0, false
	}
	var c routineConfig
	if err := json.Unmarshal(cfg, &c); err != nil {
		return 0, false
	}
	if c.IdleIntervalSeconds > 0 {
		interval := time.Duration(c.IdleIntervalSeconds) * time.Second
		return interval - now.Sub(lastFire), true
	}
	if c.Schedule != "" {
		sched, err := cron.Parse(c.Schedule)
		if err != nil {
			slog.Warn("invalid cron schedule in routine config", "schedule", c.Schedule, "err", err)
			return 0, false
		}
		next := sched.Next(lastFire)
		if next.IsZero() {
			return 0, false // a schedule that can never fire
		}
		return next.Sub(now), true
	}
	return 0, false
}

// stageScheduled reports whether a stage is idle-capable — armed by a fixed
// interval or a valid cron schedule — used by the daemon-startup resume pass.
func stageScheduled(cfg json.RawMessage) bool {
	if len(cfg) == 0 {
		return false
	}
	var c routineConfig
	if err := json.Unmarshal(cfg, &c); err != nil {
		return false
	}
	if c.IdleIntervalSeconds > 0 {
		return true
	}
	if c.Schedule != "" {
		if _, err := cron.Parse(c.Schedule); err == nil {
			return true
		}
	}
	return false
}

// newIdleCapablePlan builds a fresh SessionPlan for id with a single stage of
// the given kind, configured with idleInterval as its idle_interval_seconds
// so the session's idle scheduler is armed from creation. Used by
// BootstrapSelfReflection and SpawnGoalSession to seed idle-capable profiles
// ([idle-reflection], [pursue]) that loadOrCreatePlan's generic profile
// seeding (which sets no Config) can't produce.
func newIdleCapablePlan(id, stageKind, role string, idleInterval time.Duration) (*memory.SessionPlan, error) {
	cfg, err := json.Marshal(routineConfig{
		Role:                role,
		IdleIntervalSeconds: int(idleInterval.Seconds()),
	})
	if err != nil {
		return nil, fmt.Errorf("marshal %s config: %w", stageKind, err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	return &memory.SessionPlan{
		ID:     id,
		Status: "active",
		Routines: []memory.SessionRoutine{{
			Name:      stageKind,
			Kind:      stageKind,
			Status:    "active",
			Config:    cfg,
			UpdatedAt: now,
		}},
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// newStandingPursuePlan builds a fresh pursue-shell SessionPlan for a
// pre-defined standing agent (docs/predefined-agents.md). Like
// newIdleCapablePlan it seeds a single active pursue routine, but it also stamps
// the configured work-role name and delegation flag into the routine config so
// roleNameForPlan / planDelegates resolve the agent's narrowed role while
// keeping the pursue shell. The stage's wake trigger is a cron schedule when
// schedule is non-empty, otherwise the fixed idleInterval.
func newStandingPursuePlan(id, role string, delegates bool, idleInterval time.Duration, schedule string, routines []RoutineDecl) (*memory.SessionPlan, error) {
	sc := routineConfig{Role: role, Delegates: delegates}
	if schedule != "" {
		sc.Schedule = schedule
	} else {
		sc.IdleIntervalSeconds = int(idleInterval.Seconds())
	}
	cfg, err := json.Marshal(sc)
	if err != nil {
		return nil, fmt.Errorf("marshal standing pursue config: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)

	stages := []memory.SessionRoutine{{
		Name:      "pursue",
		Kind:      "pursue",
		Status:    "active",
		Config:    cfg,
		UpdatedAt: now,
	}}
	// Additional routines wake on their own cadence beside the pursue shell.
	// None carries a role: the pursue routine is the session's one role-bearing
	// stage, and validateRoutines rejects a second claimant.
	for _, a := range routines {
		ac := routineConfig{}
		if a.Schedule != "" {
			ac.Schedule = a.Schedule
		} else {
			ac.IdleIntervalSeconds = int(a.Interval.Seconds())
		}
		b, err := json.Marshal(ac)
		if err != nil {
			return nil, fmt.Errorf("marshal routine %q config: %w", a.Kind, err)
		}
		stages = append(stages, memory.SessionRoutine{
			Name:      a.Kind,
			Kind:      a.Kind,
			Status:    "active",
			Config:    b,
			UpdatedAt: now,
		})
	}

	plan := &memory.SessionPlan{
		ID:        id,
		Status:    "active",
		Routines:  stages,
		CreatedAt: now,
		UpdatedAt: now,
	}
	// Fail here rather than at the next load: this is where the operator's
	// config becomes a plan, so this is where the error can name their mistake.
	if err := validateRoutines(id, plan.Routines); err != nil {
		return nil, err
	}
	return plan, nil
}

// RoutineDecl is one additional stage requested on a standing agent's session,
// resolved from an [[agent.routine]] entry. Exactly one of Interval or Schedule
// is set; a routine with neither would never wake, which ValidateRoutineDecls
// rejects before it can become a stage that quietly does nothing.
type RoutineDecl struct {
	Kind     string
	Interval time.Duration
	Schedule string
}

// ValidateRoutineDecls checks operator-declared routines before they become stages.
//
// Every failure here is one that would otherwise be silent: an unregistered kind
// produces a stage with no handler (it is scheduled, wakes, finds nothing to run,
// and rearms forever), a missing cadence produces a stage that never wakes at
// all, and a duplicate kind produces two stages with the same name, which
// idleSince keys on.
func ValidateRoutineDecls(routines []RoutineDecl) error {
	seen := map[string]bool{}
	for _, a := range routines {
		if a.Kind == "" {
			return errors.New("routine has no kind")
		}
		// Checked before the registry lookup so the specific reason wins: pursue
		// is a registered kind, just not one a routine may ask for.
		if a.Kind == "pursue" {
			return errors.New(`routine "pursue" duplicates the session's own pursue shell`)
		}
		if _, ok := RoutineRegistry[a.Kind]; !ok {
			return fmt.Errorf("routine %q is not a registered routine kind", a.Kind)
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

// planNeedsResume reports whether p has at least one active, idle-capable
// stage, i.e. whether its AgentWorker should be started at daemon startup
// (Pilot 4's general resume rule).
func planNeedsResume(p memory.SessionPlan) bool {
	if p.Status != "active" {
		return false
	}
	for _, st := range p.Routines {
		if st.Status != "active" {
			continue
		}
		if stageScheduled(st.Config) {
			return true
		}
	}
	return false
}
