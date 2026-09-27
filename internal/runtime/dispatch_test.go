package runtime_test

import (
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"nine/internal/llm"
	"nine/internal/protocol"
)

// Every type in protocol.ClientMsgTypes must be routed by the daemon's dispatch
// switch.
//
// This is the check the typed constants alone cannot give. Go's untyped string
// constants convert implicitly to MsgType, so `case "typo":` still compiles and
// a constant declared but never wired into the switch is invisible until a
// client sends it and gets "unknown message type" back at runtime. Sending each
// one and asserting it does *not* come back unknown is what closes that gap.
//
// The assertion is deliberately weak on semantics: a reply of "no such
// conversation" or a validation error is a pass, because it proves the message
// reached a handler. Only the default branch is a failure.
func TestEveryClientMsgTypeIsDispatched(t *testing.T) {
	provider := seqProvider([]llm.Response{finalResp("ok")})
	_, sock := startDaemon(t, makeFactory(provider), nil, nil)

	for _, mt := range protocol.ClientMsgTypes {
		t.Run(string(mt), func(t *testing.T) {
			conn, err := net.Dial("unix", sock)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close() //nolint:errcheck

			if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
				t.Fatalf("deadline: %v", err)
			}

			// AgentID and Text are filled with placeholders so a handler that
			// requires them fails on its own terms rather than on a nil field.
			msg := protocol.Msg{Type: mt, AgentID: "no-such-agent", Text: "probe"}
			if err := json.NewEncoder(conn).Encode(msg); err != nil {
				t.Fatalf("encode: %v", err)
			}

			var reply protocol.Msg
			if err := json.NewDecoder(conn).Decode(&reply); err != nil {
				// Some handlers close the connection or stream nothing back for
				// a bogus agent id. That still means the message was routed.
				t.Skipf("no reply decoded (%v); the type was still routed", err)
				return
			}
			if reply.Type == protocol.TypeError && strings.Contains(reply.Text, "unknown message type") {
				t.Errorf("%q reached the dispatch switch's default branch: %s\n"+
					"declare a case for it, or drop it from protocol.ClientMsgTypes", mt, reply.Text)
			}
		})
	}
}

// TestEveryQueryKindIsAnswered closes the hole TestEveryClientMsgTypeIsDispatched
// leaves open.
//
// The field-less verbs all decode to one QueryReq and are routed by a second,
// inner switch on Kind. A verb missing a case there does not reach the outer
// default branch and so never produces "unknown message type" — it produces
// *nothing*, and the test above reads a silent connection as "routed" and skips.
// A client calling that verb then blocks until its own deadline, which is how a
// `grants_list` with no inner case reached a built binary.
//
// This asserts the stronger property the inner switch needs: every query kind
// answers something.
func TestEveryQueryKindIsAnswered(t *testing.T) {
	provider := seqProvider([]llm.Response{finalResp("ok")})
	_, sock := startDaemon(t, makeFactory(provider), nil, nil)

	for _, mt := range protocol.ClientMsgTypes {
		// A query kind is exactly a type that decodes to QueryReq.
		req, err := protocol.DecodeRequest(protocol.Msg{Type: mt, AgentID: "no-such-agent", Text: "probe", RequestID: "probe"})
		if err != nil {
			continue
		}
		if _, isQuery := req.(protocol.QueryReq); !isQuery {
			continue
		}

		t.Run(string(mt), func(t *testing.T) {
			conn, err := net.Dial("unix", sock)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close() //nolint:errcheck

			if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
				t.Fatalf("deadline: %v", err)
			}
			if err := json.NewEncoder(conn).Encode(protocol.Msg{Type: mt}); err != nil {
				t.Fatalf("encode: %v", err)
			}

			var reply protocol.Msg
			if err := json.NewDecoder(conn).Decode(&reply); err != nil {
				t.Fatalf("%q produced no reply (%v) — it has no case in the QueryReq "+
					"switch in dispatch, so a client calling it blocks until its own deadline", mt, err)
			}
		})
	}
}
