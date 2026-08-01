// Package cli implements the nine command-line interface. Each subcommand is a
// method on CLI so output destinations and the daemon binary path are injectable
// for testing.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"nine/internal/config"
	ninectx "nine/internal/context"
	"nine/internal/memory"
	"nine/internal/protocol"
)

// CLI executes nine client commands, writing output to Out/Err and reading
// interactive input from In.
type CLI struct {
	Out    io.Writer
	Err    io.Writer
	In     io.Reader
	Binary string // path to the nine binary, used to auto-start the daemon

	// Version is the release version, printed by `nine version`. Set by the caller.
	Version string

	// StartDaemon launches the daemon process. Set by the caller.
	StartDaemon func()
	// StartTUI opens the terminal UI, optionally attaching to attachID.
	StartTUI func(attachID string) error
}

// New returns a CLI wired to the standard streams.
func New(binary string) *CLI {
	return &CLI{Out: os.Stdout, Err: os.Stderr, In: os.Stdin, Binary: binary}
}

// Run dispatches args to the appropriate subcommand. An empty args slice
// launches the TUI. Returns a non-nil error on failure or unknown command.
func (c *CLI) Run(args []string, cfg *config.Config) error {
	if len(args) == 0 {
		return c.StartTUI("")
	}

	switch args[0] {
	case "help", "--help", "-h":
		return c.Help()
	case "docs":
		return c.Docs(topicArg(args))
	case "spec":
		return c.Spec(topicArg(args))
	case "version", "--version", "-v":
		v := c.Version
		if v == "" {
			v = "dev"
		}
		fmt.Fprintln(c.Out, v)
		return nil
	case "daemon":
		c.StartDaemon()
		return nil
	case "goals":
		return c.Goals(cfg)
	case "reflections":
		return c.Reflections(cfg)
	case "notifications":
		all := len(args) > 1 && args[1] == "--all"
		return c.Notifications(cfg, all)
	case "workflows":
		return c.Workflows(cfg)
	case "workflow":
		if len(args) < 2 {
			return fmt.Errorf("usage: nine workflow <stop|fail> [id|--all]")
		}
		switch args[1] {
		case "stop":
			if len(args) < 3 {
				return fmt.Errorf("usage: nine workflow stop <id>")
			}
			return c.WorkflowStop(cfg, args[2])
		case "fail":
			if len(args) < 3 {
				return fmt.Errorf("usage: nine workflow fail <id|--all>")
			}
			all := args[2] == "--all"
			id := ""
			if !all {
				id = args[2]
			}
			return c.WorkflowFail(cfg, id, all)
		default:
			return fmt.Errorf("unknown workflow command: %s", args[1])
		}
	case "send":
		// Non-interactive one-shot turn, for scripting and verification.
		// Usage: nine send [--id <agent-id>] <message...>
		rest := args[1:]
		id := ""
		if len(rest) >= 2 && rest[0] == "--id" {
			id = rest[1]
			rest = rest[2:]
		}
		if len(rest) == 0 {
			return fmt.Errorf("usage: nine send [--id <agent-id>] <message>")
		}
		return c.Send(cfg, id, strings.Join(rest, " "))
	case "skills":
		if len(args) < 2 || args[1] != "validate" {
			return fmt.Errorf("usage: nine skills validate [path]")
		}
		p := ""
		if len(args) > 2 {
			p = args[2]
		}
		return c.SkillValidate(cfg, p)
	case "plugins":
		if len(args) > 1 {
			if args[1] == "reload" {
				return c.PluginsReload(cfg)
			}
			return fmt.Errorf("usage: nine plugins [reload]")
		}
		return c.Plugins(cfg)
	case "plugin":
		if len(args) < 2 || args[1] != "validate" {
			return fmt.Errorf("usage: nine plugin validate [path]")
		}
		p := ""
		if len(args) > 2 {
			p = args[2]
		}
		return c.PluginValidate(cfg, p)
	case "status":
		return c.Status(cfg)
	case "context":
		if len(args) < 2 {
			return fmt.Errorf("usage: nine context <agent-id> [--verbose]")
		}
		verbose, err := parseVerboseFlag(args[2:])
		if err != nil {
			return err
		}
		return c.Context(cfg, args[1], verbose)
	case "trace":
		if len(args) < 2 {
			return fmt.Errorf("usage: nine trace <agent-id> [--turn N] [--sub-agents]")
		}
		turn, subAgents, err := parseTraceFlags(args[2:])
		if err != nil {
			return err
		}
		return c.Trace(cfg, args[1], turn, subAgents)
	case "replay":
		if len(args) < 2 {
			return fmt.Errorf("usage: nine replay <agent-id> --turn N")
		}
		turn, err := parseTurnFlag(args[2:])
		if err != nil {
			return err
		}
		return c.Replay(cfg, args[1], turn)
	case "attach":
		if len(args) < 2 {
			return fmt.Errorf("usage: nine attach <agent-id>")
		}
		return c.StartTUI(args[1])
	case "stop":
		if len(args) < 2 {
			return fmt.Errorf("usage: nine stop <agent-id|--all>")
		}
		all := args[1] == "--all"
		id := ""
		if !all {
			id = args[1]
		}
		return c.StopSession(cfg, id, all)
	default:
		// A flag-shaped first argument (e.g. `nine --foo`, `nine -x`) is almost
		// certainly a mistyped command rather than a message the user wants to
		// send, so show help instead of silently opening a conversation.
		if strings.HasPrefix(args[0], "-") {
			return c.unknownCommand(args[0])
		}
		// A single unrecognized word that closely resembles a command is far more
		// likely a typo than a one-word message (e.g. `nine staus` → `status`).
		// Surface the error and help rather than silently spending a turn on it.
		if len(args) == 1 {
			if _, ok := nearestCommand(args[0]); ok {
				return c.unknownCommand(args[0])
			}
		}
		// Anything else is treated as a message on the CLI's persistent default
		// conversation (`nine <message>`).
		return c.Message(cfg, strings.Join(args, " "))
	}
}

// knownCommands is the set of top-level subcommands, used to spot a mistyped
// command and suggest the intended one. Kept in sync with the switch in Run.
var knownCommands = []string{
	"help", "docs", "spec", "version", "daemon", "goals", "reflections",
	"notifications", "workflows", "workflow", "send", "skills", "plugins",
	"plugin", "status", "context", "trace", "replay", "attach", "stop",
}

// nearestCommand returns the known command closest to arg and true when arg is a
// likely typo of it. A match requires a small edit distance (≤2) and a shared
// first letter — people rarely fat-finger the first character, and that guard
// keeps unrelated one-word messages (e.g. `deploy`, two edits from `replay`)
// from being mistaken for commands. Returns ok=false for anything that reads
// like a genuine message, so `nine <message>` still works.
func nearestCommand(arg string) (string, bool) {
	if len(arg) < 3 {
		return "", false
	}
	best, bestDist := "", 0
	for _, cmd := range knownCommands {
		if cmd[0] != arg[0] {
			continue
		}
		d := levenshtein(arg, cmd)
		if d == 0 {
			return "", false // exact match is handled by the switch, not here
		}
		if best == "" || d < bestDist {
			best, bestDist = cmd, d
		}
	}
	if best == "" || bestDist > 2 {
		return "", false
	}
	return best, true
}

// levenshtein returns the edit distance between a and b.
func levenshtein(a, b string) int {
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

// unknownCommand reports name as an unrecognized command: a note on stderr —
// with a "did you mean" suggestion when one is close — followed by the usage
// reference on stdout. Returns nil (usage was shown); the note is the error
// surfaced to the user.
func (c *CLI) unknownCommand(name string) error {
	if suggestion, ok := nearestCommand(name); ok {
		fmt.Fprintf(c.Err, "unknown command: %s (did you mean %q?)\n\n", name, suggestion)
	} else {
		fmt.Fprintf(c.Err, "unknown command: %s\n\n", name)
	}
	return c.Help()
}

// topicArg returns the topic name for `nine docs`/`nine spec`, or "" to list.
func topicArg(args []string) string {
	if len(args) > 1 {
		return args[1]
	}
	return ""
}

// parseTurnFlag reads an optional "--turn N" (or "--turn=N") from args,
// returning 0 when absent. Any other argument is an error.
func parseTurnFlag(args []string) (int, error) {
	if len(args) == 0 {
		return 0, nil
	}
	a := args[0]
	switch {
	case a == "--turn":
		if len(args) < 2 {
			return 0, fmt.Errorf("--turn requires a value")
		}
		n, err := strconv.Atoi(args[1])
		if err != nil {
			return 0, fmt.Errorf("invalid --turn value %q", args[1])
		}
		return n, nil
	case strings.HasPrefix(a, "--turn="):
		v := strings.TrimPrefix(a, "--turn=")
		n, err := strconv.Atoi(v)
		if err != nil {
			return 0, fmt.Errorf("invalid --turn value %q", v)
		}
		return n, nil
	default:
		return 0, fmt.Errorf("unexpected argument: %s", a)
	}
}

// parseTraceFlags reads the optional "--turn N" and "--sub-agents" flags for the
// trace command, in any order. Any other argument is an error.
func parseTraceFlags(args []string) (turn int, subAgents bool, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--sub-agents":
			subAgents = true
		case a == "--turn":
			if i+1 >= len(args) {
				return 0, false, fmt.Errorf("--turn requires a value")
			}
			i++
			turn, err = strconv.Atoi(args[i])
			if err != nil {
				return 0, false, fmt.Errorf("invalid --turn value %q", args[i])
			}
		case strings.HasPrefix(a, "--turn="):
			v := strings.TrimPrefix(a, "--turn=")
			turn, err = strconv.Atoi(v)
			if err != nil {
				return 0, false, fmt.Errorf("invalid --turn value %q", v)
			}
		default:
			return 0, false, fmt.Errorf("unexpected argument: %s", a)
		}
	}
	return turn, subAgents, nil
}

// parseVerboseFlag reads an optional "--verbose" / "-v" from args, returning
// false when absent. Any other argument is an error.
func parseVerboseFlag(args []string) (bool, error) {
	for _, a := range args {
		switch a {
		case "--verbose", "-v":
			return true, nil
		default:
			return false, fmt.Errorf("unexpected argument: %s", a)
		}
	}
	return false, nil
}

// connect ensures the daemon is running and returns an open client connection.
func (c *CLI) connect(cfg *config.Config) (*protocol.Client, error) {
	sock := cfg.SocketPath()
	if _, err := protocol.EnsureDaemon(sock, c.Binary); err != nil {
		return nil, fmt.Errorf("start daemon: %w", err)
	}
	cl, err := protocol.Connect(sock)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return cl, nil
}

// Message sends a one-shot turn on the CLI's persistent default conversation,
// continuing the same thread across invocations of `nine <message>`. The
// conversation id is stored under the user's home dir; it survives daemon
// restarts because the daemon revives a checkpointed conversation on a turn. If
// the stored conversation no longer exists (e.g. the store was reset), a fresh
// one is started transparently.
func (c *CLI) Message(cfg *config.Config, msg string) error {
	cl, err := c.connect(cfg)
	if err != nil {
		return err
	}
	defer cl.Close() //nolint:errcheck

	id := loadConversationID()
	if id == "" {
		if id, err = cl.NewConversation(); err != nil {
			return fmt.Errorf("new conversation: %w", err)
		}
	}

	resp, err := cl.Turn(id, msg)
	if err != nil && strings.Contains(err.Error(), "not found") {
		// The stored conversation is gone — start a fresh one and retry once.
		if id, err = cl.NewConversation(); err == nil {
			resp, err = cl.Turn(id, msg)
		}
	}
	if err != nil {
		return fmt.Errorf("turn: %w", err)
	}

	saveConversationID(id)
	fmt.Fprintln(c.Out, resp)
	return nil
}

// conversationStatePath is where the CLI remembers its default conversation id.
func conversationStatePath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".nine", "last-conversation")
}

func loadConversationID() string {
	p := conversationStatePath()
	if p == "" {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func saveConversationID(id string) {
	p := conversationStatePath()
	if p == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o700) //nolint:errcheck // best-effort persistence
	_ = os.WriteFile(p, []byte(id), 0o600)  //nolint:errcheck
}

// Send drives one non-interactive turn for scripting/verification. With an
// empty agentID it opens a fresh conversation (printing "id=<agent-id>" to
// stderr so callers can reuse it); otherwise it continues the given session.
// The assistant's response is printed to stdout.
func (c *CLI) Send(cfg *config.Config, agentID, msg string) error {
	cl, err := c.connect(cfg)
	if err != nil {
		return err
	}
	defer cl.Close() //nolint:errcheck

	if agentID == "" {
		agentID, err = cl.NewConversation()
		if err != nil {
			return fmt.Errorf("new conversation: %w", err)
		}
	}
	fmt.Fprintf(c.Err, "id=%s\n", agentID)
	resp, err := cl.Turn(agentID, msg)
	if err != nil {
		return fmt.Errorf("turn: %w", err)
	}
	fmt.Fprintln(c.Out, resp)
	return nil
}

// Goals fetches and prints the goal list.
func (c *CLI) Goals(cfg *config.Config) error {
	cl, err := c.connect(cfg)
	if err != nil {
		return err
	}
	defer cl.Close() //nolint:errcheck

	raw, err := cl.ListGoals()
	if err != nil {
		return fmt.Errorf("list goals: %w", err)
	}
	printGoals(c.Out, raw)
	return nil
}

// Reflections fetches and prints the reflection history.
func (c *CLI) Reflections(cfg *config.Config) error {
	cl, err := c.connect(cfg)
	if err != nil {
		return err
	}
	defer cl.Close() //nolint:errcheck

	raw, err := cl.ListReflections()
	if err != nil {
		return fmt.Errorf("list reflections: %w", err)
	}
	printReflections(c.Out, raw)
	return nil
}

// Notifications prints the human-facing notification feed posted by background
// agents (docs/predefined-agents.md). With a running daemon it drains unseen
// entries (marking them seen) unless all is set; with the daemon down it reads
// the memory store directly so a finding is visible even with no session
// attached.
func (c *CLI) Notifications(cfg *config.Config, all bool) error {
	sock := cfg.SocketPath()
	if protocol.CanConnect(sock) {
		cl, err := protocol.Connect(sock)
		if err != nil {
			return fmt.Errorf("connect: %w", err)
		}
		defer cl.Close() //nolint:errcheck
		raw, err := cl.ListNotifications(all)
		if err != nil {
			return fmt.Errorf("list notifications: %w", err)
		}
		printNotifications(c.Out, raw)
		return nil
	}

	// Daemon down: read the feed directly from the memory store.
	store, err := memory.Open(cfg.DatabaseURL())
	if err != nil {
		return fmt.Errorf("open memory store: %w", err)
	}
	defer store.Close() //nolint:errcheck

	ns, err := store.UserNotificationList(!all)
	if err != nil {
		return fmt.Errorf("list notifications: %w", err)
	}
	if !all {
		for _, n := range ns {
			store.UserNotificationMarkSeen(n.ID) //nolint:errcheck
		}
	}
	data, _ := json.Marshal(map[string]any{"notifications": ns})
	printNotifications(c.Out, string(data))
	return nil
}

// Workflows fetches and prints the workflow list.
func (c *CLI) Workflows(cfg *config.Config) error {
	cl, err := c.connect(cfg)
	if err != nil {
		return err
	}
	defer cl.Close() //nolint:errcheck

	raw, err := cl.ListWorkflows()
	if err != nil {
		return fmt.Errorf("list workflows: %w", err)
	}
	printWorkflows(c.Out, raw)
	return nil
}

// WorkflowStop cancels an active workflow by ID. Requires a running daemon.
func (c *CLI) WorkflowStop(cfg *config.Config, id string) error {
	sock := cfg.SocketPath()
	if !protocol.CanConnect(sock) {
		return fmt.Errorf("daemon is not running; stop requires a running daemon")
	}
	cl, err := protocol.Connect(sock)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer cl.Close() //nolint:errcheck

	if err := cl.StopWorkflow(id); err != nil {
		return fmt.Errorf("stop workflow: %w", err)
	}
	fmt.Fprintf(c.Out, "workflow %s cancelled\n", id)
	return nil
}

// WorkflowFail marks workflow(s) as failed. If the daemon is down it contacts
// the memory plugin directly so the operation works without a running daemon.
func (c *CLI) WorkflowFail(cfg *config.Config, id string, all bool) error {
	sock := cfg.SocketPath()
	if protocol.CanConnect(sock) {
		cl, err := protocol.Connect(sock)
		if err != nil {
			return fmt.Errorf("connect: %w", err)
		}
		defer cl.Close() //nolint:errcheck
		if err := cl.FailWorkflow(id, all); err != nil {
			return fmt.Errorf("fail workflow: %w", err)
		}
		if all {
			fmt.Fprintln(c.Out, "all active workflows marked as failed")
		} else {
			fmt.Fprintf(c.Out, "workflow %s marked as failed\n", id)
		}
		return nil
	}

	// Daemon is down: open the memory store in-process. (Memory is an in-process
	// package, not a plugin — there is no memory subprocess to contact.)
	store, err := memory.Open(cfg.DatabaseURL())
	if err != nil {
		return fmt.Errorf("open memory store: %w", err)
	}
	defer store.Close() //nolint:errcheck

	updated, err := store.WorkflowFail(id, all)
	if err != nil {
		return fmt.Errorf("fail workflow: %w", err)
	}
	fmt.Fprintf(c.Out, "marked %d workflow(s) as failed\n", updated)
	return nil
}

// Status fetches and prints daemon status.
func (c *CLI) Status(cfg *config.Config) error {
	sock := cfg.SocketPath()
	if !protocol.CanConnect(sock) {
		fmt.Fprintln(c.Out, "no daemon running")
		return nil
	}
	cl, err := protocol.Connect(sock)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer cl.Close() //nolint:errcheck

	info, err := cl.Status()
	if err != nil {
		return fmt.Errorf("status: %w", err)
	}
	printStatus(c.Out, info)
	return nil
}

// StopSession terminates a session by ID, or every active session with all set.
// It requires a running daemon — with none up there are no sessions to stop.
func (c *CLI) StopSession(cfg *config.Config, id string, all bool) error {
	sock := cfg.SocketPath()
	if !protocol.CanConnect(sock) {
		fmt.Fprintln(c.Out, "no daemon running; nothing to stop")
		return nil
	}
	cl, err := protocol.Connect(sock)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer cl.Close() //nolint:errcheck

	msg, err := cl.StopSession(id, all)
	if err != nil {
		return fmt.Errorf("stop session: %w", err)
	}
	fmt.Fprintln(c.Out, msg)
	return nil
}

// Context fetches and prints a session's assembled-context breakdown. It never
// triggers an LLM call — the daemon snapshots the deterministic assembly.
func (c *CLI) Context(cfg *config.Config, agentID string, verbose bool) error {
	sock := cfg.SocketPath()
	if !protocol.CanConnect(sock) {
		fmt.Fprintln(c.Out, "no daemon running")
		return nil
	}
	cl, err := protocol.Connect(sock)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer cl.Close() //nolint:errcheck

	raw, err := cl.Context(agentID)
	if err != nil {
		return fmt.Errorf("context: %w", err)
	}
	var rep ninectx.Report
	if err := json.Unmarshal([]byte(raw), &rep); err != nil {
		return fmt.Errorf("decode context: %w", err)
	}
	printContextReport(c.Out, &rep, verbose)
	return nil
}
