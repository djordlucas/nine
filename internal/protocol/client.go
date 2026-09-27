package protocol

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// CanConnect reports whether a daemon is listening on sock.
func CanConnect(sock string) bool {
	conn, err := net.DialTimeout("unix", sock, 200*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// EnsureDaemon starts the daemon if none is listening on sock.
// binary is the path to the nine executable (typically os.Args[0]).
// Returns the started process if we launched it, or nil if one was already running.
func EnsureDaemon(sock, binary string) (*os.Process, error) {
	if CanConnect(sock) {
		return nil, nil
	}
	cmd := exec.Command(binary, "daemon")
	// Leave Stdout/Stderr nil so the daemon's go to os.DevNull rather than
	// being inherited from whoever started it. The caller is usually the TUI,
	// which is about to take over that same terminal with an alt screen — a
	// daemon writing there paints its boot log over the UI. That is not
	// hypothetical: with file logging disabled (NINE_LOG_FILE=off, the
	// container default) slog *is* stderr, so every boot line lands in the
	// chat area. The daemon owns its log destination; the terminal is the
	// client's, and the Setsid below already says this process is detached
	// from it.
	//
	// The daemon's own logging is unaffected — see setupLogger in
	// cmd/nine/main.go for where its output actually goes.
	//
	// Setsid puts it in a new session, so closing the terminal does not send it
	// SIGHUP.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	for range 50 {
		time.Sleep(100 * time.Millisecond)
		if CanConnect(sock) {
			return cmd.Process, nil
		}
	}
	cmd.Process.Kill() //nolint:errcheck
	// Discarding the daemon's output above means the reason is not on this
	// terminal, so say where it is rather than leaving a bare timeout.
	return nil, fmt.Errorf("daemon did not start within 5s; check its log (see NINE_LOG_FILE in docs/configuration.md) or run %q in the foreground to see why", binary+" daemon")
}

// Client is a connection to a running daemon.
type Client struct {
	conn    net.Conn
	scanner *bufio.Scanner
	enc     *json.Encoder
}

// Connect dials the daemon's Unix socket.
func Connect(socketPath string) (*Client, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, err
	}
	return &Client{
		conn:    conn,
		scanner: bufio.NewScanner(conn),
		enc:     json.NewEncoder(conn),
	}, nil
}

// Close closes the connection.
func (c *Client) Close() error { return c.conn.Close() }

// NewConversation asks the daemon to create a new, non-interactive
// conversation and returns its ID.
func (c *Client) NewConversation() (string, error) {
	id, _, _, err := c.NewConversationInteractive(false)
	return id, err
}

// NewConversationInteractive creates a conversation, marking it interactive
// (HITL-eligible) when interactive is true. Only the TUI sets this. Returns the
// new conversation's ID, its resolved role, and the daemon's instance name.
func (c *Client) NewConversationInteractive(interactive bool) (id, role, instanceName string, err error) {
	if err := c.send(Msg{Type: TypeNewConversation, Interactive: interactive}); err != nil {
		return "", "", "", err
	}
	reply, err := c.recv()
	if err != nil {
		return "", "", "", err
	}
	if reply.Type == TypeError {
		return "", "", "", fmt.Errorf("daemon: %s", reply.Text)
	}
	if reply.Type != TypeConversationID {
		return "", "", "", fmt.Errorf("unexpected reply: %s", reply.Type)
	}
	return reply.ID, reply.Role, reply.InstanceName, nil
}

// Attach reconnects to an existing conversation and returns an AttachResult
// with the resolved agent ID, display name, any buffered tool events from while
// no client was connected, and the last completed response if available.
func (c *Client) Attach(agentID string) (AttachResult, error) {
	if err := c.send(NewAttachMsg(agentID)); err != nil {
		return AttachResult{}, err
	}
	reply, err := c.recv()
	if err != nil {
		return AttachResult{}, err
	}
	if reply.Type == TypeError {
		return AttachResult{}, fmt.Errorf("daemon: %s", reply.Text)
	}
	if reply.Type != TypeOK {
		return AttachResult{}, fmt.Errorf("unexpected reply: %s", reply.Type)
	}
	resolved := reply.AgentID
	if resolved == "" {
		resolved = agentID
	}
	return AttachResult{
		AgentID:         resolved,
		Name:            reply.Name,
		InstanceName:    reply.InstanceName,
		Role:            reply.Role,
		ReplayEvents:    reply.ReplayEvents,
		PendingResponse: reply.PendingResponse,
		History:         reply.History,
	}, nil
}

// Turn sends a user message and returns the assistant's response.
// Progress events are silently discarded; use TurnWithProgress to receive them.
func (c *Client) Turn(agentID, text string) (string, error) {
	return c.TurnWithProgress(agentID, text, nil)
}

// TurnWithProgress sends a user message and calls onProgress (if non-nil) for
// each tool event received before the final response.
func (c *Client) TurnWithProgress(agentID, text string, onProgress func(ProgressEvent)) (string, error) {
	return c.turnWithProgress(agentID, text, false, onProgress)
}

// TurnForced is like TurnWithProgress but forces native thinking / the analysis
// pass for this turn (the /think command).
func (c *Client) TurnForced(agentID, text string, onProgress func(ProgressEvent)) (string, error) {
	return c.turnWithProgress(agentID, text, true, onProgress)
}

func (c *Client) turnWithProgress(agentID, text string, forceThink bool, onProgress func(ProgressEvent)) (string, error) {
	msg := NewUserTurnMsg(agentID, text)
	msg.ForceThink = forceThink
	if err := c.send(msg); err != nil {
		return "", err
	}

	for {
		msg, err := c.recv()
		if err != nil {
			return "", err
		}
		if evt, ok := msg.ToProgressEvent(); ok {
			if onProgress != nil {
				onProgress(evt)
			}
			continue
		}
		switch msg.Type {
		case TypeResponse:
			// Consume the trailing "done" message.
			if done, err := c.recv(); err != nil {
				return "", err
			} else if done.Type == TypeError {
				return "", fmt.Errorf("daemon: %s", done.Text)
			}
			return msg.Text, nil
		case TypeError:
			return "", fmt.Errorf("daemon: %s", msg.Text)
		default:
			return "", fmt.Errorf("unexpected reply: %s", msg.Type)
		}
	}
}

// AnswerHuman delivers a human's answer to a pending ask_human request. It is
// sent on its own connection (the turn's connection is blocked reading the
// in-flight stream), and returns once the daemon acknowledges.
func (c *Client) AnswerHuman(agentID, requestID, answer string) error {
	if err := c.send(NewHumanInputAnswerMsg(agentID, requestID, answer)); err != nil {
		return err
	}
	reply, err := c.recv()
	if err != nil {
		return err
	}
	return expectReply(reply, TypeHumanInputAnswer)
}

// QueueTurn sends a user turn on a fresh connection while the main turn stream
// is in flight. The daemon detects the worker is busy and queues the message
// (docs/queued-messages.md). Returns the notice text from the daemon, or an
// error. Unlike TurnWithProgress, this does not wait for a full response — the
// daemon sends a notice when the message is queued.
func (c *Client) QueueTurn(agentID, text string) (string, error) {
	if err := c.send(NewUserTurnMsg(agentID, text)); err != nil {
		return "", err
	}
	for {
		msg, err := c.recv()
		if err != nil {
			return "", err
		}
		switch msg.Type {
		case TypeNotice:
			return msg.Text, nil
		case TypeError:
			return "", fmt.Errorf("daemon: %s", msg.Text)
		}
	}
}

// Status requests daemon status information.
func (c *Client) Status() (*StatusInfo, error) {
	if err := c.send(NewQueryMsg(TypeStatus)); err != nil {
		return nil, err
	}
	reply, err := c.recv()
	if err != nil {
		return nil, err
	}
	if err := expectReply(reply, TypeStatus); err != nil {
		return nil, err
	}
	var info StatusInfo
	if err := json.Unmarshal([]byte(reply.Text), &info); err != nil {
		return nil, fmt.Errorf("decode status: %w", err)
	}
	return &info, nil
}

// ListGoals requests the goal list from the daemon. Returns raw JSON.
func (c *Client) ListGoals() (string, error) {
	return c.queryList(TypeListGoals)
}

// ListNotifications requests the human-facing notification feed. When all is
// true the full history is returned and nothing is marked seen; otherwise only
// unseen entries are returned and they are marked seen. Returns raw JSON.
func (c *Client) ListNotifications(all bool) (string, error) {
	m := NewQueryMsg(TypeListNotifications)
	if all {
		m.Text = "--all"
	}
	if err := c.send(m); err != nil {
		return "", err
	}
	reply, err := c.recv()
	if err != nil {
		return "", err
	}
	if err := expectReply(reply, TypeListNotifications); err != nil {
		return "", err
	}
	return reply.Text, nil
}

// ListWorkflows requests the workflow list from the daemon. Returns raw JSON.
func (c *Client) ListWorkflows() (string, error) {
	return c.queryList(TypeListWorkflows)
}

// StopWorkflow sends a workflow_stop message to cancel an active workflow.
func (c *Client) StopWorkflow(id string) error {
	if err := c.send(NewWorkflowStopMsg(id)); err != nil {
		return err
	}
	reply, err := c.recv()
	if err != nil {
		return err
	}
	return expectReply(reply, TypeWorkflowStop)
}

// FailWorkflow sends a workflow_fail message to mark workflow(s) as failed.
func (c *Client) FailWorkflow(id string, all bool) error {
	text := id
	if all {
		text = "--all"
	}
	if err := c.send(NewWorkflowFailMsg(text)); err != nil {
		return err
	}
	reply, err := c.recv()
	if err != nil {
		return err
	}
	return expectReply(reply, TypeWorkflowFail)
}

// StopSession terminates a session by ID (or every session when all is true),
// returning the daemon's human-readable outcome message.
func (c *Client) StopSession(id string, all bool) (string, error) {
	if err := c.send(NewSessionStopMsg(id, all)); err != nil {
		return "", err
	}
	reply, err := c.recv()
	if err != nil {
		return "", err
	}
	if err := expectReply(reply, TypeSessionStop); err != nil {
		return "", err
	}
	return reply.Text, nil
}

// ListSessions requests the session roster: every conversation with its age,
// status, journal size, and whether the retention reaper is allowed to take it.
func (c *Client) ListSessions() ([]SessionInfo, error) {
	if err := c.send(NewSessionsListMsg()); err != nil {
		return nil, err
	}
	reply, err := c.recv()
	if err != nil {
		return nil, err
	}
	if err := expectReply(reply, TypeSessionsList); err != nil {
		return nil, err
	}
	var out []SessionInfo
	if err := json.Unmarshal([]byte(reply.Text), &out); err != nil {
		return nil, fmt.Errorf("decode sessions: %w", err)
	}
	return out, nil
}

// DeleteSession erases a session and everything keyed to it, returning the
// daemon's human-readable summary of what was removed.
//
// Distinct from StopSession, which ends a session and keeps its history. There
// is deliberately no all=true here: erasing every session at once should not be
// reachable by a flag.
func (c *Client) DeleteSession(id string) (string, error) {
	if err := c.send(NewSessionDeleteMsg(id)); err != nil {
		return "", err
	}
	reply, err := c.recv()
	if err != nil {
		return "", err
	}
	if err := expectReply(reply, TypeSessionDelete); err != nil {
		return "", err
	}
	return reply.Text, nil
}

// ListStanding requests the standing-run roster.
func (c *Client) ListStanding() ([]StandingInfo, error) {
	if err := c.send(NewStandingListMsg()); err != nil {
		return nil, err
	}
	reply, err := c.recv()
	if err != nil {
		return nil, err
	}
	if err := expectReply(reply, TypeStandingList); err != nil {
		return nil, err
	}
	var out []StandingInfo
	if err := json.Unmarshal([]byte(reply.Text), &out); err != nil {
		return nil, fmt.Errorf("decode standing tools: %w", err)
	}
	return out, nil
}

// ShowStanding requests one standing run in detail, with up to n recent log
// lines.
func (c *Client) ShowStanding(id string, n int) (StandingInfo, error) {
	if err := c.send(NewStandingShowMsg(id, n)); err != nil {
		return StandingInfo{}, err
	}
	reply, err := c.recv()
	if err != nil {
		return StandingInfo{}, err
	}
	if err := expectReply(reply, TypeStandingShow); err != nil {
		return StandingInfo{}, err
	}
	var out StandingInfo
	if err := json.Unmarshal([]byte(reply.Text), &out); err != nil {
		return StandingInfo{}, fmt.Errorf("decode standing tool: %w", err)
	}
	return out, nil
}

// ControlStanding stops or starts a standing run; action is "stop" or "start".
// Returns the daemon's human-readable outcome.
func (c *Client) ControlStanding(id, action string) (string, error) {
	if err := c.send(NewStandingControlMsg(id, action)); err != nil {
		return "", err
	}
	reply, err := c.recv()
	if err != nil {
		return "", err
	}
	if err := expectReply(reply, TypeStandingControl); err != nil {
		return "", err
	}
	return reply.Text, nil
}

// CallTool invokes one tool once, for testing, and returns the raw envelope
// JSON — including a `continue` a resumable tool produced, since that is
// exactly what is being debugged.
func (c *Client) CallTool(tool string, args json.RawMessage, liveState bool) (string, error) {
	if err := c.send(NewToolCallMsg(tool, args, liveState)); err != nil {
		return "", err
	}
	reply, err := c.recv()
	if err != nil {
		return "", err
	}
	if err := expectReply(reply, TypeToolCall); err != nil {
		return "", err
	}
	return reply.Text, nil
}

func (c *Client) queryList(msgType MsgType) (string, error) {
	if err := c.send(NewQueryMsg(msgType)); err != nil {
		return "", err
	}
	reply, err := c.recv()
	if err != nil {
		return "", err
	}
	if err := expectReply(reply, msgType); err != nil {
		return "", err
	}
	return reply.Text, nil
}

// Context requests a breakdown of the session's assembled context as raw JSON
// (a ninectx.Report). It performs no LLM call. The caller unmarshals the result.
func (c *Client) Context(agentID string) (string, error) {
	if err := c.send(NewContextMsg(agentID)); err != nil {
		return "", err
	}
	reply, err := c.recv()
	if err != nil {
		return "", err
	}
	if err := expectReply(reply, TypeContext); err != nil {
		return "", err
	}
	return reply.Text, nil
}

// ListTools requests all tool definitions from all loaded plugins.
func (c *Client) ListTools() ([]ToolSummary, error) {
	if err := c.send(NewQueryMsg(TypeListTools)); err != nil {
		return nil, err
	}
	reply, err := c.recv()
	if err != nil {
		return nil, err
	}
	if err := expectReply(reply, TypeListTools); err != nil {
		return nil, err
	}
	var tools []ToolSummary
	if err := json.Unmarshal([]byte(reply.Text), &tools); err != nil {
		return nil, fmt.Errorf("decode tools: %w", err)
	}
	return tools, nil
}

// ListPlugins requests the daemon's plugin roster: built-in and user plugins,
// each with its tools, plus any user plugins that were skipped with a reason.
func (c *Client) ListPlugins() ([]PluginStatus, error) {
	return c.pluginStatusQuery(TypePluginsList)
}

// ReloadPlugins asks the daemon to re-scan the user-plugin directory and reload
// it, returning the resulting roster.
func (c *Client) ReloadPlugins() ([]PluginStatus, error) {
	return c.pluginStatusQuery(TypePluginsReload)
}

// ListSandboxedTools requests the sandboxed-tool roster: every tool loaded from
// [tools].user_dir with its resolved capabilities, plus any that were skipped
// with the reason.
func (c *Client) ListSandboxedTools() ([]SandboxedToolStatus, error) {
	return c.sandboxedToolQuery(TypeToolsList)
}

// ReloadSandboxedTools asks the daemon to re-scan the sandboxed-tool directory
// and reload it, returning the resulting roster.
func (c *Client) ReloadSandboxedTools() ([]SandboxedToolStatus, error) {
	return c.sandboxedToolQuery(TypeToolsReload)
}

func (c *Client) sandboxedToolQuery(msgType MsgType) ([]SandboxedToolStatus, error) {
	if err := c.send(NewQueryMsg(msgType)); err != nil {
		return nil, err
	}
	reply, err := c.recv()
	if err != nil {
		return nil, err
	}
	if err := expectReply(reply, msgType); err != nil {
		return nil, err
	}
	var tools []SandboxedToolStatus
	if err := json.Unmarshal([]byte(reply.Text), &tools); err != nil {
		return nil, fmt.Errorf("decode sandboxed tools: %w", err)
	}
	return tools, nil
}

func (c *Client) pluginStatusQuery(msgType MsgType) ([]PluginStatus, error) {
	if err := c.send(NewQueryMsg(msgType)); err != nil {
		return nil, err
	}
	reply, err := c.recv()
	if err != nil {
		return nil, err
	}
	if err := expectReply(reply, msgType); err != nil {
		return nil, err
	}
	var plugins []PluginStatus
	if err := json.Unmarshal([]byte(reply.Text), &plugins); err != nil {
		return nil, fmt.Errorf("decode plugins: %w", err)
	}
	return plugins, nil
}

// PluginCall calls a plugin tool directly, bypassing the LLM agent.
func (c *Client) PluginCall(tool string, args json.RawMessage) (string, error) {
	if err := c.send(NewPluginCallMsg(tool, args)); err != nil {
		return "", err
	}
	reply, err := c.recv()
	if err != nil {
		return "", err
	}
	if err := expectReply(reply, TypePluginCall); err != nil {
		return "", err
	}
	return reply.Text, nil
}

// expectReply validates a reply before a caller reads its payload: a daemon
// error becomes a Go error, and anything other than want is refused.
//
// Every daemon handler answers a request by echoing its type back with the
// payload in Text (R-PROTO.10), so the expected type is always known. Checking
// it matters because the alternative is silent: a reply of the wrong type has an
// empty Text, so a method that only screened for "error" would hand its caller a
// successful-looking zero value. That is how a version skew or a routing bug
// reaches a user as blank output rather than as a failure.
func expectReply(reply Msg, want MsgType) error {
	if reply.Type == TypeError {
		return fmt.Errorf("daemon: %s", reply.Text)
	}
	if reply.Type != want {
		return fmt.Errorf("unexpected reply: got %q, want %q", reply.Type, want)
	}
	return nil
}

func (c *Client) send(m Msg) error { return c.enc.Encode(m) }

func (c *Client) recv() (Msg, error) {
	if !c.scanner.Scan() {
		if err := c.scanner.Err(); err != nil {
			return Msg{}, err
		}
		return Msg{}, fmt.Errorf("connection closed")
	}
	var m Msg
	if err := json.Unmarshal(c.scanner.Bytes(), &m); err != nil {
		return Msg{}, err
	}
	return m, nil
}

// SetPlanMode changes the reasoning mode (off | plan-only | always) for a
// session live, returning the daemon's confirmation text.
func (c *Client) SetPlanMode(agentID, mode string) (string, error) {
	if err := c.send(NewSetPlanModeMsg(agentID, mode)); err != nil {
		return "", err
	}
	reply, err := c.recv()
	if err != nil {
		return "", err
	}
	if err := expectReply(reply, TypeSetPlanMode); err != nil {
		return "", err
	}
	return reply.Text, nil
}

// ListCapabilities requests the generated tier's capability picture: every grant
// in force with its source, and every request the agent has made.
func (c *Client) ListCapabilities() (CapabilityState, error) {
	if err := c.send(NewQueryMsg(TypeGrantsList)); err != nil {
		return CapabilityState{}, err
	}
	reply, err := c.recv()
	if err != nil {
		return CapabilityState{}, err
	}
	if err := expectReply(reply, TypeGrantsList); err != nil {
		return CapabilityState{}, err
	}
	var state CapabilityState
	if err := json.Unmarshal([]byte(reply.Text), &state); err != nil {
		return CapabilityState{}, fmt.Errorf("decode capability state: %w", err)
	}
	return state, nil
}

// DecideCapability settles a capability request ("approve"/"deny") or revokes a
// grant ("revoke"), returning the daemon's description of what happened.
//
// An approval takes effect on the running daemon: the ceiling widens and the
// generated catalog is re-projected against it, so a tool that previously could
// not load becomes callable on the next turn without a restart.
func (c *Client) DecideCapability(id, action string) (string, error) {
	if err := c.send(NewGrantsDecideMsg(id, action)); err != nil {
		return "", err
	}
	reply, err := c.recv()
	if err != nil {
		return "", err
	}
	if err := expectReply(reply, TypeGrantsDecide); err != nil {
		return "", err
	}
	return reply.Text, nil
}
