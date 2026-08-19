package protocol_test

import (
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"

	"nine/internal/protocol"
)

// A daemon error reply must surface as an error, not as a zero-valued success.
// Every request method has this branch and none of it was exercised: a real
// daemon answering correctly never takes it.
func TestClientSurfacesDaemonErrorReplies(t *testing.T) {
	f := newFakeDaemon(t, func(enc *json.Encoder, in protocol.Msg) {
		enc.Encode(protocol.NewErrorMsg("boom: " + string(in.Type))) //nolint:errcheck
	})

	cases := []struct {
		name string
		call func(c *protocol.Client) error
	}{
		{"NewConversation", func(c *protocol.Client) error { _, err := c.NewConversation(); return err }},
		{"Attach", func(c *protocol.Client) error { _, err := c.Attach("a1"); return err }},
		{"Turn", func(c *protocol.Client) error { _, err := c.Turn("a1", "hi"); return err }},
		{"Status", func(c *protocol.Client) error { _, err := c.Status(); return err }},
		{"Context", func(c *protocol.Client) error { _, err := c.Context("a1"); return err }},
		{"ListGoals", func(c *protocol.Client) error { _, err := c.ListGoals(); return err }},
		{"ListWorkflows", func(c *protocol.Client) error { _, err := c.ListWorkflows(); return err }},
		{"ListTools", func(c *protocol.Client) error { _, err := c.ListTools(); return err }},
		{"ListPlugins", func(c *protocol.Client) error { _, err := c.ListPlugins(); return err }},
		{"ListSandboxedTools", func(c *protocol.Client) error { _, err := c.ListSandboxedTools(); return err }},
		{"StopWorkflow", func(c *protocol.Client) error { return c.StopWorkflow("w1") }},
		{"FailWorkflow", func(c *protocol.Client) error { return c.FailWorkflow("w1", false) }},
		{"StopSession", func(c *protocol.Client) error { _, err := c.StopSession("a1", false); return err }},
		{"AnswerHuman", func(c *protocol.Client) error { return c.AnswerHuman("a1", "r1", "yes") }},
		{"SetPlanMode", func(c *protocol.Client) error { _, err := c.SetPlanMode("a1", "plan-only"); return err }},
		{"PluginCall", func(c *protocol.Client) error { _, err := c.PluginCall("t", nil); return err }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(f.connect())
			if err == nil {
				t.Fatal("call succeeded against a daemon that replied with an error")
			}
			if !strings.Contains(err.Error(), "boom") {
				t.Errorf("error = %v, want it to carry the daemon's message", err)
			}
		})
	}
}

// A reply of the wrong type must be rejected rather than read as success. This
// is the branch that protects a client against a daemon it does not fully agree
// with — a version skew, or a bug — and it is the one an external consumer is
// most likely to meet.
func TestClientRejectsWrongReplyType(t *testing.T) {
	// The daemon answers everything with "done", which is a valid message but
	// never the right answer to any of these requests.
	f := newFakeDaemon(t, func(enc *json.Encoder, _ protocol.Msg) {
		enc.Encode(protocol.NewDoneMsg("a1")) //nolint:errcheck
	})

	// Every reply-reading method, not a sample: the point of the check is that it
	// is uniform, and the bug it replaced was one method having it and the next
	// not.
	cases := []struct {
		name string
		call func(c *protocol.Client) error
	}{
		{"NewConversation", func(c *protocol.Client) error { _, err := c.NewConversation(); return err }},
		{"Attach", func(c *protocol.Client) error { _, err := c.Attach("a1"); return err }},
		{"Status", func(c *protocol.Client) error { _, err := c.Status(); return err }},
		{"Context", func(c *protocol.Client) error { _, err := c.Context("a1"); return err }},
		{"ListGoals", func(c *protocol.Client) error { _, err := c.ListGoals(); return err }},
		{"ListReflections", func(c *protocol.Client) error { _, err := c.ListReflections(); return err }},
		{"ListWorkflows", func(c *protocol.Client) error { _, err := c.ListWorkflows(); return err }},
		{"ListNotifications", func(c *protocol.Client) error { _, err := c.ListNotifications(false); return err }},
		{"ListTools", func(c *protocol.Client) error { _, err := c.ListTools(); return err }},
		{"ListPlugins", func(c *protocol.Client) error { _, err := c.ListPlugins(); return err }},
		{"ReloadPlugins", func(c *protocol.Client) error { _, err := c.ReloadPlugins(); return err }},
		{"ListSandboxedTools", func(c *protocol.Client) error { _, err := c.ListSandboxedTools(); return err }},
		{"ReloadSandboxedTools", func(c *protocol.Client) error { _, err := c.ReloadSandboxedTools(); return err }},
		{"StopWorkflow", func(c *protocol.Client) error { return c.StopWorkflow("w1") }},
		{"FailWorkflow", func(c *protocol.Client) error { return c.FailWorkflow("w1", false) }},
		{"StopSession", func(c *protocol.Client) error { _, err := c.StopSession("a1", false); return err }},
		{"AnswerHuman", func(c *protocol.Client) error { return c.AnswerHuman("a1", "r1", "yes") }},
		{"SetPlanMode", func(c *protocol.Client) error { _, err := c.SetPlanMode("a1", "plan-only"); return err }},
		{"PluginCall", func(c *protocol.Client) error { _, err := c.PluginCall("t", nil); return err }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(f.connect())
			if err == nil {
				t.Fatal("call succeeded against a wrong-typed reply")
			}
			if !strings.Contains(err.Error(), "unexpected reply") {
				t.Errorf("error = %v, want it to name the unexpected reply", err)
			}
		})
	}
}

// A turn streams progress events before its final response. The client must hand
// each to the callback and return only the response text.
func TestTurnWithProgressStreamsThenReturns(t *testing.T) {
	f := newFakeDaemon(t, func(enc *json.Encoder, in protocol.Msg) {
		if in.Type != protocol.TypeUserTurn {
			return
		}
		enc.Encode(protocol.NewToolStartMsg("a1", "echo", "Echo", nil))      //nolint:errcheck
		enc.Encode(protocol.NewResponseChunkMsg("a1", "par"))                //nolint:errcheck
		enc.Encode(protocol.NewResponseChunkMsg("a1", "tial"))               //nolint:errcheck
		enc.Encode(protocol.NewToolEndMsg("a1", "echo", "Echo", nil, "out")) //nolint:errcheck
		enc.Encode(protocol.NewResponseMsg("a1", "final answer"))            //nolint:errcheck
		enc.Encode(protocol.NewDoneMsg("a1"))                                //nolint:errcheck
	})

	var got []protocol.MsgType
	var chunks []string
	answer, err := f.connect().TurnWithProgress("a1", "hi", func(e protocol.ProgressEvent) {
		got = append(got, e.Type)
		if e.Type == protocol.TypeResponseChunk {
			chunks = append(chunks, e.Text)
		}
	})
	if err != nil {
		t.Fatalf("TurnWithProgress: %v", err)
	}
	if answer != "final answer" {
		t.Errorf("answer = %q, want %q", answer, "final answer")
	}
	want := []protocol.MsgType{
		protocol.TypeToolStart, protocol.TypeResponseChunk,
		protocol.TypeResponseChunk, protocol.TypeToolEnd,
	}
	if len(got) != len(want) {
		t.Fatalf("progress events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if strings.Join(chunks, "") != "partial" {
		t.Errorf("chunks joined = %q, want %q", strings.Join(chunks, ""), "partial")
	}
	// The final response is not delivered as a progress event — it is the return
	// value, and delivering it twice would double it in a TUI transcript.
	for _, e := range got {
		if e == protocol.TypeResponse {
			t.Error("the final response was also delivered as a progress event")
		}
	}
}

// An error arriving mid-stream ends the turn, rather than being treated as
// progress and waited past.
func TestTurnWithProgressStopsOnMidStreamError(t *testing.T) {
	f := newFakeDaemon(t, func(enc *json.Encoder, in protocol.Msg) {
		if in.Type != protocol.TypeUserTurn {
			return
		}
		enc.Encode(protocol.NewResponseChunkMsg("a1", "start")) //nolint:errcheck
		enc.Encode(protocol.NewErrorMsg("turn exploded"))       //nolint:errcheck
	})

	_, err := f.connect().TurnWithProgress("a1", "hi", nil)
	if err == nil {
		t.Fatal("turn succeeded despite a mid-stream error")
	}
	if !strings.Contains(err.Error(), "turn exploded") {
		t.Errorf("error = %v, want the daemon's message", err)
	}
}

// A nil progress callback must be allowed — the CLI passes none.
func TestTurnWithNilProgressCallback(t *testing.T) {
	f := newFakeDaemon(t, func(enc *json.Encoder, in protocol.Msg) {
		if in.Type != protocol.TypeUserTurn {
			return
		}
		enc.Encode(protocol.NewResponseChunkMsg("a1", "x")) //nolint:errcheck
		enc.Encode(protocol.NewResponseMsg("a1", "done"))   //nolint:errcheck
		enc.Encode(protocol.NewDoneMsg("a1"))               //nolint:errcheck
	})

	answer, err := f.connect().Turn("a1", "hi")
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if answer != "done" {
		t.Errorf("answer = %q, want %q", answer, "done")
	}
}

// A connection that closes mid-turn must produce an error, not an empty success.
// A caller that cannot distinguish the two would show the user a blank reply.
func TestTurnFailsWhenConnectionClosesMidStream(t *testing.T) {
	sock := rawServer(t, func(conn net.Conn) {
		// Read the request, emit one chunk, then hang up.
		io.CopyN(io.Discard, conn, 1) //nolint:errcheck
		b, _ := json.Marshal(protocol.NewResponseChunkMsg("a1", "partial"))
		conn.Write(append(b, '\n')) //nolint:errcheck
		conn.Close()                //nolint:errcheck
	})
	c, err := protocol.Connect(sock)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { c.Close() }) //nolint:errcheck

	if _, err := c.Turn("a1", "hi"); err == nil {
		t.Fatal("Turn succeeded against a connection that closed mid-stream")
	}
}

// A malformed line must be reported as an error rather than decoded into a
// zero-valued Msg and acted on.
func TestClientRejectsMalformedReply(t *testing.T) {
	sock := rawServer(t, func(conn net.Conn) {
		io.CopyN(io.Discard, conn, 1)            //nolint:errcheck
		conn.Write([]byte("{not json at all\n")) //nolint:errcheck
		conn.Close()                             //nolint:errcheck
	})
	c, err := protocol.Connect(sock)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { c.Close() }) //nolint:errcheck

	if _, err := c.NewConversation(); err == nil {
		t.Fatal("NewConversation succeeded against a malformed reply")
	}
}

// Connect must fail cleanly when nothing is listening, rather than returning a
// client that fails later on first use.
func TestConnectFailsWithNoDaemon(t *testing.T) {
	if _, err := protocol.Connect("/tmp/nine-definitely-not-listening.sock"); err == nil {
		t.Fatal("Connect succeeded with no daemon listening")
	}
}

// The request the client puts on the wire is part of the contract: the daemon
// routes on Type and reads AgentID/Text. These assert the encoding rather than
// the reply handling.
func TestClientSendsWellFormedRequests(t *testing.T) {
	f := newFakeDaemon(t, func(enc *json.Encoder, in protocol.Msg) {
		switch in.Type {
		case protocol.TypeNewConversation:
			enc.Encode(protocol.NewConversationIDMsg("c1")) //nolint:errcheck
		case protocol.TypeUserTurn:
			enc.Encode(protocol.NewResponseMsg("a1", "ok")) //nolint:errcheck
			enc.Encode(protocol.NewDoneMsg("a1"))           //nolint:errcheck
		default:
			enc.Encode(protocol.NewTextMsg(in.Type, "{}")) //nolint:errcheck
		}
	})

	t.Run("user_turn carries agent and text", func(t *testing.T) {
		c := f.connect()
		if _, err := c.Turn("agent-7", "hello there"); err != nil {
			t.Fatalf("Turn: %v", err)
		}
		got := f.lastReceived()
		if got.Type != protocol.TypeUserTurn {
			t.Errorf("Type = %q, want %q", got.Type, protocol.TypeUserTurn)
		}
		if got.AgentID != "agent-7" || got.Text != "hello there" {
			t.Errorf("sent %+v, want agent-7 / \"hello there\"", got)
		}
	})

	t.Run("interactive flag reaches the wire", func(t *testing.T) {
		c := f.connect()
		if _, _, _, err := c.NewConversationInteractive(true); err != nil {
			t.Fatalf("NewConversationInteractive: %v", err)
		}
		if got := f.lastReceived(); !got.Interactive {
			t.Errorf("Interactive = false, want true (only the TUI sets it)")
		}
	})

	// --all is carried as the sentinel Text "--all" rather than a bool field, so
	// a client that sent an agent id instead would silently stop one session.
	t.Run("session_stop --all uses the --all sentinel", func(t *testing.T) {
		c := f.connect()
		if _, err := c.StopSession("", true); err != nil {
			t.Fatalf("StopSession: %v", err)
		}
		got := f.lastReceived()
		if got.Type != protocol.TypeSessionStop || got.Text != "--all" {
			t.Errorf("sent %+v, want session_stop with Text %q", got, "--all")
		}
	})
}
