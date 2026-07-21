package runtime_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"nine/internal/llm"
	"nine/internal/runtime"
)

func TestSanitizeInstanceName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Atlas", "Atlas"},
		{`"Cobalt Fox"`, "Cobalt Fox"},
		{"  spaced  ", "spaced"},
		{"first line\nsecond line", "first line"},
		{"Name: Willow.", "Name: Willow"}, // trailing period stripped
		{"", ""},
		{"   ", ""},
		{strings.Repeat("x", 60), strings.Repeat("x", 32)},
	}
	for _, tc := range cases {
		if got := runtime.SanitizeInstanceName(tc.in); got != tc.want {
			t.Errorf("SanitizeInstanceName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRandomInstanceName(t *testing.T) {
	name := runtime.RandomInstanceName()
	if !strings.Contains(name, "-") {
		t.Errorf("RandomInstanceName() = %q, want an adjective-noun form", name)
	}
	if got := runtime.SanitizeInstanceName(name); got != name {
		t.Errorf("RandomInstanceName() = %q is not already sanitized (got %q)", name, got)
	}
}

// fakeKV is an in-memory InstanceNameStore for testing name resolution.
type fakeKV struct {
	mu   sync.Mutex
	data map[string]string
}

func newFakeKV() *fakeKV { return &fakeKV{data: map[string]string{}} }

func (f *fakeKV) Get(key string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.data[key]
	return v, ok, nil
}

func (f *fakeKV) Set(key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[key] = value
	return nil
}

func (f *fakeKV) get(key string) string {
	v, _, _ := f.Get(key)
	return v
}

func newDaemonForTest(t *testing.T) *runtime.Daemon {
	t.Helper()
	return runtime.New(t.TempDir()+"/s.sock", makeFactory(seqProvider(nil)), nil, nil)
}

func TestResolveInstanceNameConfigWins(t *testing.T) {
	d := newDaemonForTest(t)
	store := newFakeKV()
	// A provider that would fail the test if it were ever called.
	provider := llm.ProviderFunc(func(context.Context, llm.Request) (llm.Response, error) {
		t.Error("provider called despite a configured instance name")
		return llm.Response{}, nil
	})

	d.ResolveInstanceName(context.Background(), "Sentinel", store, provider)

	if got := d.InstanceName(); got != "Sentinel" {
		t.Errorf("InstanceName() = %q, want %q", got, "Sentinel")
	}
	if v := store.get(runtime.InstanceNameKV); v != "" {
		t.Errorf("configured name should not be persisted; store has %q", v)
	}
}

func TestResolveInstanceNameReusesPersisted(t *testing.T) {
	d := newDaemonForTest(t)
	store := newFakeKV()
	store.Set(runtime.InstanceNameKV, "Willow") //nolint:errcheck
	provider := llm.ProviderFunc(func(context.Context, llm.Request) (llm.Response, error) {
		t.Error("provider called despite a persisted instance name")
		return llm.Response{}, nil
	})

	d.ResolveInstanceName(context.Background(), "", store, provider)

	if got := d.InstanceName(); got != "Willow" {
		t.Errorf("InstanceName() = %q, want %q", got, "Willow")
	}
}

func TestResolveInstanceNameGeneratesAndPersists(t *testing.T) {
	d := newDaemonForTest(t)
	store := newFakeKV()
	provider := llm.ProviderFunc(func(context.Context, llm.Request) (llm.Response, error) {
		return llm.Response{Text: "  Cobalt Fox\n"}, nil
	})

	d.ResolveInstanceName(context.Background(), "", store, provider)

	// The placeholder is applied synchronously; the real name arrives async.
	if got := d.InstanceName(); got != runtime.DefaultInstanceName && got != "Cobalt Fox" {
		t.Errorf("initial InstanceName() = %q, want placeholder or generated name", got)
	}

	if !eventually(2*time.Second, func() bool { return d.InstanceName() == "Cobalt Fox" }) {
		t.Fatalf("instance never got its generated name; InstanceName() = %q", d.InstanceName())
	}
	if v := store.get(runtime.InstanceNameKV); v != "Cobalt Fox" {
		t.Errorf("persisted name = %q, want %q", v, "Cobalt Fox")
	}
}

func TestResolveInstanceNameFallsBackWhenLLMFails(t *testing.T) {
	d := newDaemonForTest(t)
	store := newFakeKV()
	provider := llm.ProviderFunc(func(context.Context, llm.Request) (llm.Response, error) {
		return llm.Response{}, errors.New("llm down")
	})

	d.ResolveInstanceName(context.Background(), "", store, provider)

	// A random adjective-noun fallback is generated and persisted.
	if !eventually(2*time.Second, func() bool {
		v := store.get(runtime.InstanceNameKV)
		return v != "" && v != runtime.DefaultInstanceName && strings.Contains(v, "-")
	}) {
		t.Fatalf("no fallback name persisted; store has %q", store.get(runtime.InstanceNameKV))
	}
	if got := d.InstanceName(); got == "" || got == runtime.DefaultInstanceName {
		t.Errorf("InstanceName() = %q, want the fallback name", got)
	}
}

func TestInstanceNameDeliveredOnConnect(t *testing.T) {
	d, sock := startDaemon(t, makeFactory(seqProvider(nil)), nil, nil)
	d.SetInstanceName("Atlas")

	c := dial(t, sock)

	// new_conversation carries the instance name.
	_, _, instanceName, err := c.NewConversationInteractive(true)
	if err != nil {
		t.Fatal(err)
	}
	if instanceName != "Atlas" {
		t.Errorf("new_conversation instance name = %q, want %q", instanceName, "Atlas")
	}
}

func eventually(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}
