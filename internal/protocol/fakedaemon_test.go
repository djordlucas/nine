package protocol_test

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"nine/internal/protocol"
)

// fakeDaemon is a scriptable server on a Unix socket.
//
// The client already has integration coverage through internal/runtime's daemon
// tests, but every one of those exchanges goes through a *correct* daemon. That
// leaves the client's defensive branches — an error reply, a reply of the wrong
// type, a stream that stops early, a malformed line — unexercised, which is
// exactly the behavior an external consumer depends on and a healthy daemon never
// produces. A scripted server is the only way to reach them.
type fakeDaemon struct {
	t    *testing.T
	sock string
	ln   net.Listener

	mu       sync.Mutex
	received []protocol.Msg
}

// newFakeDaemon starts a server that, for each accepted connection, reads one
// message and then runs handle to produce the reply stream. Sockets go under a
// short /tmp path: macOS caps Unix-socket paths at 104 bytes.
func newFakeDaemon(t *testing.T, handle func(enc *json.Encoder, in protocol.Msg)) *fakeDaemon {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "nine-fake")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	f := &fakeDaemon{t: t, sock: filepath.Join(dir, "d.sock")}
	ln, err := net.Listen("unix", f.sock)
	if err != nil {
		t.Fatal(err)
	}
	f.ln = ln
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			go func() {
				defer conn.Close() //nolint:errcheck
				dec := json.NewDecoder(conn)
				enc := json.NewEncoder(conn)
				for {
					var in protocol.Msg
					if err := dec.Decode(&in); err != nil {
						return
					}
					f.mu.Lock()
					f.received = append(f.received, in)
					f.mu.Unlock()
					handle(enc, in)
				}
			}()
		}
	}()
	return f
}

// connect dials the fake as a real client would.
func (f *fakeDaemon) connect() *protocol.Client {
	f.t.Helper()
	c, err := protocol.Connect(f.sock)
	if err != nil {
		f.t.Fatalf("connect: %v", err)
	}
	f.t.Cleanup(func() { c.Close() }) //nolint:errcheck
	return c
}

func (f *fakeDaemon) lastReceived() protocol.Msg {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.received) == 0 {
		f.t.Fatal("the fake daemon received no message")
	}
	return f.received[len(f.received)-1]
}

// rawServer is for cases that must write bytes the Msg encoder would never
// produce — malformed JSON, or a connection that closes mid-exchange.
func rawServer(t *testing.T, handle func(conn net.Conn)) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "nine-raw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handle(conn)
		}
	}()
	return sock
}
