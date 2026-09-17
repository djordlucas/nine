package agent_test

import (
	"context"
	"encoding/json"
	"testing"

	"nine/internal/agent"
)

func TestQueuedMessagesGet(t *testing.T) {
	store := newTestStore(t)
	store.QueueMessage("q-agent-1", "hello")  //nolint:errcheck // test setup
	store.QueueMessage("q-agent-1", "world")  //nolint:errcheck // test setup

	var consumed []string
	d := agent.New()
	agent.RegisterQueuedTools(d, store,
		func() string { return "q-agent-1" },
		func(text string) { consumed = append(consumed, text) },
	)

	res, err := d.Dispatch(context.Background(), "queued_messages_get", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	var out struct {
		Messages []agent.QueuedMessage `json:"messages"`
	}
	if err := json.Unmarshal([]byte(res.Output), &out); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(out.Messages))
	}
	if out.Messages[0].Text != "hello" || out.Messages[1].Text != "world" {
		t.Errorf("messages = %+v", out.Messages)
	}
}

func TestQueuedMessageMarkConsumed(t *testing.T) {
	store := newTestStore(t)
	store.QueueMessage("q-agent-2", "first")  //nolint:errcheck // test setup
	store.QueueMessage("q-agent-2", "second")  //nolint:errcheck // test setup

	var consumed []string
	d := agent.New()
	agent.RegisterQueuedTools(d, store,
		func() string { return "q-agent-2" },
		func(text string) { consumed = append(consumed, text) },
	)

	res, err := d.Dispatch(context.Background(), "queued_message_mark_consumed",
		json.RawMessage(`{"index":0}`))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if res.Output != "first" {
		t.Errorf("output = %q, want 'first'", res.Output)
	}

	// The onConsumed callback should have been called.
	if len(consumed) != 1 || consumed[0] != "first" {
		t.Errorf("onConsumed = %v, want [first]", consumed)
	}

	// Verify the message is marked consumed in the store.
	msgs, _ := store.GetQueuedMessages("q-agent-2")
	if !msgs[0].Consumed {
		t.Error("message 0 should be consumed")
	}
	if msgs[1].Consumed {
		t.Error("message 1 should not be consumed")
	}
}

func TestQueuedMessagesMarkAllConsumed(t *testing.T) {
	store := newTestStore(t)
	store.QueueMessage("q-agent-3", "a")
	store.QueueMessage("q-agent-3", "b")

	var consumed []string
	d := agent.New()
	agent.RegisterQueuedTools(d, store,
		func() string { return "q-agent-3" },
		func(text string) { consumed = append(consumed, text) },
	)

	res, err := d.Dispatch(context.Background(), "queued_messages_mark_all_consumed",
		json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// Both messages should have been consumed.
	if len(consumed) != 2 {
		t.Fatalf("onConsumed called %d times, want 2", len(consumed))
	}
	if consumed[0] != "a" || consumed[1] != "b" {
		t.Errorf("onConsumed = %v, want [a b]", consumed)
	}

	// Output should report the count.
	if res.Output != "marked 2 messages as consumed" {
		t.Errorf("output = %q", res.Output)
	}

	// All messages should be consumed.
	unconsumed, _ := store.UnconsumedMessagesCount("q-agent-3")
	if unconsumed != 0 {
		t.Errorf("unconsumed = %d, want 0", unconsumed)
	}
}

func TestQueuedMessagesCount(t *testing.T) {
	store := newTestStore(t)
	store.QueueMessage("q-agent-4", "one")
	store.QueueMessage("q-agent-4", "two")

	d := agent.New()
	agent.RegisterQueuedTools(d, store,
		func() string { return "q-agent-4" },
		nil,
	)

	res, err := d.Dispatch(context.Background(), "queued_messages_count", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if res.Output != "2" {
		t.Errorf("count = %q, want '2'", res.Output)
	}
}

func TestQueuedMessagesUnconsumedCount(t *testing.T) {
	store := newTestStore(t)
	store.QueueMessage("q-agent-5", "one")  //nolint:errcheck // test setup
	store.QueueMessage("q-agent-5", "two")  //nolint:errcheck // test setup
	store.MarkConsumed(context.Background(), "q-agent-5", 0) //nolint:errcheck // test setup

	d := agent.New()
	agent.RegisterQueuedTools(d, store,
		func() string { return "q-agent-5" },
		nil,
	)

	res, err := d.Dispatch(context.Background(), "queued_messages_unconsumed_count", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if res.Output != "1" {
		t.Errorf("unconsumed count = %q, want '1'", res.Output)
	}
}

func TestQueuedMessageMarkConsumedInvalidIndex(t *testing.T) {
	store := newTestStore(t)
	store.QueueMessage("q-agent-6", "only")

	d := agent.New()
	agent.RegisterQueuedTools(d, store,
		func() string { return "q-agent-6" },
		nil,
	)

	if _, err := d.Dispatch(context.Background(), "queued_message_mark_consumed",
		json.RawMessage(`{"index":5}`)); err == nil {
		t.Error("out-of-range index should error")
	}
}

func TestQueuedToolsNilStore(t *testing.T) {
	// RegisterQueuedTools with a nil store should be a no-op — no panic.
	d := agent.New()
	agent.RegisterQueuedTools(d, nil,
		func() string { return "x" },
		nil,
	)

	if _, err := d.Dispatch(context.Background(), "queued_messages_count", json.RawMessage(`{}`)); err == nil {
		t.Error("dispatch should error when store is nil (tool not registered)")
	}
}
