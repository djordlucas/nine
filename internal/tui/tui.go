package tui

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"

	"nine/internal/config"
	"nine/internal/protocol"
)

const (
	headerHeight = 2
	// inputAreaHeight: rule + 1 line of input + 1 line of status (token/time).
	inputAreaHeight = 3

	maxInputDisplay  = 80
	maxOutputDisplay = 200

	// contextWarnPercent mirrors the daemon's contextWarnFraction (0.9,
	// internal/agent): at or above this fraction of the budget the header flags
	// context pressure so the warning persists after the transient `notice`
	// scrolls out of the transcript. A display heuristic, not a shared contract.
	contextWarnPercent = 90
)

// palette holds all TUI styles for a given theme.
type palette struct {
	header       lipgloss.Style
	rule         lipgloss.Style
	you          lipgloss.Style
	nine         lipgloss.Style
	system       lipgloss.Style // slash command output
	spinner      lipgloss.Style
	prompt       lipgloss.Style
	ts           lipgloss.Style
	tool         lipgloss.Style
	arrow        lipgloss.Style
	toolInput    lipgloss.Style
	output       lipgloss.Style
	continuation lipgloss.Style
}

// adaptive returns a lipgloss color that picks Light on light terminals and Dark on dark ones.
func adaptive(light, dark string) lipgloss.AdaptiveColor {
	return lipgloss.AdaptiveColor{Light: light, Dark: dark}
}

// autoPalette adapts to the terminal background automatically via lipgloss.AdaptiveColor.
// This is the default and handles both light and dark terminals correctly.
func autoPalette() palette {
	return palette{
		header:       lipgloss.NewStyle().Background(lipgloss.Color("0")).Foreground(lipgloss.Color("15")).Bold(true).Padding(0, 1),
		rule:         lipgloss.NewStyle().Foreground(adaptive("245", "238")),
		you:          lipgloss.NewStyle().Bold(true).Foreground(adaptive("127", "212")),
		nine:         lipgloss.NewStyle().Bold(true).Foreground(adaptive("23", "86")),
		system:       lipgloss.NewStyle().Foreground(adaptive("25", "75")),
		spinner:      lipgloss.NewStyle().Foreground(adaptive("127", "205")),
		prompt:       lipgloss.NewStyle().Foreground(adaptive("238", "241")),
		ts:           lipgloss.NewStyle().Foreground(adaptive("244", "240")),
		tool:         lipgloss.NewStyle().Foreground(adaptive("130", "214")),
		arrow:        lipgloss.NewStyle().Foreground(adaptive("130", "214")),
		toolInput:    lipgloss.NewStyle().Foreground(adaptive("241", "244")).Italic(true),
		output:       lipgloss.NewStyle().Foreground(adaptive("236", "252")),
		continuation: lipgloss.NewStyle().Foreground(adaptive("244", "240")),
	}
}

func lightPalette() palette {
	return palette{
		header:       lipgloss.NewStyle().Background(lipgloss.Color("0")).Foreground(lipgloss.Color("15")).Bold(true).Padding(0, 1),
		rule:         lipgloss.NewStyle().Foreground(lipgloss.Color("245")),
		you:          lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("127")),
		nine:         lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("23")),
		system:       lipgloss.NewStyle().Foreground(lipgloss.Color("25")),
		spinner:      lipgloss.NewStyle().Foreground(lipgloss.Color("127")),
		prompt:       lipgloss.NewStyle().Foreground(lipgloss.Color("238")),
		ts:           lipgloss.NewStyle().Foreground(lipgloss.Color("244")),
		tool:         lipgloss.NewStyle().Foreground(lipgloss.Color("130")),
		arrow:        lipgloss.NewStyle().Foreground(lipgloss.Color("130")),
		toolInput:    lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Italic(true),
		output:       lipgloss.NewStyle().Foreground(lipgloss.Color("236")),
		continuation: lipgloss.NewStyle().Foreground(lipgloss.Color("244")),
	}
}

func darkPalette() palette {
	return palette{
		header:       lipgloss.NewStyle().Background(lipgloss.Color("0")).Foreground(lipgloss.Color("15")).Bold(true).Padding(0, 1),
		rule:         lipgloss.NewStyle().Foreground(lipgloss.Color("238")),
		you:          lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212")),
		nine:         lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("86")),
		system:       lipgloss.NewStyle().Foreground(lipgloss.Color("75")),
		spinner:      lipgloss.NewStyle().Foreground(lipgloss.Color("205")),
		prompt:       lipgloss.NewStyle().Foreground(lipgloss.Color("241")),
		ts:           lipgloss.NewStyle().Foreground(lipgloss.Color("240")),
		tool:         lipgloss.NewStyle().Foreground(lipgloss.Color("214")),
		arrow:        lipgloss.NewStyle().Foreground(lipgloss.Color("214")),
		toolInput:    lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Italic(true),
		output:       lipgloss.NewStyle().Foreground(lipgloss.Color("252")),
		continuation: lipgloss.NewStyle().Foreground(lipgloss.Color("240")),
	}
}

type toolEvent struct {
	name        string
	displayName string // human-friendly label; falls back to name when empty
	inputStr    string
	outputStr   string // empty while still running
	thought     string // ReAct reasoning text the model emitted before this call
	trace       string

	at time.Time

	subAgent     bool   // true if this represents a sub-agent lifecycle, not a tool call
	subAgentID   string // set when subAgent is true
	subAgentRole string // the sub-agent's resolved leaf role, when subAgent is true
}

type chatMsg struct {
	role       string // "user" or "nine"
	text       string
	at         time.Time
	toolEvents []toolEvent // non-empty only for "nine" role
	trace      string
}

// Internal tea.Msg types.
type connectedMsg struct {
	client          *protocol.Client
	agentID         string
	name            string
	instanceName    string
	role            string
	daemonProc      *os.Process // non-nil if we started the daemon; nil if it was already running
	replayEvents    []toolEvent // tool events that happened while no client was connected
	pendingResponse string      // last completed turn response, if available
	history         []chatMsg   // full prior transcript to render on reattach (supersedes replay)
}
type responseMsg struct{ text string }
type progressMsg struct{ evt protocol.ProgressEvent }
type errMsg struct{ err error }
type chunkMsg struct{ text string }

// timeTickMsg updates the current time display.
type timeTickMsg time.Time

// answerResultMsg reports the outcome of delivering a human answer on a
// separate connection. A non-nil err is shown inline, not fatally.
type answerResultMsg struct{ err error }

// streamConn drives TurnWithProgress in a goroutine and feeds tea.Msg values
// into a channel. Call next() to get a tea.Cmd that returns the next message.
type streamConn struct{ ch chan tea.Msg }

func startStream(c *protocol.Client, agentID, text string, forceThink bool) *streamConn {
	s := &streamConn{ch: make(chan tea.Msg, 64)}
	go func() {
		onProgress := func(evt protocol.ProgressEvent) {
			if evt.Type == protocol.TypeResponseChunk {
				s.ch <- chunkMsg{text: evt.Text}
			} else {
				s.ch <- progressMsg{evt: evt}
			}
		}

		var resp string
		var err error
		if forceThink {
			resp, err = c.TurnForced(agentID, text, onProgress)
		} else {
			resp, err = c.TurnWithProgress(agentID, text, onProgress)
		}
		if err != nil {
			s.ch <- errMsg{err: err}
		} else {
			s.ch <- responseMsg{text: resp}
		}
	}()
	return s
}

func (s *streamConn) next() tea.Cmd {
	return func() tea.Msg { return <-s.ch }
}

// connState holds the daemon connection, session identity, and in-flight turn stream.
type connState struct {
	client       *protocol.Client
	agentID      string
	sessionName  string
	instanceName string      // this Nine instance's display name (shown in the header)
	role         string      // resolved role the session runs (shown in the header)
	daemonProc   *os.Process // non-nil if we started the daemon
	sockPath     string
	binary       string
	attachID     string // non-empty when attaching to an existing agent
	stream       *streamConn
	err          error

	// reconnecting is set while the TUI is transparently re-attaching to its
	// session after the daemon connection dropped (e.g. a hot-reload restart).
	reconnecting bool
}

// chatState holds the chat transcript and the input/viewport widgets.
type chatState struct {
	messages        []chatMsg
	viewport        viewport.Model
	input           textinput.Model
	spinner         spinner.Model
	thinking        bool
	planning        bool // the no-tool analysis (planning) pass is in flight this turn
	thinkingAt      time.Time
	thinkingStep    int // LLM call count within the current turn (1-based)
	pendingToolEvts []toolEvent
	streamingText   string                   // accumulated text chunks from the current LLM turn
	thinkingTrace   string                   // accumulated reasoning tokens for the current step (ephemeral)
	thinkingThink   bool                     // the in-flight step streams reasoning; false = execute-only call
	stage           string                   // current waiting phase label; empty when none
	humanQueue      []*protocol.HumanRequest // unanswered questions, oldest first; [0] is the one on screen
	ready           bool

	// Slash-command picker, shown above the input while the user is typing a
	// command name. suggestFilter is the input value its current items were
	// built from, so the selection only resets when the filter actually changed.
	suggest       list.Model
	suggestOpen   bool
	suggestFilter string
}

// pendingHuman is the question currently on screen, or nil when none is
// waiting. Parallel sub-agents can each raise an approval gate at the same
// time (R-HITL.5), so questions queue and are answered oldest-first rather
// than the newest silently replacing the one the user is reading.
func (c *chatState) pendingHuman() *protocol.HumanRequest {
	if len(c.humanQueue) == 0 {
		return nil
	}
	return c.humanQueue[0]
}

// popHuman removes the on-screen question and returns the next one, if any.
func (c *chatState) popHuman() *protocol.HumanRequest {
	if len(c.humanQueue) > 0 {
		c.humanQueue = c.humanQueue[1:]
	}
	return c.pendingHuman()
}

// displayState holds layout, theming, and rendering configuration.
type displayState struct {
	width         int
	height        int
	showDetail    bool // toggled with Ctrl+T; shows tool inputs/outputs
	showContext   bool // from config; shows ctx used/budget in header
	contextUsed   int
	contextBudget int
	pal           palette
	glamourStyle  string
	renderer      *glamour.TermRenderer
	version       string // Nine's build version, shown under the logo
	currentTime   time.Time // current time for display in prompt bar
}

type model struct {
	conn    connState
	chat    chatState
	display displayState
	cfg     *config.Config
}

func initialModel(sockPath, binary, attachID string, pal palette, glamourStyle string, showContext bool, cfg *config.Config, version string) model {
	ti := textinput.New()
	ti.Placeholder = "Type a message..."
	ti.Prompt = "> "
	ti.PromptStyle = pal.prompt
	ti.CharLimit = 0
	ti.Focus() //nolint:errcheck

	sp := spinner.New()
	sp.Spinner = spinner.Pulse
	sp.Style = pal.spinner

	return model{
		conn: connState{
			sockPath: sockPath,
			binary:   binary,
			attachID: attachID,
		},
		chat: chatState{
			input:   ti,
			spinner: sp,
			suggest: newSuggestList(pal),
		},
		display: displayState{
			showDetail:   true,
			showContext:  showContext,
			pal:          pal,
			glamourStyle: glamourStyle,
			version:      version,
			currentTime:  time.Now(),
		},
		cfg: cfg,
	}
}

// newRenderer builds a glamour renderer for the current viewport width.
// Called once on first WindowSizeMsg and again whenever the width changes.
func newRenderer(glamourStyle string, width int) *glamour.TermRenderer {
	var styleOpt glamour.TermRendererOption
	if glamourStyle == "auto" || glamourStyle == "" {
		styleOpt = glamour.WithAutoStyle()
	} else {
		styleOpt = glamour.WithStandardStyle(glamourStyle)
	}
	r, _ := glamour.NewTermRenderer(styleOpt, glamour.WithWordWrap(width))
	return r
}

// clockTick returns a command that sends time updates every second.
func clockTick() tea.Cmd {
	return tea.Every(time.Second, func(t time.Time) tea.Msg {
		return timeTickMsg(t)
	})
}

func (m model) Init() tea.Cmd {
	var cmd tea.Cmd
	if m.conn.attachID != "" {
		cmd = attachCmd(m.conn.sockPath, m.conn.binary, m.conn.attachID)
	} else {
		cmd = connectCmd(m.conn.sockPath, m.conn.binary)
	}
	return tea.Batch(cmd, m.chat.spinner.Tick, clockTick())
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		// While the slash-command picker is open it owns navigation, completion
		// and dismissal, and these keys must not reach the text input or the
		// viewport (which would otherwise scroll on up/down). Enter is handled
		// further down instead, so a no-argument command can be completed and
		// submitted in the same keystroke.
		if m.chat.suggestOpen {
			switch msg.Type {
			case tea.KeyUp, tea.KeyCtrlP:
				m.chat.suggest.CursorUp()
				return m, nil
			case tea.KeyDown, tea.KeyCtrlN:
				m.chat.suggest.CursorDown()
				return m, nil
			case tea.KeyTab:
				if c, ok := m.selectedSuggestion(); ok {
					m.acceptSuggestion(c)
				}
				return m, nil
			case tea.KeyEsc:
				// Esc quits the TUI everywhere else; with the picker open it
				// dismisses the picker only.
				m.closeSuggestions()
				return m, nil
			}
		}

		switch msg.Type {
		case tea.KeyCtrlC, tea.KeyEsc:
			if m.conn.client != nil {
				m.conn.client.Close() //nolint:errcheck
			}
			return m, tea.Quit

		case tea.KeyCtrlT:
			m.display.showDetail = !m.display.showDetail
			m.rebuildContent()

		case tea.KeyPgUp, tea.KeyPgDown, tea.KeyHome, tea.KeyEnd:
			if m.chat.ready {
				var vpCmd tea.Cmd
				m.chat.viewport, vpCmd = m.chat.viewport.Update(msg)
				cmds = append(cmds, vpCmd)
			}
			return m, tea.Batch(cmds...)

		case tea.KeyEnter:
			// Accept the highlighted command rather than submitting whatever
			// partial text is in the box — otherwise "/hel" + Enter would send
			// "/hel" while "/help" sits visibly selected. A command that takes
			// an argument stops here so the user can type it; one that does not
			// falls through and runs immediately.
			if c, ok := m.selectedSuggestion(); ok {
				m.acceptSuggestion(c)
				if c.args != "" {
					return m, tea.Batch(cmds...)
				}
			}
			// Answering a pending ask_human takes priority — it is allowed even
			// while the turn is still in flight (thinking == true).
			if req := m.chat.pendingHuman(); req != nil && m.conn.client != nil {
				text := strings.TrimSpace(m.chat.input.Value())
				if text == "" {
					break
				}
				m.chat.input.Reset()
				m.chat.messages = append(m.chat.messages, chatMsg{
					role: "user",
					text: text,
					at:   time.Now(),
				})
				// Answering uncovers the next queued question, if any, rather than
				// returning the input box to normal.
				if next := m.chat.popHuman(); next != nil {
					m.chat.messages = append(m.chat.messages, chatMsg{
						role: "ask",
						text: formatQuestion(next),
						at:   time.Now(),
					})
				} else {
					m.resetInputPrompt()
				}
				m.rebuildContent()
				m.chat.viewport.GotoBottom()
				cmds = append(cmds, answerCmd(m.conn.sockPath, m.conn.agentID, req.RequestID, text))
				return m, tea.Batch(cmds...)
			}
			if !m.chat.thinking && m.conn.client != nil {
				text := strings.TrimSpace(m.chat.input.Value())
				if text == "" {
					break
				}
				m.chat.input.Reset()
				forceThink := false
				if strings.HasPrefix(text, "/") {
					cmd, arg, _ := strings.Cut(strings.TrimPrefix(text, "/"), " ")
					if cmd == "think" {
						text = strings.TrimSpace(arg)
						if text == "" {
							m.appendSystem("usage: /think <message>")
							m.rebuildContent()
							m.chat.viewport.GotoBottom()
							return m, tea.Batch(cmds...)
						}
						forceThink = true
					} else {
						switch cmd {
						case "clear":
							m.chat.messages = m.chat.messages[:0]
						case "new":
							id, role, instanceName, err := m.conn.client.NewConversationInteractive(true)
							if err != nil {
								m.appendSystem("error: " + err.Error())
							} else {
								m.conn.agentID = id
								m.conn.role = role
								if instanceName != "" {
									m.conn.instanceName = instanceName
								}
								m.chat.messages = m.chat.messages[:0]
								m.appendSystem("new conversation: " + shortID(id, 8))
							}
						default:
							result, err := runCmd(cmd, arg, m.conn.client, m.cfg, m.conn.agentID)
							if err != nil {
								m.appendSystem("error: " + err.Error())
							} else {
								m.appendSystem(result)
							}
						}
						m.rebuildContent()
						m.chat.viewport.GotoBottom()
						return m, tea.Batch(cmds...)
					}
				}
				m.chat.messages = append(m.chat.messages, chatMsg{
					role: "user",
					text: text,
					at:   time.Now(),
				})
				m.chat.thinking = true
				m.chat.planning = false
				m.chat.thinkingAt = time.Now()
				m.chat.thinkingStep = 0
				m.chat.thinkingTrace = ""
				m.chat.thinkingThink = false
				m.chat.stage = ""
				m.chat.streamingText = ""
				m.chat.pendingToolEvts = nil
				m.rebuildContent()
				m.chat.viewport.GotoBottom()
				m.conn.stream = startStream(m.conn.client, m.conn.agentID, text, forceThink)
				cmds = append(cmds, m.conn.stream.next())
			}
		}

	case tea.WindowSizeMsg:
		m.display.width = msg.Width
		m.display.height = msg.Height
		m.chat.input.Width = msg.Width - len(m.chat.input.Prompt) - 1
		vph := m.viewportHeight()
		if !m.chat.ready {
			m.chat.viewport = viewport.New(msg.Width, vph)
			m.chat.ready = true
		} else {
			m.chat.viewport.Width = msg.Width
			m.chat.viewport.Height = vph
		}
		m.chat.suggest.SetSize(msg.Width, m.suggestHeight())
		m.display.renderer = newRenderer(m.display.glamourStyle, msg.Width-4)
		m.rebuildContent()

	case connectedMsg:
		wasReconnecting := m.conn.reconnecting
		m.conn.reconnecting = false
		m.conn.err = nil
		m.conn.client = msg.client
		m.conn.agentID = msg.agentID
		m.conn.sessionName = msg.name
		if msg.instanceName != "" {
			m.conn.instanceName = msg.instanceName
		}
		m.conn.role = msg.role
		m.conn.daemonProc = msg.daemonProc
		if wasReconnecting {
			// The transcript is already on screen; the daemon's replay would
			// duplicate it. Just confirm the reconnect (any turn interrupted by
			// the restart was not completed — resend it if needed).
			m.appendSystem("reconnected to the session")
			m.rebuildContent()
			m.chat.viewport.GotoBottom()
			break
		}
		if len(msg.history) > 0 {
			// Full transcript available: render the whole conversation so the
			// reattached session looks exactly as it did before detaching.
			m.chat.messages = append(m.chat.messages, msg.history...)
			m.rebuildContent()
			m.chat.viewport.GotoBottom()
		} else if len(msg.replayEvents) > 0 || msg.pendingResponse != "" {
			at := time.Now()
			if len(msg.replayEvents) > 0 {
				at = msg.replayEvents[0].at
			}
			m.chat.messages = append(m.chat.messages, chatMsg{
				role:       "nine",
				text:       msg.pendingResponse,
				at:         at,
				toolEvents: msg.replayEvents,
			})
		}

	case chunkMsg:
		m.chat.streamingText += msg.text
		// The model is producing: whatever we were waiting on is over.
		m.chat.stage = ""
		m.rebuildContent()
		m.chat.viewport.GotoBottom()
		if m.conn.stream != nil {
			cmds = append(cmds, m.conn.stream.next())
		}

	case progressMsg:
		evt := msg.evt
		switch evt.Type {
		case protocol.TypeToolStart:
			// The intermediate text the model emitted before this call is its ReAct
			// "thought". Retain it on the tool event so it persists in the transcript
			// instead of being discarded, then clear the live streaming buffer.
			m.chat.pendingToolEvts = append(m.chat.pendingToolEvts, toolEvent{
				name:        evt.ToolName,
				displayName: evt.ToolDisplayName,
				inputStr:    formatInput(evt.ToolInput),
				thought:     strings.TrimSpace(m.chat.streamingText),
				at:          evt.At,
				trace:       m.chat.thinkingTrace,
			})
			m.chat.streamingText = ""
			m.chat.thinkingTrace = ""
		case protocol.TypeToolEnd:
			for i := len(m.chat.pendingToolEvts) - 1; i >= 0; i-- {
				if m.chat.pendingToolEvts[i].name == evt.ToolName && m.chat.pendingToolEvts[i].outputStr == "" {
					m.chat.pendingToolEvts[i].outputStr = truncateOutput(evt.ToolOutput)
					break
				}
			}
		case protocol.TypeThinking:
			m.chat.thinkingStep = evt.LLMCallN
			m.chat.thinkingThink = evt.Think
			// The analysis pass (if any) is done once the exec loop's first call starts.
			m.chat.planning = false
			// Each inner LLM call starts a fresh reasoning trace.
			m.chat.thinkingTrace = ""
			m.chat.stage = ""
		case protocol.TypeStage:
			m.chat.stage = evt.Text
		case protocol.TypePlanStart:
			m.chat.planning = true
		case protocol.TypePlanEnd:
			m.chat.planning = false
		case protocol.TypeNotice:
			m.appendSystem(evt.Text)
		case protocol.TypeThinkingChunk:
			m.chat.thinkingTrace += evt.Text
			m.chat.stage = ""
		case protocol.TypeContextUpdate:
			m.display.contextUsed = evt.ContextUsed
			m.display.contextBudget = evt.ContextBudget
		case protocol.TypeSetName:
			m.conn.sessionName = evt.Text
		case protocol.TypeSetInstanceName:
			if evt.Text != "" {
				m.conn.instanceName = evt.Text
			}
		case protocol.TypeSubAgentStart:
			m.chat.pendingToolEvts = append(m.chat.pendingToolEvts, toolEvent{
				inputStr:     truncateOutput(evt.Text),
				at:           evt.At,
				subAgent:     true,
				subAgentID:   evt.SubAgentID,
				subAgentRole: evt.Role,
			})
		case protocol.TypeSubAgentEnd:
			for i := len(m.chat.pendingToolEvts) - 1; i >= 0; i-- {
				te := &m.chat.pendingToolEvts[i]
				if te.subAgent && te.subAgentID == evt.SubAgentID && te.outputStr == "" {
					te.outputStr = evt.Status
					break
				}
			}
		case protocol.TypeHumanInputRequired:
			if evt.HumanRequest != nil {
				// Only the head of the queue is rendered; a question arriving while
				// another is unanswered (parallel sub-agents each hitting a gate)
				// waits its turn instead of overwriting the one on screen.
				first := m.chat.pendingHuman() == nil
				m.chat.humanQueue = append(m.chat.humanQueue, evt.HumanRequest)
				if first {
					m.chat.messages = append(m.chat.messages, chatMsg{
						role: "ask",
						text: formatQuestion(evt.HumanRequest),
						at:   time.Now(),
					})
					m.chat.input.Prompt = "Answer: "
					m.chat.input.PromptStyle = m.display.pal.you
				}
			}
		}
		m.rebuildContent()
		m.chat.viewport.GotoBottom()
		if m.conn.stream != nil {
			cmds = append(cmds, m.conn.stream.next())
		}

	case responseMsg:
		m.chat.thinking = false
		m.chat.planning = false
		m.conn.stream = nil
		m.chat.streamingText = ""
		trace := m.chat.thinkingTrace
		m.chat.thinkingTrace = ""
		m.chat.stage = ""

		if m.chat.pendingHuman() != nil {
			// Turn ended with questions still unanswered (e.g. they timed out).
			// The whole queue goes with it — every asker's wait ended too.
			m.chat.humanQueue = nil
			m.resetInputPrompt()
		}
		m.chat.messages = append(m.chat.messages, chatMsg{
			role:       "nine",
			text:       msg.text,
			at:         m.chat.thinkingAt,
			toolEvents: m.chat.pendingToolEvts,
			trace:      trace,
		})
		m.chat.pendingToolEvts = nil
		m.rebuildContent()
		m.chat.viewport.GotoBottom()

	case answerResultMsg:
		if msg.err != nil {
			m.appendSystem("answer failed: " + msg.err.Error())
			m.rebuildContent()
			m.chat.viewport.GotoBottom()
		}

	case errMsg:
		m.chat.thinking = false
		m.chat.planning = false
		m.conn.stream = nil
		m.chat.stage = ""

		// A dropped connection mid-session (e.g. the daemon restarting under
		// hot-reload) is recoverable: the session's state is checkpointed in
		// Postgres, and re-attaching revives it. Reconnect transparently instead
		// of dying. Real daemon/agent errors (prefixed "daemon:") are shown as
		// before.
		if m.conn.agentID != "" && !m.conn.reconnecting && isConnDropped(msg.err) {
			if m.conn.client != nil {
				m.conn.client.Close() //nolint:errcheck
				m.conn.client = nil
			}
			m.conn.reconnecting = true
			m.chat.streamingText = ""
			m.chat.thinkingTrace = ""
			m.chat.pendingToolEvts = nil
			m.appendSystem("lost the daemon connection — reconnecting…")
			m.rebuildContent()
			m.chat.viewport.GotoBottom()
			cmds = append(cmds, reattachCmd(m.conn.sockPath, m.conn.agentID))
		} else {
			m.conn.reconnecting = false
			m.conn.err = msg.err
		}

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.chat.spinner, cmd = m.chat.spinner.Update(msg)
		cmds = append(cmds, cmd)
		if m.chat.thinking {
			m.rebuildContent()
		}

	case timeTickMsg:
		m.display.currentTime = time.Time(msg)
		cmds = append(cmds, clockTick())
	}

	if m.chat.ready {
		var vpCmd tea.Cmd
		m.chat.viewport, vpCmd = m.chat.viewport.Update(msg)
		cmds = append(cmds, vpCmd)
	}

	var tiCmd tea.Cmd
	m.chat.input, tiCmd = m.chat.input.Update(msg)
	cmds = append(cmds, tiCmd)

	// The input has the keystroke by now, so the picker tracks typing, deletion
	// and paste alike — including closing itself the moment the leading "/" is
	// gone or nothing matches any more.
	m.refreshSuggestions()

	return m, tea.Batch(cmds...)
}

func (m model) View() string {
	if !m.chat.ready {
		return "\n  Connecting to nine daemon...\n"
	}
	if m.conn.err != nil {
		return fmt.Sprintf("\n  error: %v\n\n  Press ctrl+c to exit.\n", m.conn.err)
	}

	label := "connecting..."
	if m.conn.reconnecting {
		label = "reconnecting…"
	} else if m.conn.agentID != "" {
		short := m.conn.agentID
		if len(short) > 8 {
			short = shortID(short, 8)
		}
		if m.conn.sessionName != "" {
			label = m.conn.sessionName + "  (" + short + ")"
		} else {
			label = short
		}
	}
	ctxHint := contextHint(m.display.contextUsed, m.display.contextBudget, m.display.showContext)
	askHint := ""
	if m.chat.pendingHuman() != nil {
		askHint = "  ·  ? awaiting answer"
		if waiting := len(m.chat.humanQueue) - 1; waiting > 0 {
			askHint += fmt.Sprintf(" (%d more waiting)", waiting)
		}
	}

	instanceName := m.conn.instanceName
	if instanceName == "" {
		instanceName = "nine"
	}
	header := m.display.pal.header.Width(m.display.width).Render(
		fmt.Sprintf("%s  ·  %s%s%s", instanceName, label, askHint, ctxHint),
	)
	rule := m.display.pal.rule.Render(strings.Repeat("─", m.display.width))

	parts := []string{header, rule, m.chat.viewport.View(), rule}
	// The picker sits directly on top of the input box, so it reads as attached
	// to what is being typed.
	if m.chat.suggestOpen {
		parts = append(parts, m.chat.suggest.View())
	}
	// Render status line with role, token counter and time (top line of bottom bar)
	statusLine := renderStatusLine(m.display, m.conn.role)
	parts = append(parts, statusLine)
	
	// Render input line (bottom line of bottom bar)
	inputLine := renderInputWithTime(m.chat.input, m.display)
	parts = append(parts, inputLine)

	return strings.Join(parts, "\n")
}

// contextHint renders the header's context-usage segment. When usage crosses
// contextWarnPercent of the budget it flags the pressure with a ⚠ marker and a
// percentage, and does so even when showContext is off — a warning outranks the
// user's opt-out of the routine ctx readout. Below the threshold it shows the
// formatted used/budget readout only when showContext is on, and nothing when the
// budget is unknown.
func contextHint(used, budget int, showContext bool) string {
	if budget <= 0 {
		return ""
	}
	pct := used * 100 / budget
	formattedUsed := formatTokens(used)
	formattedBudget := formatTokens(budget)
	if used*100 >= contextWarnPercent*budget {
		return fmt.Sprintf("  ·  ⚠ %d%% (%s/%s)", pct, formattedUsed, formattedBudget)
	}
	if showContext {
		return fmt.Sprintf("  ·  %d%% (%s/%s)", pct, formattedUsed, formattedBudget)
	}
	return ""
}

// formatTokens formats token counts with 'k' suffix for thousands.
// e.g., 6715 -> "6k", 32768 -> "32k", 500 -> "500"
func formatTokens(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%dk", n/1000)
	}
	return fmt.Sprintf("%d", n)
}

// renderInputWithTime renders the text input on its own line.
// Token counter, time, and role are rendered on a separate status line above.
func renderInputWithTime(input textinput.Model, d displayState) string {
	// Get the current input line
	inputView := input.View()
	
	// Input takes the full width of its line
	return lipgloss.NewStyle().Width(d.width).Render(inputView)
}

// renderStatusLine renders role, token counter, and time on the top line
// of the bottom bar. Format: [role] [tokenCounter | time] (right-aligned)
func renderStatusLine(d displayState, role string) string {
	// Format time as HH:MM:SS (always shown at far right)
	timeStr := d.pal.ts.Render(d.currentTime.Format("15:04:05"))
	
	// Build token counter string
	tokenStr := ""
	if d.contextBudget > 0 {
		pct := d.contextUsed * 100 / d.contextBudget
		tokenStr = d.pal.ts.Render(fmt.Sprintf("%d%% (%s/%s)", pct, formatTokens(d.contextUsed), formatTokens(d.contextBudget)))
	}
	
	// Build the status line: [role] [tokenCounter | time]
	var parts []string
	
	// Add role on the left if present
	if role != "" {
		parts = append(parts, d.pal.you.Render(role))
	}
	
	if tokenStr != "" {
		if role != "" {
			parts = append(parts, d.pal.continuation.Render(" "))
		}
		parts = append(parts, tokenStr)
		parts = append(parts, d.pal.continuation.Render(" | "))
	} else if role != "" {
		// No token counter but have role, add pipe directly
		parts = append(parts, d.pal.continuation.Render(" | "))
	}
	
	parts = append(parts, timeStr)
	
	// Right-align the entire status line
	statusContent := lipgloss.JoinHorizontal(lipgloss.Top, parts...)
	// Use lipgloss.Right to right-align within the width
	return lipgloss.NewStyle().Width(d.width).Align(lipgloss.Right).Render(statusContent)
}

func (m *model) viewportHeight() int {
	// 1 for the rule above the input + 1 for the input line itself, plus
	// whatever rows the slash-command picker is borrowing.
	h := m.display.height - headerHeight - inputAreaHeight - m.suggestHeight()
	if h < 1 {
		return 1
	}
	return h
}

func (m *model) appendSystem(text string) {
	m.chat.messages = append(m.chat.messages, chatMsg{role: "system", text: text, at: time.Now()})
}

// resetInputPrompt restores the default "> " input prompt after a HITL answer.
func (m *model) resetInputPrompt() {
	m.chat.input.Prompt = "> "
	m.chat.input.PromptStyle = m.display.pal.prompt
}

// formatQuestion renders an ask_human question and any options for display.
// formatQuestion renders a pending question for the transcript. A question
// raised by a sub-agent is prefixed with who is asking — without that, an
// approval prompt for a tool the user never saw requested is unactionable
// (R-HITL.8).
func formatQuestion(hr *protocol.HumanRequest) string {
	var b strings.Builder
	if hr.Origin != "" {
		b.WriteString("[" + hr.Origin + "]\n")
	}
	b.WriteString(hr.Question)
	for i, opt := range hr.Options {
		fmt.Fprintf(&b, "\n  %d) %s", i+1, opt)
	}
	return b.String()
}

// nineLogo is the ASCII banner shown at the top of a fresh conversation.
const nineLogo = ` ███╗   ██╗██╗███╗   ██╗███████╗
 ████╗  ██║██║████╗  ██║██╔════╝
 ██╔██╗ ██║██║██╔██╗ ██║█████╗
 ██║╚██╗██║██║██║╚██╗██║██╔══╝
 ██║ ╚████║██║██║ ╚████║███████╗
 ╚═╝  ╚═══╝╚═╝╚═╝  ╚═══╝╚══════╝`

// renderLogo writes the centered, styled nine banner.
func renderLogo(sb *strings.Builder, pal palette, width int, version string) {
	logoWidth := lipgloss.Width(nineLogo)
	pad := (width - logoWidth) / 2
	if pad < 0 {
		pad = 0
	}
	indent := strings.Repeat(" ", pad)
	sb.WriteByte('\n')
	for _, line := range strings.Split(nineLogo, "\n") {
		sb.WriteString(indent + pal.nine.Render(line) + "\n")
	}
}

func (m *model) rebuildContent() {
	if !m.chat.ready {
		return
	}
	var sb strings.Builder
	if len(m.chat.messages) == 0 && !m.chat.thinking {
		renderLogo(&sb, m.display.pal, m.chat.viewport.Width, m.display.version)
		m.chat.viewport.SetContent(sb.String())
		return
	}
	for _, msg := range m.chat.messages {
		renderChatMsg(&sb, msg, m.chat.viewport.Width, m.display.showDetail, m.display.pal, m.display.renderer)
		sb.WriteByte('\n')
	}
	if m.chat.thinking {
		renderThinking(&sb, thinkingView{
			sp:         m.chat.spinner,
			at:         m.chat.thinkingAt,
			evts:       m.chat.pendingToolEvts,
			showDetail: m.display.showDetail,
			pal:        m.display.pal,
			streamText: m.chat.streamingText,
			trace:      m.chat.thinkingTrace,
			width:      m.chat.viewport.Width,
			step:       m.chat.thinkingStep,
			think:      m.chat.thinkingThink,
			planning:   m.chat.planning,
			stage:      m.chat.stage,
		})
	}
	m.chat.viewport.SetContent(sb.String())
}

func headerLine(label, styledLabel, tsText, styledTs string, width int) string {
	fill := width - len(label) - len(tsText)
	if fill < 1 {
		fill = 1
	}
	return styledLabel + strings.Repeat(" ", fill) + styledTs + "\n"
}

func renderChatMsg(sb *strings.Builder, msg chatMsg, width int, showDetail bool, pal palette, r *glamour.TermRenderer) {
	tsText := "[" + msg.at.Format("15:04:05") + "]"
	styledTs := pal.ts.Render(tsText)
	const indent = "  "
	switch msg.role {
	case "user":
		sb.WriteString(headerLine("You:", pal.you.Render("You:"), tsText, styledTs, width))
		if msg.text != "" {
			wrapped := wordWrap(msg.text, width-len(indent))
			for _, line := range strings.Split(wrapped, "\n") {
				sb.WriteString(indent + line + "\n")
			}
		}
	case "system":
		sb.WriteString(headerLine("nine:", pal.system.Render("nine:"), tsText, styledTs, width))
		for _, line := range strings.Split(msg.text, "\n") {
			sb.WriteString(indent + pal.system.Render(line) + "\n")
		}
	case "ask":
		sb.WriteString(headerLine("Nine asks:", pal.you.Render("Nine asks:"), tsText, styledTs, width))
		wrapped := wordWrap(msg.text, width-len(indent))
		for _, line := range strings.Split(wrapped, "\n") {
			sb.WriteString(indent + pal.tool.Render(line) + "\n")
		}
	default: // "nine"
		sb.WriteString(headerLine("Nine:", pal.nine.Render("Nine:"), tsText, styledTs, width))
		for _, te := range msg.toolEvents {
			renderToolEvent(sb, te, showDetail, pal, width)
		}
		renderTrace(sb, msg.trace, showDetail, pal, width)
		if msg.text != "" {
			rendered := renderMarkdown(r, msg.text)
			sb.WriteString(rendered)
		}
	}
}

func renderMarkdown(r *glamour.TermRenderer, text string) string {
	if r == nil {
		return "  " + text + "\n"
	}
	out, err := r.Render(text)
	if err != nil {
		return "  " + text + "\n"
	}
	return out
}

// thinkingView is the live state renderThinking draws: the in-flight step, the
// phase it's waiting on, and everything accumulated so far this turn.
type thinkingView struct {
	sp         spinner.Model
	at         time.Time
	evts       []toolEvent
	showDetail bool
	pal        palette
	streamText string
	trace      string
	width      int
	step       int
	think      bool
	planning   bool
	stage      string
}

// statusLabel names what the turn is doing right now: a named waiting phase if
// one is active, else the kind of call in flight. "working" rather than
// "thinking" when the call won't stream reasoning — the execute phase of
// plan-then-execute, or a model that can't think — so the user isn't waiting on
// traces that are never coming.
func (v thinkingView) statusLabel() string {
	switch {
	case v.stage != "":
		return v.stage + "..."
	case v.planning:
		return "planning..."
	case v.think:
		return "thinking..."
	default:
		return "working..."
	}
}

func (v thinkingView) stepHint() string {
	if v.step > 1 {
		return fmt.Sprintf(" (step %d)", v.step)
	}
	return ""
}

func renderThinking(sb *strings.Builder, v thinkingView) {
	tsText := "[" + v.at.Format("15:04:05") + "]"
	styledTs := v.pal.ts.Render(tsText)
	const indent = "  "
	sb.WriteString(headerLine("Nine:", v.pal.nine.Render("Nine:"), tsText, styledTs, v.width))
	for _, te := range v.evts {
		renderToolEvent(sb, te, v.showDetail, v.pal, v.width)
	}

	renderTrace(sb, v.trace, v.showDetail, v.pal, v.width)

	if v.streamText != "" {
		wrapped := wordWrap(v.streamText, v.width-len(indent))
		for _, line := range strings.Split(wrapped, "\n") {
			sb.WriteString(indent + v.pal.output.Render(line) + "\n")
		}
	} else {
		sb.WriteString(indent + v.sp.View() + " " + v.statusLabel() + v.stepHint() +
			v.pal.ts.Render("  "+formatElapsed(time.Since(v.at))) + "\n")
	}
}

// maxThinkingTraceLines caps how many trailing lines of the live reasoning trace
// are shown while a step is in flight (detail view shows the full trace).
const maxThinkingTraceLines = 8

// renderThought renders retained ReAct reasoning text above a tool call, dimmed
// and in full (it is short and persists in the transcript).
func renderThought(sb *strings.Builder, thought string, pal palette, width int) {
	renderReasoning(sb, thought, pal, width, 0)
}

// renderTrace writes a reasoning trace, live or retained. Outside detail view it
// is capped to its last few lines so a long trace can't flood the viewport.
func renderTrace(sb *strings.Builder, trace string, showDetail bool, pal palette, width int) {
	if trace == "" {
		return
	}

	lineCap := maxThinkingTraceLines
	if showDetail {
		lineCap = 0
	}

	renderReasoning(sb, trace, pal, width, lineCap)
}

// renderReasoning writes dimmed, word-wrapped reasoning text indented under the
// current message. maxLines > 0 keeps only the last maxLines wrapped lines,
// prefixed with an ellipsis marker; maxLines <= 0 shows everything.
func renderReasoning(sb *strings.Builder, text string, pal palette, width int, maxLines int) {
	const indent = "  "
	wrapped := wordWrap(strings.TrimSpace(text), width-len(indent)-2)
	lines := strings.Split(wrapped, "\n")
	truncated := false
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
		truncated = true
	}
	if truncated {
		sb.WriteString(indent + pal.continuation.Render("· …") + "\n")
	}
	for _, line := range lines {
		sb.WriteString(indent + pal.continuation.Render("· ") + pal.toolInput.Render(line) + "\n")
	}
}

func renderToolEvent(sb *strings.Builder, te toolEvent, showDetail bool, pal palette, width int) {
	tsText := "[" + te.at.Format("15:04:05") + "]"
	styledTs := pal.ts.Render(tsText)
	const indent = "  "
	if te.subAgent {
		label := "sub-agent"
		if te.subAgentRole != "" {
			label += " · " + te.subAgentRole
		}
		rawLeft := "↳ " + label + "  " + te.inputStr
		fill := width - lipgloss.Width(rawLeft) - len(tsText)
		if fill < 1 {
			fill = 1
		}
		styledLine := pal.arrow.Render("↳") + " " + pal.tool.Render(label) + "  " + pal.toolInput.Render(te.inputStr)
		sb.WriteString(styledLine + strings.Repeat(" ", fill) + styledTs + "\n")
		status := te.outputStr
		if status == "" {
			status = "running"
		}
		sb.WriteString(indent + pal.continuation.Render("└") + " " + pal.output.Render(status) + "\n")
		return
	}
	label := te.name
	if te.displayName != "" {
		label = te.displayName
	}

	renderTrace(sb, te.trace, showDetail, pal, width)
	if te.thought != "" {
		renderThought(sb, te.thought, pal, width)
	}
	elapsedStr := ""
	if te.outputStr == "" {
		elapsedStr = "  " + formatElapsed(time.Since(te.at))
	}
	if showDetail {
		rawLeft := "→ " + label + elapsedStr
		fill := width - lipgloss.Width(rawLeft) - len(tsText)
		if fill < 1 {
			fill = 1
		}
		styledLine := pal.arrow.Render("→") + " " + pal.tool.Render(label)
		if elapsedStr != "" {
			styledLine += pal.ts.Render(elapsedStr)
		}
		sb.WriteString(styledLine + strings.Repeat(" ", fill) + styledTs + "\n")
		if te.inputStr != "" {
			sb.WriteString(indent + pal.toolInput.Render(te.inputStr) + "\n")
		}
		if te.outputStr != "" {
			sb.WriteString(indent + pal.continuation.Render("└") + " " + pal.output.Render(te.outputStr) + "\n")
		}
	} else {
		rawLeft := "→ " + label + elapsedStr
		fill := width - lipgloss.Width(rawLeft) - len(tsText)
		if fill < 1 {
			fill = 1
		}
		styledLine := pal.arrow.Render("→") + " " + pal.tool.Render(label)
		if elapsedStr != "" {
			styledLine += pal.ts.Render(elapsedStr)
		}
		sb.WriteString(styledLine + strings.Repeat(" ", fill) + styledTs + "\n")
	}
}

func formatElapsed(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%dm%ds", m, s)
}

// replayToToolEvents converts a slice of protocol.Msg (tool_start/tool_end/
// sub_agent_start/sub_agent_end) into paired toolEvent entries for display.
func replayToToolEvents(msgs []protocol.Msg) []toolEvent {
	var evts []toolEvent
	for _, msg := range msgs {
		at := time.Now()
		if msg.Timestamp > 0 {
			at = time.UnixMilli(msg.Timestamp)
		}
		switch msg.Type {
		case protocol.TypeToolStart:
			evts = append(evts, toolEvent{
				name:        msg.ToolName,
				displayName: msg.ToolDisplayName,
				inputStr:    formatInput(msg.ToolInput),
				at:          at,
			})
		case protocol.TypeToolEnd:
			for i := len(evts) - 1; i >= 0; i-- {
				if evts[i].name == msg.ToolName && evts[i].outputStr == "" {
					evts[i].outputStr = truncateOutput(msg.ToolOutput)
					break
				}
			}
		case protocol.TypeSubAgentStart:
			evts = append(evts, toolEvent{
				inputStr:     truncateOutput(msg.Text),
				at:           at,
				subAgent:     true,
				subAgentID:   msg.SubAgentID,
				subAgentRole: msg.Role,
			})
		case protocol.TypeSubAgentEnd:
			for i := len(evts) - 1; i >= 0; i-- {
				te := &evts[i]
				if te.subAgent && te.subAgentID == msg.SubAgentID && te.outputStr == "" {
					te.outputStr = msg.Status
					break
				}
			}
		}
	}
	return evts
}

// historyToChatMsgs reconstructs a chat transcript from the ordered protocol
// messages the daemon builds on reattach (see Daemon.journalHistory). It groups
// a user prompt, the tool/sub-agent activity that followed, and the assistant's
// response into the same alternating "user"/"nine" messages a live session
// produces, so the reattached view is indistinguishable from an uninterrupted one.
func historyToChatMsgs(msgs []protocol.Msg) []chatMsg {
	var (
		out     []chatMsg
		segment []protocol.Msg // tool/sub-agent msgs accumulating for the current nine turn
		segAt   time.Time
	)
	msgAt := func(m protocol.Msg) time.Time {
		if m.Timestamp > 0 {
			return time.UnixMilli(m.Timestamp)
		}
		return time.Now()
	}
	// flushNine emits the pending nine turn (its tool activity plus response
	// text), if any. A turn with activity but no text (e.g. one still in flight
	// when the client attached) is still shown so nothing is lost.
	flushNine := func(text string, at time.Time) {
		if len(segment) == 0 && text == "" {
			return
		}
		if at.IsZero() {
			at = segAt
		}
		out = append(out, chatMsg{
			role:       "nine",
			text:       text,
			at:         at,
			toolEvents: replayToToolEvents(segment),
		})
		segment = nil
		segAt = time.Time{}
	}
	for _, m := range msgs {
		switch m.Type {
		case protocol.TypeHistoryUser:
			flushNine("", time.Time{}) // close any open nine turn before the next prompt
			out = append(out, chatMsg{role: "user", text: m.Text, at: msgAt(m)})
		case protocol.TypeResponse:
			flushNine(m.Text, msgAt(m))
		case "tool_start", "tool_end", "sub_agent_start", "sub_agent_end":
			if len(segment) == 0 {
				segAt = msgAt(m)
			}
			segment = append(segment, m)
		}
	}
	flushNine("", time.Time{}) // trailing in-flight activity, if any
	return out
}

func formatInput(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > maxInputDisplay {
		return s[:maxInputDisplay] + "…"
	}
	return s
}

func truncateOutput(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxOutputDisplay {
		extra := len(s) - maxOutputDisplay
		return s[:maxOutputDisplay] + fmt.Sprintf("… [+%d chars]", extra)
	}
	return s
}

// wordWrap wraps text at width characters without breaking words.
func wordWrap(text string, width int) string {
	if width <= 0 {
		return text
	}
	var out strings.Builder
	for i, line := range strings.Split(text, "\n") {
		if i > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(wrapLine(line, width))
	}
	return out.String()
}

func wrapLine(line string, width int) string {
	if len(line) <= width {
		return line
	}
	words := strings.Fields(line)
	if len(words) == 0 {
		return line
	}
	var out strings.Builder
	col := 0
	for i, word := range words {
		if i == 0 {
			out.WriteString(word)
			col = len(word)
		} else if col+1+len(word) <= width {
			out.WriteByte(' ')
			out.WriteString(word)
			col += 1 + len(word)
		} else {
			out.WriteByte('\n')
			out.WriteString(word)
			col = len(word)
		}
	}
	return out.String()
}

// connectCmd connects to the daemon (starting it if needed) and creates a new conversation.
func connectCmd(sockPath, binary string) tea.Cmd {
	return func() tea.Msg {
		proc, err := protocol.EnsureDaemon(sockPath, binary)
		if err != nil {
			return errMsg{err}
		}
		c, err := protocol.Connect(sockPath)
		if err != nil {
			return errMsg{err}
		}
		id, role, instanceName, err := c.NewConversationInteractive(true)
		if err != nil {
			c.Close() //nolint:errcheck
			return errMsg{err}
		}
		return connectedMsg{client: c, agentID: id, role: role, instanceName: instanceName, daemonProc: proc}
	}
}

// attachCmd connects to the daemon and attaches to an existing conversation,
// capturing any buffered events and the last response for replay.
func attachCmd(sockPath, binary, agentID string) tea.Cmd {
	return func() tea.Msg {
		proc, err := protocol.EnsureDaemon(sockPath, binary)
		if err != nil {
			return errMsg{err}
		}
		c, err := protocol.Connect(sockPath)
		if err != nil {
			return errMsg{err}
		}
		result, err := c.Attach(agentID)
		if err != nil {
			c.Close() //nolint:errcheck
			return errMsg{err}
		}
		return connectedMsg{
			client:          c,
			agentID:         result.AgentID,
			name:            result.Name,
			instanceName:    result.InstanceName,
			role:            result.Role,
			daemonProc:      proc,
			replayEvents:    replayToToolEvents(result.ReplayEvents),
			pendingResponse: result.PendingResponse,
			history:         historyToChatMsgs(result.History),
		}
	}
}

// isConnDropped reports whether err looks like a lost daemon connection (as
// opposed to a daemon/agent-level error, which the client prefixes "daemon:").
// These are the strings a broken AF_UNIX read surfaces.
func isConnDropped(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, sub := range []string{
		"connection closed", "EOF", "closed network connection",
		"connection reset", "broken pipe", "connection refused",
	} {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// reattachCmd waits for the daemon to come back (e.g. a hot-reload restart) and
// re-attaches to the same session, which the daemon revives from its Postgres
// checkpoint. It polls the socket directly rather than EnsureDaemon, so it never
// spawns a competing daemon over one that is restarting itself.
func reattachCmd(sockPath, agentID string) tea.Cmd {
	return func() tea.Msg {
		deadline := time.Now().Add(60 * time.Second)
		for {
			if protocol.CanConnect(sockPath) {
				if c, err := protocol.Connect(sockPath); err == nil {
					if result, err := c.Attach(agentID); err == nil {
						return connectedMsg{
							client:          c,
							agentID:         result.AgentID,
							name:            result.Name,
							instanceName:    result.InstanceName,
							role:            result.Role,
							replayEvents:    replayToToolEvents(result.ReplayEvents),
							pendingResponse: result.PendingResponse,
							history:         historyToChatMsgs(result.History),
						}
					}
					c.Close() //nolint:errcheck
				}
			}
			if time.Now().After(deadline) {
				return errMsg{err: fmt.Errorf("could not reconnect to the daemon after 60s — is it running?")}
			}
			time.Sleep(400 * time.Millisecond)
		}
	}
}

// answerCmd delivers a human answer to a pending ask_human request. The turn's
// connection is blocked reading the in-flight stream, so the answer is sent on
// a fresh short-lived connection.
func answerCmd(sockPath, agentID, requestID, answer string) tea.Cmd {
	return func() tea.Msg {
		c, err := protocol.Connect(sockPath)
		if err != nil {
			return answerResultMsg{err: err}
		}
		defer c.Close() //nolint:errcheck
		return answerResultMsg{err: c.AnswerHuman(agentID, requestID, answer)}
	}
}

// Run starts the TUI. sockPath is the daemon's Unix socket; binary is os.Args[0].
// cfg is the loaded nine config, used by slash commands like /config.
// attachID, when non-empty, attaches to an existing conversation instead of creating one.
func Run(sockPath, binary string, cfg *config.Config, attachID, version string) error {
	theme := cfg.UI.Theme
	showContext := cfg.UI.ShowContext == nil || *cfg.UI.ShowContext

	var pal palette
	var glamourStyle string
	switch theme {
	case "light":
		pal = lightPalette()
		glamourStyle = "light"
	case "dark":
		pal = darkPalette()
		glamourStyle = "dark"
	default: // "" or "auto" — detect from terminal background before bubbletea owns stdin.
		// Both lipgloss (sync.Once) and glamour (termenv) send an OSC 11 query to
		// detect the background color. If that query fires after bubbletea takes over
		// stdin, the terminal's response arrives as a keypress in the textarea.
		// Calling HasDarkBackground() here, while we still own stdin, caches the
		// lipgloss result and lets us pass an explicit style to glamour so it never
		// queries again inside the event loop.
		pal = autoPalette()
		if lipgloss.HasDarkBackground() {
			glamourStyle = "dark"
		} else {
			glamourStyle = "light"
		}
	}
	p := tea.NewProgram(
		initialModel(sockPath, binary, attachID, pal, glamourStyle, showContext, cfg, version),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)
	_, err := p.Run()
	return err
}
