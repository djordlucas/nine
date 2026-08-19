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

// ErrStall is passed to StageHandler.OnTurnEnd as err when the session's
// stall detector fires (AgentWorker.checkStall), in place of a real turn
// error. Handlers that care about stalls (e.g. pursue) check
// errors.Is(err, ErrStall); others ignore it.
var ErrStall = errors.New("session stalled")

// StageHandler is the extension point for session-plan stages. See
// docs/session-plans.md for the full design and rationale for these three
// methods.
type StageHandler interface {
	// Init is called once when a stage is loaded (whether freshly created or
	// restored from a session_plans row), so handlers can validate cfg or
	// capture any dependencies they need.
	Init(ctx context.Context, agentID string, cfg json.RawMessage) error
	// OnTurnEnd is called after every turn completes, and also when the
	// session's stall detector fires (result == "", err == ErrStall). Stages
	// use it to sync their own state from domain data and to react to
	// stalls. A stage that mutates its own SessionStage.Status/Result does so
	// by reading and writing its session_plans row directly.
	OnTurnEnd(ctx context.Context, agentID string, result string, err error) error
	// OnIdle is called by the per-stage idle scheduler when this stage's
	// idle_interval elapses. If the stage has work to do, it returns the text
	// for AgentWorker to submit as the next turn (ok=true). If there's
	// nothing to do, ok=false and no turn is submitted.
	OnIdle(ctx context.Context, agentID string) (turnText string, ok bool)
}

// StageFactory creates a new StageHandler instance for a stage Kind.
type StageFactory func() StageHandler

// StageRegistry maps a SessionStage.Kind to the factory for its handler.
var StageRegistry = map[string]StageFactory{
	"active": func() StageHandler { return activeStage{} },
}

// activeStage is the trivial stage every ordinary [active] conversation gets:
// it never has work of its own, but gives loadOrCreatePlan something to seed
// and is the slot future per-conversation stages get added alongside.
type activeStage struct{}

func (activeStage) Init(context.Context, string, json.RawMessage) error    { return nil }
func (activeStage) OnTurnEnd(context.Context, string, string, error) error { return nil }
func (activeStage) OnIdle(context.Context, string) (string, bool)          { return "", false }

// defaultProfile is the stage list for an ordinary user conversation.
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
	handlers  map[string]StageHandler // keyed by SessionStage.Name
	persisted bool
	save      func(*memory.SessionPlan) error
	load      func(string) (*memory.SessionPlan, error)
}

// initStages instantiates a StageHandler for each stage from StageRegistry
// and calls Init on it.
func initStages(ctx context.Context, agentID string, stages []memory.SessionStage) (map[string]StageHandler, error) {
	handlers := make(map[string]StageHandler, len(stages))
	for _, st := range stages {
		factory, ok := StageRegistry[st.Kind]
		if !ok {
			return nil, fmt.Errorf("unknown stage kind %q", st.Kind)
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
// eager profiles (those with an idle-capable stage, e.g. idle-reflection or
// pursue) are persisted immediately, since creating the row is itself the
// signal that the session exists. Lazy profiles (ordinary [active]
// conversations) are returned unpersisted; the worker persists them on its
// first checkpoint, so short-lived conversations never write a row.
//
// store may be nil, in which case the plan is purely in-memory (no
// persistence, no resume).
// roleBearingKinds are the stage kinds that decide a session's role. A session
// has exactly one role, fixed at loop-build time (docs/roles.md), so at most one
// stage may claim it.
var roleBearingKinds = map[string]bool{"pursue": true, "idle-reflection": true}

// validateStages rejects a plan whose stages would make role, delegation, or
// goal ownership depend on their order in the array.
//
// roleNameForPlan returns the role of the *first* role-bearing stage it finds, so
// with two of them the session's role — and therefore its tool boundary — is
// decided by JSON serialization order. That is not a rule anyone could infer from
// the code, and it is not one worth having: it makes an operator's profile behave
// differently depending on how it was written down.
//
// Goal ownership is bounded for a stronger reason: a pursue session owns its goal
// 1:1 (agentID == goalID), so two pursue stages describe a session that owns two
// goals, which the identity relation cannot express.
//
// Checked at both construction and load. Load matters most — a plan predating this
// rule, or one written directly to the store, reaches the same code paths.
func validateStages(agentID string, stages []memory.SessionStage) error {
	var roleBearing, pursue []string
	for _, st := range stages {
		if st.Kind == "pursue" {
			pursue = append(pursue, st.Name)
		}
		if roleBearingKinds[st.Kind] || stageRole(st.Config) != "" {
			roleBearing = append(roleBearing, st.Name)
		}
	}
	if len(pursue) > 1 {
		return fmt.Errorf("session plan %s has %d goal-owning stages (%s); a pursue session owns exactly one goal",
			agentID, len(pursue), strings.Join(pursue, ", "))
	}
	if len(roleBearing) > 1 {
		return fmt.Errorf("session plan %s has %d role-bearing stages (%s); at most one stage may set the session's role",
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
			if err := validateStages(agentID, existing.Stages); err != nil {
				return nil, err
			}
			handlers, err := initStages(ctx, agentID, existing.Stages)
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
	stages := make([]memory.SessionStage, 0, len(profile))
	for _, kind := range profile {
		stages = append(stages, memory.SessionStage{
			Name:      kind,
			Kind:      kind,
			Status:    "active",
			UpdatedAt: now,
		})
	}
	// Reject a bad profile at construction, where the error names the caller's
	// mistake, rather than at the next load when its origin is gone.
	if err := validateStages(agentID, stages); err != nil {
		return nil, err
	}
	handlers, err := initStages(ctx, agentID, stages)
	if err != nil {
		return nil, err
	}
	plan := &memory.SessionPlan{
		ID:        agentID,
		Status:    "active",
		Stages:    stages,
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

// stageConfig is the common shape of SessionStage.Config that the idle
// scheduler and role resolver understand. Stage-specific fields live alongside
// it and are read by the stage's own handler. Role and Delegates are set on a
// pursue stage seeded for a pre-defined standing agent
// (docs/predefined-agents.md §5 piece 5): they carry the configured work role
// and delegation opt-in into the pursue shell.
type stageConfig struct {
	IdleIntervalSeconds int    `json:"idle_interval_seconds,omitempty"`
	Schedule            string `json:"schedule,omitempty"` // cron expr — XOR IdleIntervalSeconds
	Role                string `json:"role,omitempty"`
	Delegates           bool   `json:"delegates,omitempty"`
}

// stageRole returns the configured work-role name carried by a stage's config,
// or "" when none is set.
func stageRole(cfg json.RawMessage) string {
	if len(cfg) == 0 {
		return ""
	}
	var c stageConfig
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
	for _, st := range plan.plan.Stages {
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
	for _, st := range plan.plan.Stages {
		if st.Kind != "pursue" || len(st.Config) == 0 {
			continue
		}
		var c stageConfig
		if err := json.Unmarshal(st.Config, &c); err == nil && c.Delegates {
			return true
		}
	}
	return false
}

// stageNextWake reports how long until an idle-capable stage is next due to
// wake, given lastFire (when it last fired) and the current time. A stage is
// scheduled either by a fixed idle interval (remaining = interval − elapsed) or
// by a cron expression (remaining = nextCronTime − now); scheduled is false for
// a non-idle stage or an unparseable schedule. Duration is clamped at 0 when the
// stage is already due (docs/scheduling.md).
func stageNextWake(cfg json.RawMessage, lastFire, now time.Time) (remaining time.Duration, scheduled bool) {
	d, ok := stageWakeDelta(cfg, lastFire, now)
	return max(d, 0), ok
}

// stageOverdueBy reports how long a stage has been due, and whether it is due at
// all. A stage due exactly now is overdue by zero and still due.
//
// It exists because stageNextWake clamps: every overdue stage reports 0 there, so
// two stages that are both late are indistinguishable, and "which is later" — the
// question fairness turns on — cannot be asked. This reads the same schedule
// unclamped.
func stageOverdueBy(cfg json.RawMessage, lastFire, now time.Time) (overdue time.Duration, due bool) {
	d, ok := stageWakeDelta(cfg, lastFire, now)
	if !ok || d > 0 {
		return 0, false
	}
	return -d, true
}

// stageWakeDelta is the shared, unclamped schedule reading: the signed time until
// the stage is next due, negative meaning overdue by that much. Callers take
// either the clamped view (stageNextWake, for arming a timer, which cannot be
// negative) or the overdue view (stageOverdueBy, for choosing between stages).
func stageWakeDelta(cfg json.RawMessage, lastFire, now time.Time) (delta time.Duration, scheduled bool) {
	if len(cfg) == 0 {
		return 0, false
	}
	var c stageConfig
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
			slog.Warn("invalid cron schedule in stage config", "schedule", c.Schedule, "err", err)
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
	var c stageConfig
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
func newIdleCapablePlan(id, stageKind string, idleInterval time.Duration) (*memory.SessionPlan, error) {
	cfg, err := json.Marshal(map[string]int{"idle_interval_seconds": int(idleInterval.Seconds())})
	if err != nil {
		return nil, fmt.Errorf("marshal %s config: %w", stageKind, err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	return &memory.SessionPlan{
		ID:     id,
		Status: "active",
		Stages: []memory.SessionStage{{
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
// newIdleCapablePlan it seeds a single active pursue stage, but it also stamps
// the configured work-role name and delegation flag into the stage config so
// roleNameForPlan / planDelegates resolve the agent's narrowed role while
// keeping the pursue shell. The stage's wake trigger is a cron schedule when
// schedule is non-empty, otherwise the fixed idleInterval.
func newStandingPursuePlan(id, role string, delegates bool, idleInterval time.Duration, schedule string) (*memory.SessionPlan, error) {
	sc := stageConfig{Role: role, Delegates: delegates}
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
	return &memory.SessionPlan{
		ID:     id,
		Status: "active",
		Stages: []memory.SessionStage{{
			Name:      "pursue",
			Kind:      "pursue",
			Status:    "active",
			Config:    cfg,
			UpdatedAt: now,
		}},
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// planNeedsResume reports whether p has at least one active, idle-capable
// stage, i.e. whether its AgentWorker should be started at daemon startup
// (Pilot 4's general resume rule).
func planNeedsResume(p memory.SessionPlan) bool {
	if p.Status != "active" {
		return false
	}
	for _, st := range p.Stages {
		if st.Status != "active" {
			continue
		}
		if stageScheduled(st.Config) {
			return true
		}
	}
	return false
}
