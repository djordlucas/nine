package runner

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"nine/internal/agent"
	"nine/internal/embed"
	"nine/internal/llm"
	"nine/internal/memory"
	"nine/internal/plugin"
	"nine/internal/protocol"
	"nine/internal/runtime"
)

// Harness stands up a real Nine daemon in-process — the production wiring from
// cmd/nine/daemon.go minus resume/standing-agent/self-reflection bootstrap — over
// a per-run isolated store and ephemeral workspace, then drives a case's prompts
// and captures the durable journal for grading (docs/evals.md §5). The LLM
// provider is injected, so the same harness serves Track R (a recorded provider)
// and Track L (a live model).
//
// A Harness is reusable and stateless across runs; each Run allocates and tears
// down its own store, workspace, plugins, and daemon. Runs are not safe to
// execute concurrently on one process because the pursue stage handler is a
// package global (see Run); the runner executes them sequentially.
type Harness struct {
	// PluginBin is the directory holding the plugin binaries (shell/files/http/
	// time). Empty disables plugins, leaving only the in-process core tools
	// (memory_*, file_*, skill_*) — enough for many cases and all harness self-tests.
	PluginBin string
	// Embedder powers semantic memory, tool ranking, and related-session
	// surfacing. Nil disables those features (the reads cost nothing when off).
	Embedder embed.Embedder
	// BaseDir overrides where each run's isolated database file is created
	// (else the system temp dir).
	BaseDir string
	// ContextBudget is the per-loop token budget (default 100_000).
	ContextBudget int
	// MaxToolOutputTokens mirrors [tools] max_output_tokens (production:
	// cmd/nine/daemon.go). 0 keeps agent.DefaultMaxOutputTokens. A case can
	// override it via session.config "tools.max_output_tokens".
	MaxToolOutputTokens int
}

// RunResult is everything grading needs from one execution of a case: the final
// answer per prompt, the full journal, and live handles to the isolated store and
// workspace for side-effect reads. Close tears the run down; call it after
// grading (the store must stay open until then).
type RunResult struct {
	AgentID   string
	Answers   []string
	Events    []memory.SessionEvent
	Store     *memory.Store
	Workspace string
	// PreGoals is the goal count after setup but before the prompts ran, so
	// grading can attribute newly-created goals to the run (docs/evals.md §3
	// goals.created).
	PreGoals int

	cleanups []func()
	closed   bool
}

// Close runs teardown (daemon stop, plugin shutdown, schema drop, workspace
// removal) in reverse order. Idempotent.
func (r *RunResult) Close() {
	if r == nil || r.closed {
		return
	}
	r.closed = true
	for i := len(r.cleanups) - 1; i >= 0; i-- {
		r.cleanups[i]()
	}
}

// Run executes one repetition of c against provider and returns a RunResult whose
// store/workspace remain live for grading until Close. Isolation is total: a
// fresh schema, a fresh workspace, and a fresh daemon per call.
func (h *Harness) Run(ctx context.Context, c *Case, provider llm.Provider) (res *RunResult, err error) {
	r := &RunResult{}
	// On any setup error, unwind whatever we already built.
	defer func() {
		if err != nil {
			r.Close()
			res = nil
		}
	}()

	// 1. Isolated store (schema-per-run).
	store, dropStore, err := openIsolatedStore(h.BaseDir)
	if err != nil {
		return nil, err
	}
	r.Store = store
	r.cleanups = append(r.cleanups, dropStore)

	// 2. Ephemeral workspace. /tmp (not the default temp root) keeps the socket
	//    path short on macOS, which caps Unix-socket paths at 104 bytes.
	workspace, err := os.MkdirTemp("/tmp", "nine-eval-ws-")
	if err != nil {
		return nil, err
	}
	r.Workspace = workspace
	r.cleanups = append(r.cleanups, func() { os.RemoveAll(workspace) })

	// 3. Apply setup fixtures to the fresh store + workspace.
	if err := applySetup(store, workspace, c.Setup); err != nil {
		return nil, fmt.Errorf("apply setup: %w", err)
	}

	// 4. Plugins (optional). Each gets the workspace as its root so file writes
	//    and setup.files line up. Missing binaries are skipped by TryStart.
	pluginMgr := plugin.NewManager(h.PluginBin)
	if h.PluginBin != "" {
		pluginMgr.TryStart("files", "NINE_WORKSPACE="+workspace)
		pluginMgr.TryStart("shell")
		pluginMgr.TryStart("http")
		pluginMgr.TryStart("time")
	}
	r.cleanups = append(r.cleanups, func() { pluginMgr.StopAll() }) //nolint:errcheck

	// 5. Assemble the daemon core — the same wiring cmd/nine/daemon.go uses, minus
	//    the production-only bootstrap (resume, standing agents, self-reflection,
	//    subscribers). The store, plugins, provider queue, and per-case knobs are
	//    injected; the runner serializes runs because Assemble registers the
	//    pursue stage handler over this run's store (a package global).
	budget := h.ContextBudget
	if budget == 0 {
		budget = 100_000
	}

	var hitl *runtime.HITL
	if c.Session.Interactive {
		hitl = runtime.NewHITL(store, 2*time.Minute)
	}

	// Force the case's role by overriding the resolved params before delegating
	// to the production factory. The daemon otherwise derives the role from the
	// session's plan profile; a case declares the role it means to exercise.
	forcedRole := c.Session.Role
	roleFactory := func(inner runtime.LoopFactory) runtime.LoopFactory {
		return func(agentID string, p runtime.RoleParams) *agent.Loop {
			if forcedRole != "" {
				p.Role = forcedRole
			}
			return inner(agentID, p)
		}
	}

	sock := filepath.Join(workspace, "d.sock")
	asm := runtime.Assemble(runtime.AssemblyConfig{
		SocketPath:          sock,
		Store:               store,
		Plugins:             pluginMgr,
		Embedder:            h.Embedder,
		ContextBudget:       budget,
		MaxToolOutputTokens: maxToolOutputTokens(h.MaxToolOutputTokens, c),
		SystemPrompt:        runtime.BuildSystemPrompt(false),
		RelatedSessions:     h.Embedder != nil,
		SurfaceMemories:     h.Embedder != nil,
		Queue:               llm.NewQueue(provider, 4),
		TaskTimeoutSeconds:  c.TimeoutSecs,
		HITL:                hitl,
		DefaultLeafRole:     "executor",
		MaxGoalSessions:     8,
		RoleFactory:         roleFactory,
	})
	daemon := asm.Daemon
	supervisor := asm.Supervisor

	// The sink is closed explicitly after the last turn (to drain the journal
	// before reads); guard against a double close on teardown.
	var sinkOnce sync.Once
	closeSink := func() { sinkOnce.Do(func() { asm.EventSink.Close() }) } //nolint:errcheck
	r.cleanups = append(r.cleanups, closeSink)

	// 6. Start the daemon and wait for the socket.
	dctx, dcancel := context.WithCancel(ctx)
	go supervisor.Run(dctx)
	go daemon.Start(dctx) //nolint:errcheck
	r.cleanups = append(r.cleanups, func() { dcancel(); daemon.Stop() })
	if err := waitForSocket(sock, 5*time.Second); err != nil {
		return nil, err
	}

	// 7. Drive the prompts on one conversation.
	client, err := protocol.Connect(sock)
	if err != nil {
		return nil, err
	}
	r.cleanups = append(r.cleanups, func() { client.Close() }) //nolint:errcheck

	agentID, _, _, err := client.NewConversationInteractive(c.Session.Interactive)
	if err != nil {
		return nil, err
	}
	r.AgentID = agentID

	// Snapshot goal count before the prompts, so grading can count goals the run
	// itself created (setup may pre-seed some).
	if goals, gerr := store.GoalList(); gerr == nil {
		r.PreGoals = len(goals)
	}

	answers, err := driveTurns(ctx, sock, client, agentID, c)
	if err != nil {
		return nil, err
	}
	r.Answers = answers

	// 8. Drain the journal, then read it back for grading.
	closeSink()
	events, err := store.SessionEventsByAgent(agentID)
	if err != nil {
		return nil, fmt.Errorf("read journal: %w", err)
	}
	r.Events = events

	return r, nil
}

// driveTurns sends each prompt in order and returns the final answers. When the
// case is interactive it answers ask_human prompts from the case's HumanAnswers
// (in order) over a second connection, since the turn connection is blocked
// reading the streamed response.
func driveTurns(ctx context.Context, sock string, client *protocol.Client, agentID string, c *Case) ([]string, error) {
	answers := make([]string, 0, len(c.Prompts))
	answerIdx := 0

	var onProgress func(protocol.ProgressEvent)
	if c.Session.Interactive {
		replier, err := protocol.Connect(sock)
		if err != nil {
			return nil, err
		}
		defer replier.Close() //nolint:errcheck
		var mu sync.Mutex
		onProgress = func(evt protocol.ProgressEvent) {
			if evt.Type != "human_input_required" || evt.HumanRequest == nil {
				return
			}
			mu.Lock()
			ans := "yes"
			if answerIdx < len(c.HumanAnswers) {
				ans = c.HumanAnswers[answerIdx]
			}
			answerIdx++
			mu.Unlock()
			replier.AnswerHuman(agentID, evt.HumanRequest.RequestID, ans) //nolint:errcheck
		}
	}

	for i, prompt := range c.Prompts {
		select {
		case <-ctx.Done():
			return answers, ctx.Err()
		default:
		}
		ans, err := client.TurnWithProgress(agentID, prompt, onProgress)
		if err != nil {
			return answers, fmt.Errorf("turn %d: %w", i+1, err)
		}
		answers = append(answers, ans)
	}
	return answers, nil
}

// applySetup seeds the isolated store and workspace with the case's fixtures.
func applySetup(store *memory.Store, workspace string, s Setup) error {
	for k, v := range s.KV {
		if err := store.Set(k, v); err != nil {
			return fmt.Errorf("seed kv %q: %w", k, err)
		}
	}
	for name, body := range s.Skills {
		if err := store.SkillUpsert(memory.Skill{Name: name, Content: body, Source: "eval"}); err != nil {
			return fmt.Errorf("seed skill %q: %w", name, err)
		}
	}
	for _, desc := range s.Goals {
		if err := store.GoalCreate(memory.NewID(), desc, "", ""); err != nil {
			return fmt.Errorf("seed goal %q: %w", desc, err)
		}
	}
	for path, content := range s.Files {
		abs, err := workspacePath(workspace, path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			return fmt.Errorf("seed file %q: %w", path, err)
		}
	}
	return nil
}

// workspacePath resolves a case-declared path against the workspace root. A
// leading "/work" (the schema's convention) or "/" is treated as workspace-root
// relative; any resolved path escaping the root is rejected.
func workspacePath(root, p string) (string, error) {
	clean := p
	if strings.HasPrefix(clean, "/work/") {
		clean = strings.TrimPrefix(clean, "/work/")
	} else {
		clean = strings.TrimPrefix(clean, "/")
	}
	abs := filepath.Join(root, filepath.Clean("/"+clean))
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if abs != rootAbs && !strings.HasPrefix(abs, rootAbs+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes workspace root", p)
	}
	return abs, nil
}

// waitForSocket polls until the daemon's Unix socket accepts a connection.
func waitForSocket(sock string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", sock)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("daemon socket %s not ready after %v", sock, timeout)
}

// maxToolOutputTokens resolves the dispatcher's output cap for a run: the
// case's `session.config` override if present, else the harness default (0 =
// agent.DefaultMaxOutputTokens). Cases that exercise the large-output path use
// it to make a modest tool result exceed the cap without generating megabytes
// (docs/tool-output-spill.md).
func maxToolOutputTokens(harnessDefault int, c *Case) int {
	v, ok := c.Session.Config["tools.max_output_tokens"]
	if !ok {
		return harnessDefault
	}
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return harnessDefault
}
