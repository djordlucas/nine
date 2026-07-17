package runtime_test

import (
	"testing"

	"nine/internal/protocol"
	"nine/internal/runtime"
	"nine/internal/llm"
)

func TestNameFromPrompt(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"help me refactor the auth module", "help-me-refactor-the-auth"},
		{"what is 2+2", "what-is-22"},
		{"Fix the bug", "fix-the-bug"},
		{"   ", ""},
		{"", ""},
		{"one two three four five six seven", "one-two-three-four-five"},
		{"Hello, World! How are you doing today?", "hello-world-how-are-you"},
		// single long word is truncated at 32 chars
		{"abcdefghijklmnopqrstuvwxyzabcdefghijklm", "abcdefghijklmnopqrstuvwxyzabcdef"},
	}
	for _, tc := range cases {
		got := runtime.NameFromPrompt(tc.input)
		if got != tc.want {
			t.Errorf("NameFromPrompt(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestSessionNameSetOnFirstTurn(t *testing.T) {
	provider := seqProvider([]llm.Response{finalResp("ok")})
	_, sock := startDaemon(t, makeFactory(provider), nil, nil)
	c := dial(t, sock)

	id, err := c.NewConversation()
	if err != nil {
		t.Fatal(err)
	}

	var gotName string
	_, err = c.TurnWithProgress(id, "help me write a test", func(evt protocol.ProgressEvent) {
		if evt.Type == "set_name" {
			gotName = evt.Text
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotName != "help-me-write-a-test" {
		t.Errorf("set_name = %q, want %q", gotName, "help-me-write-a-test")
	}
}

func TestSessionNameAppearsInStatus(t *testing.T) {
	provider := seqProvider([]llm.Response{finalResp("ok")})
	_, sock := startDaemon(t, makeFactory(provider), nil, nil)
	c := dial(t, sock)

	id, err := c.NewConversation()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Turn(id, "fix the login bug"); err != nil {
		t.Fatal(err)
	}

	info, err := c.Status()
	if err != nil {
		t.Fatal(err)
	}
	var found protocol.AgentInfo
	for _, a := range info.Agents {
		if a.ID == id {
			found = a
		}
	}
	if found.ID == "" {
		t.Fatal("agent not found in status")
	}
	if found.Name != "fix-the-login-bug" {
		t.Errorf("Name = %q, want %q", found.Name, "fix-the-login-bug")
	}
}

func TestSessionNameNotSetBeforeFirstTurn(t *testing.T) {
	provider := seqProvider([]llm.Response{finalResp("ok")})
	_, sock := startDaemon(t, makeFactory(provider), nil, nil)
	c := dial(t, sock)

	id, err := c.NewConversation()
	if err != nil {
		t.Fatal(err)
	}

	info, err := c.Status()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range info.Agents {
		if a.ID == id && a.Name != "" {
			t.Errorf("expected no name before first turn, got %q", a.Name)
		}
	}
}

func TestSessionNameNotOverwrittenOnSubsequentTurns(t *testing.T) {
	provider := seqProvider([]llm.Response{finalResp("r1"), finalResp("r2")})
	_, sock := startDaemon(t, makeFactory(provider), nil, nil)
	c := dial(t, sock)

	id, err := c.NewConversation()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Turn(id, "first prompt sets name"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Turn(id, "second prompt should not change name"); err != nil {
		t.Fatal(err)
	}

	info, err := c.Status()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range info.Agents {
		if a.ID == id {
			if a.Name != "first-prompt-sets-name" {
				t.Errorf("Name = %q, want %q", a.Name, "first-prompt-sets-name")
			}
		}
	}
}

func TestAttachByName(t *testing.T) {
	provider := seqProvider([]llm.Response{finalResp("ok"), finalResp("ok2")})
	_, sock := startDaemon(t, makeFactory(provider), nil, nil)
	c := dial(t, sock)

	id, err := c.NewConversation()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Turn(id, "debug the cache layer"); err != nil {
		t.Fatal(err)
	}

	c2 := dial(t, sock)
	res, err := c2.Attach("debug-the-cache-layer")
	if err != nil {
		t.Fatalf("Attach by name: %v", err)
	}
	if res.AgentID != id {
		t.Errorf("resolved ID = %q, want %q", res.AgentID, id)
	}
}

func TestAttachByPrefix(t *testing.T) {
	provider := seqProvider([]llm.Response{finalResp("ok"), finalResp("ok2")})
	_, sock := startDaemon(t, makeFactory(provider), nil, nil)
	c := dial(t, sock)

	id, err := c.NewConversation()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Turn(id, "some task"); err != nil {
		t.Fatal(err)
	}

	prefix := id[:8]
	c2 := dial(t, sock)
	res2, err := c2.Attach(prefix)
	if err != nil {
		t.Fatalf("Attach by prefix: %v", err)
	}
	if res2.AgentID != id {
		t.Errorf("resolved ID = %q, want %q", res2.AgentID, id)
	}
}

func TestSessionNamePersistedAndRestoredOnAttach(t *testing.T) {
	ckpt := runtime.NewInMemoryCheckpointStore()
	provider := seqProvider([]llm.Response{finalResp("ok")})
	_, sock := startDaemon(t, makeFactory(provider), ckpt, nil)
	c := dial(t, sock)

	id, err := c.NewConversation()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Turn(id, "refactor the payment service"); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()

	// New daemon backed by the same checkpoint store.
	_, sock2 := startDaemon(t, makeFactory(provider), ckpt, nil)
	c2 := dial(t, sock2)

	ar, err := c2.Attach(id)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if ar.Name != "refactor-the-payment-service" {
		t.Errorf("Attach name = %q, want %q", ar.Name, "refactor-the-payment-service")
	}
}
