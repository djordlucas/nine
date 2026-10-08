package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"nine/internal/agent"
	"nine/internal/builtins"
	"nine/internal/config"
	"nine/internal/embed"
	"nine/internal/llm"
	"nine/internal/memory"
	"nine/internal/plugin"
	"nine/internal/protocol"
	"nine/internal/runtime"
	"nine/internal/toolvm"
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
// execute concurrently on one process because the pursue routine handler is a
// package global (see Run); the runner executes them sequentially.
type Harness struct {
	// NineBin is the path to a built nine binary. Its built-in plugins
	// (shell/files/http/time) are started as `nine plugin serve <name>` child
	// processes, the same way the daemon starts them. Empty disables plugins,
	// leaving only the in-process core tools (memory_*, file_*, skill_*) —
	// enough for many cases and all harness self-tests.
	//
	// This is a binary, not the [plugins].bin directory: the Go built-ins no
	// longer ship as separate executables (internal/builtins).
	NineBin string
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
	live, err := h.start(ctx, c, provider)
	if err != nil {
		return nil, err
	}
	r := live.Result
	defer func() {
		if err != nil {
			r.Close()
			res = nil
		}
	}()

	// 6b. A background case's processes, reconciled as the daemon does at boot.
	if len(c.Setup.Processes) > 0 {
		if err := startProcesses(ctx, live, c.Setup.Processes); err != nil {
			return nil, fmt.Errorf("setup processes: %w", err)
		}
	}

	// Snapshot goal count before the prompts, so grading can count goals the run
	// itself created (setup may pre-seed some).
	if goals, gerr := live.Result.Store.GoalList(); gerr == nil {
		r.PreGoals = len(goals)
	}

	// 7. Drive the prompts on one conversation. A background case may have none.
	if len(c.Prompts) > 0 {
		client, err := protocol.Connect(live.Sock)
		if err != nil {
			return nil, err
		}
		r.cleanups = append(r.cleanups, func() { client.Close() }) //nolint:errcheck

		agentID, _, _, err := client.NewConversationInteractive(c.Session.Interactive)
		if err != nil {
			return nil, err
		}
		r.AgentID = agentID

		answers, err := driveTurns(ctx, live.Sock, client, agentID, c)
		if err != nil {
			return nil, err
		}
		r.Answers = answers
	}

	// 7b. Give background work its time, then grade the session it names.
	if c.Wait != nil {
		waitFor(ctx, r, c.Wait)
		if c.Wait.Session != "" {
			r.AgentID = c.Wait.Session
		}
	}

	// 8. Drain the journal, then read it back for grading.
	live.CloseJournal()
	if r.AgentID != "" {
		events, err := r.Store.SessionEventsByAgent(r.AgentID)
		if err != nil {
			return nil, fmt.Errorf("read journal: %w", err)
		}
		r.Events = events
	}

	return r, nil
}

// startProcesses reconciles a case's [[process]] blocks the way cmd/nine does
// at boot: validated by the configuration's own rules, goal blocks becoming a
// config-owned goal and its pursue process, the rest written by
// ReconcileProcesses; then the runner is asked for a pass.
func startProcesses(ctx context.Context, lv *live, setup []ProcessSetup) error {
	blocks := make([]config.ProcessConfig, 0, len(setup))
	for _, p := range setup {
		blocks = append(blocks, config.ProcessConfig{
			Name: p.Name, Tool: p.Tool, Every: p.Every, Schedule: p.Schedule, Goal: p.Goal,
			Role: p.Role, Delegates: p.Delegates, Session: p.Session, ReportTo: p.ReportTo, Args: p.Args,
			Budget: config.BudgetConfig{TurnsPerDay: p.Budget.TurnsPerDay, TokensPerDay: p.Budget.TokensPerDay},
		})
	}
	if err := (&config.Config{Process: blocks}).Validate(); err != nil {
		return err
	}
	// A case may supply its own tools under the workspace's .tools directory.
	lv.Tools.Load(ctx, nil)

	store := lv.Result.Store
	runtime.ReconcileProcesses(store, lv.Tools, blocks)
	for _, p := range setup {
		if p.Goal == "" {
			if err := applyStartState(store, p.Name, p); err != nil {
				return err
			}
		}
	}
	for i, b := range blocks {
		if b.Goal == "" {
			continue
		}
		var every time.Duration
		if b.Every != "" {
			d, err := time.ParseDuration(b.Every)
			if err != nil {
				return err
			}
			every = d
		}
		role := b.Role
		if role == "" {
			role = "monitor"
		}
		if err := store.GoalCreate(b.Name, b.Goal, "", runtime.ConfigGoalOrigin); err != nil {
			return err
		}
		if _, err := lv.Daemon.SpawnStandingSession(ctx, b.Name, role, b.Delegates, every, b.Schedule, b.Budget); err != nil {
			return err
		}
		// At once, so a process a case starts stopped does not count against
		// max_running when the next block is spawned.
		if err := applyStartState(store, "goal:"+b.Name, setup[i]); err != nil {
			return err
		}
	}
	lv.Processes.Wake()
	return nil
}

// applyStartState puts process id in the state the case starts it in: its
// goal paused, or stopped by a given party, before the runner's first pass.
func applyStartState(store *memory.Store, id string, p ProcessSetup) error {
	stoppedBy := p.StoppedBy
	if p.GoalStatus != "" {
		if err := store.GoalUpdateStatus(p.Name, p.GoalStatus); err != nil {
			return fmt.Errorf("process %s: goal status: %w", p.Name, err)
		}
		if p.GoalStatus != "active" && stoppedBy == "" {
			stoppedBy = "goal"
		}
	}
	if stoppedBy == "" {
		return nil
	}
	if stoppedBy == "budget" {
		if err := store.ProcessUsageAdd(id, 0); err != nil {
			return fmt.Errorf("process %s: start a budget day: %w", id, err)
		}
	}
	if ok, err := store.ProcessStop(id, stoppedBy); err != nil || !ok {
		return fmt.Errorf("process %s: stop by %s: ok=%v err=%v", id, stoppedBy, ok, err)
	}
	return nil
}

// waitFor returns once every side effect in w.Until holds, checked the way the
// grader checks them, or once w.Seconds have passed.
func waitFor(ctx context.Context, r *RunResult, w *WaitSpec) {
	probe := &Case{Expect: Expect{SideEffects: w.Until}}
	deadline := time.Now().Add(time.Duration(w.Seconds) * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		g := &grader{c: probe, res: r}
		g.gradeSideEffects()
		if len(g.failures) == 0 {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// live is a started daemon for one case, before any conversation: the production
// assembly over the case's isolated store, workspace, plugins and tool host.
type live struct {
	Result *RunResult
	Daemon *runtime.Daemon
	Tools  *toolvm.Host
	// Processes is the process runner the harness started.
	Processes *runtime.StandingRunner
	Sock      string
	// CloseJournal drains the event sink so the journal can be read; idempotent.
	CloseJournal func()
}

// start builds and starts the daemon for c (steps 1–6 of Run). The caller drives
// it and closes live.Result when done; on error everything built is torn down.
func (h *Harness) start(ctx context.Context, c *Case, provider llm.Provider) (lv *live, err error) {
	r := &RunResult{}
	// On any setup error, unwind whatever we already built.
	defer func() {
		if err != nil {
			r.Close()
			lv = nil
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

	// 3. Seed the built-in skills, exactly as cmd/nine/daemon.go does at boot,
	//    then layer the case's own fixtures on top (production order: built-ins,
	//    then user skills).
	//
	//    Without this the eval world had *no* built-in skills at all: `nine`
	//    ships a dozen, production seeds and embeds them on every boot, and a
	//    case saw none of them. That silently put the whole built-in corpus
	//    beyond reach of the suite — no case could exercise the guidance in
	//    web-research, git-workflow, or any other, which is precisely the
	//    knowledge those skills exist to carry. It also left the `skills` vector
	//    namespace empty, so skill_search had nothing to rank unless the case's
	//    own model wrote something first.
	if err := runtime.SeedSkills(store, h.Embedder); err != nil {
		return nil, fmt.Errorf("seed built-in skills: %w", err)
	}

	// 4. Apply setup fixtures to the fresh store + workspace.
	if err := applySetup(store, h.Embedder, workspace, c.Setup); err != nil {
		return nil, fmt.Errorf("apply setup: %w", err)
	}

	// 5. Plugins (optional). Each gets the workspace as its root so file writes
	//    and setup.files line up. A failed start is logged and skipped by
	//    TryStartBuiltin.
	pluginMgr := plugin.NewManager("")
	if h.NineBin != "" {
		// os.Executable() here is the test binary, which has no `plugin serve`
		// subcommand — point the manager at the nine binary under test instead.
		pluginMgr.SetBuiltinBinary(h.NineBin)
		// shell is the only built-in plugin left; time, files and http are shipped
		// sandboxed tools now, loaded by the toolvm host above.
		pluginMgr.TryStartBuiltin("shell")
		// 5b. MCP servers the case declares, mirroring startMCPServers in
		//     cmd/nine/daemon.go. This is not an optional extra: a capability Nine
		//     does not implement itself now arrives this way and no other, so a
		//     harness that skipped it could not exercise a browser, or any other
		//     MCP-provided tool, at all.
		startCaseMCPServers(pluginMgr, c.Setup.MCPServers)
	}
	r.cleanups = append(r.cleanups, func() { pluginMgr.StopAll() }) //nolint:errcheck

	// 6. Assemble the daemon core — the same wiring cmd/nine/daemon.go uses, minus
	//    the production-only bootstrap (resume, standing agents, self-reflection,
	//    subscribers). The store, plugins, provider queue, and per-case knobs are
	//    injected; the runner serializes runs because Assemble registers the
	//    pursue routine handler over this run's store (a package global).
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

	// The sandboxed-tool host, with the case's workspace as the mount for shipped
	// tools that declare fs. Cases used to get read_file/write_file from the
	// `files` plugin; those are shipped tools now, so without this a case that
	// touches a file has no tool to do it with — and eval-replay would not catch
	// it, because the file cases are Track L.
	toolHost, terr := toolvm.Open(ctx, toolvm.Config{UserDir: filepath.Join(workspace, ".tools")})
	if terr != nil {
		return nil, fmt.Errorf("open sandboxed tool host: %w", terr)
	}
	// The generated tier (tool_write/tool_delete/js_eval) is off unless a case opts
	// in with session.config "tools.agent.enabled" (and "tools.agent.eval" for
	// js_eval). That is the one place the harness deliberately does *not* mirror
	// production, where the tier and its workspace ceiling are both on by default:
	// a case must state the reach it exercises, so a case that does not mention
	// generated tools cannot start writing them when a default moves.
	//
	// The policy is installed before any generated tool loads, mirroring
	// OpenSandboxedTools. An opted-in case gets the production default ceiling —
	// the workspace, read and write, at the same guest path shipped tools use — so
	// what it exercises is what a deployment has. Still no deps bundler: a
	// case-written tool is import-free.
	generatedOn := caseBool(c, "tools.agent.enabled")
	agentCfg := toolvm.AgentConfig{Enabled: generatedOn}
	if generatedOn {
		m := []toolvm.Mount{{Host: workspace, Guest: toolvm.ShippedWorkspaceGuest}}
		agentCfg.Ceiling = toolvm.Ceiling{Grant: toolvm.Grant{FSRead: m, FSWrite: m}}
	}
	toolHost.SetAgentConfig(agentCfg)
	toolHost.SetShippedWorkspace(toolvm.ShippedWorkspace{Host: workspace})
	toolHost.LoadShipped(ctx, nil)
	r.cleanups = append(r.cleanups, func() { toolHost.Close(ctx) }) //nolint:errcheck
	var generatedTools agent.GeneratedToolStore
	if generatedOn {
		generatedTools = runtime.NewGeneratedToolStoreWithStanding(store, toolHost, pluginMgr, nil, false, false, 0)
	}
	// Tools Nine wrote in an earlier session, through the store tool_write uses.
	if len(c.Setup.GeneratedTools) > 0 {
		if generatedTools == nil {
			return nil, fmt.Errorf("setup.generated_tools needs session.config tools.agent.enabled")
		}
		for name, g := range c.Setup.GeneratedTools {
			spec := agent.GeneratedToolSpec{
				Name: name, Description: g.Description, Source: g.Source,
				InputSchema: json.RawMessage(orJSON(g.InputSchema, `{"type":"object","properties":{}}`)),
			}
			if g.Capabilities != "" {
				spec.Capabilities = json.RawMessage(g.Capabilities)
			}
			if _, err := generatedTools.Write(ctx, spec); err != nil {
				return nil, fmt.Errorf("seed generated tool %s: %w", name, err)
			}
		}
	}

	sock := filepath.Join(workspace, "d.sock")
	// The workspace index, as the daemon wires it. A case that writes a file and
	// then searches for it is exercising the scan, so the harness has to run one
	// — with a short interval, since an eval case is over in seconds.
	workspaceScanner := runtime.NewWorkspaceScanner(store, workspace,
		runtime.DefaultIndexMaxFileBytes, runtime.DefaultIndexMaxFiles, time.Second)
	workspaceScanner.ScanAll(context.Background())

	asm := runtime.Assemble(runtime.AssemblyConfig{
		Workspace:           runtime.NewWorkspaceBackend(store, workspaceScanner, workspace),
		WorkspaceRoot:       workspace,
		Tools:               toolHost,
		GeneratedTools:      generatedTools,
		GeneratedEval:       generatedOn && caseBool(c, "tools.agent.eval"),
		SocketPath:          sock,
		Store:               store,
		Plugins:             pluginMgr,
		Embedder:            h.Embedder,
		ContextBudget:       budget,
		MaxToolOutputTokens: maxToolOutputTokens(h.MaxToolOutputTokens, c),
		SystemPrompt:        runtime.BuildSystemPrompt(),
		RelatedSessions:     h.Embedder != nil,
		SurfaceMemories:     h.Embedder != nil,
		Queue:               llm.NewQueue(provider, 4),
		TaskTimeoutSeconds:  c.TimeoutSecs,
		HITL:                hitl,
		DefaultLeafRole:     "executor",
		MaxRunning:          caseInt(c, "processes.max_running", runtime.DefaultMaxRunning),
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
	// The process runner, as production starts it: goal sessions, standing
	// agents and standing tools all run through it (adr/process-sessions.md).
	// A short poll, since an eval case is over in seconds.
	go runtime.RunStandingTools(dctx, asm.Processes, 200*time.Millisecond)
	go daemon.Start(dctx) //nolint:errcheck
	r.cleanups = append(r.cleanups, func() { dcancel(); daemon.Stop() })
	if err := waitForSocket(sock, 5*time.Second); err != nil {
		return nil, err
	}

	return &live{Result: r, Daemon: daemon, Tools: toolHost, Processes: asm.Processes, Sock: sock, CloseJournal: closeSink}, nil
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
			if evt.Type != protocol.TypeHumanInputRequired || evt.HumanRequest == nil {
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
// embedder may be nil, in which case seeded skills are stored but not indexed —
// and so are not reachable by skill_search.
func applySetup(store *memory.Store, embedder embed.Embedder, workspace string, s Setup) error {
	for k, v := range s.KV {
		if err := store.Set(k, v); err != nil {
			return fmt.Errorf("seed kv %q: %w", k, err)
		}
	}
	for path, content := range s.StoredFiles {
		if err := store.FileStore(path, content); err != nil {
			return fmt.Errorf("seed stored file %q: %w", path, err)
		}
	}
	for name, sk := range s.Skills {
		if err := store.SkillUpsert(memory.Skill{
			Name:        name,
			Description: sk.Description,
			Tags:        sk.Tags,
			Content:     sk.Content,
			Source:      "eval",
		}); err != nil {
			return fmt.Errorf("seed skill %q: %w", name, err)
		}
		// Index it the same way skill_write does, or the skill exists but no
		// skill_search can find it.
		if embedder != nil && sk.Description != "" {
			vec, err := embedder.Embed(context.Background(), sk.Description)
			if err != nil {
				return fmt.Errorf("embed seeded skill %q: %w", name, err)
			}
			if err := store.VectorStore("skills:"+name, "skills", name, vec); err != nil {
				return fmt.Errorf("index seeded skill %q: %w", name, err)
			}
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
// (adr/tool-output-spill.md).
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

// caseInt reads an integer session.config override, or def when it is absent.
func caseInt(c *Case, key string, def int) int {
	switch v := c.Session.Config[key].(type) {
	case int:
		return v
	case float64:
		return int(v)
	}
	return def
}

// caseBool reads a boolean session.config override; absent or non-bool is false.
func caseBool(c *Case, key string) bool {
	b, _ := c.Session.Config[key].(bool)
	return b
}

// startCaseMCPServers brings up one `mcp` bridge per server the case declares.
//
// It hand-mirrors startMCPServers in cmd/nine/daemon.go — same spec encoding,
// same instance naming, same concurrent start with a wait — so a case exercises
// the path production uses rather than a shortcut. The wait matters for the same
// reason it does there: the tool registry has to be complete before the first
// turn, or the model is asked to use a tool that has not registered yet and the
// case fails as a model error.
//
// A server that fails to start is logged by TryStartBuiltinInstance and skipped,
// leaving its tools absent — which surfaces as the case failing its trajectory
// assertion, naming the tool that never arrived.
func startCaseMCPServers(mgr *plugin.Manager, servers []MCPServerSetup) {
	var wg sync.WaitGroup
	for _, srv := range servers {
		spec, err := json.Marshal(map[string]any{
			"name":    srv.Name,
			"command": os.ExpandEnv(srv.Command),
			"args":    expandAll(srv.Args),
			"env":     srv.Env,
		})
		if err != nil {
			// Only unmarshalable values could cause this and these are all
			// strings; skip rather than abort the run.
			continue
		}
		instance := plugin.MCPInstanceName(srv.Name)
		wg.Add(1)
		go func() {
			defer wg.Done()
			mgr.TryStartBuiltinInstance(builtins.MCPBuiltinName, instance, "NINE_MCP_SERVER="+string(spec))
		}()
	}
	wg.Wait()
}

// expandAll applies environment expansion to each argument, so a case can point
// at a fixture the suite builds at run time without hardcoding a temp path.
func expandAll(args []string) []string {
	if len(args) == 0 {
		return nil
	}
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = os.ExpandEnv(a)
	}
	return out
}

func orJSON(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
