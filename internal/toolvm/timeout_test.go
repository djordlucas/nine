package toolvm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// spinner never returns, so the only thing that ends the call is the deadline.
const spinner = `export default () => { for (;;) {} };`

func timeoutTool(t *testing.T, cfg Config) *Host {
	t.Helper()
	dir := t.TempDir()
	writeTool(t, dir, "slow", `
name = "slow"
kind = "js"
entrypoint = "./slow.js"
description = "slow"
`, spinner)
	writeTool(t, dir, "other", `
name = "other"
kind = "js"
entrypoint = "./other.js"
description = "other"
`, spinner)
	cfg.UserDir = dir
	ctx := context.Background()
	h, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close(context.Background()) }) //nolint:errcheck
	h.Load(ctx, nil)
	return h
}

// The point of the feature: one tool's deadline must not be every tool's. Raising
// the global bound to accommodate a slow tool also hands that budget to a tool
// with an infinite loop, and this deadline is the only CPU bound the host has.
func TestPerToolTimeoutOverridesTheGlobal(t *testing.T) {
	h := timeoutTool(t, Config{
		Timeout:  150 * time.Millisecond,
		Timeouts: map[string]time.Duration{"slow": 600 * time.Millisecond},
	})

	start := time.Now()
	_, err := h.Call(context.Background(), "slow", json.RawMessage(`{}`))
	slowTook := time.Since(start)
	if err == nil {
		t.Fatal("the spinning tool returned")
	}
	if !strings.Contains(err.Error(), "timed out after 600ms") {
		t.Errorf("error names the wrong deadline: %v", err)
	}
	if slowTook < 400*time.Millisecond {
		t.Errorf("slow tool was killed after %v, want ~600ms — the override was ignored", slowTook)
	}

	// The tool with no override keeps the global bound; it must not inherit the
	// exception made for another tool.
	start = time.Now()
	_, err = h.Call(context.Background(), "other", json.RawMessage(`{}`))
	otherTook := time.Since(start)
	if err == nil {
		t.Fatal("the spinning tool returned")
	}
	if !strings.Contains(err.Error(), "timed out after 150ms") {
		t.Errorf("error names the wrong deadline: %v", err)
	}
	if otherTook > 400*time.Millisecond {
		t.Errorf("other tool ran %v, want ~150ms — it inherited another tool's override", otherTook)
	}
}

// A tool with no override, and a host with no configured timeout, gets the
// package default. Nothing about this feature changes the default path.
func TestTimeoutFallsBackThroughTheChain(t *testing.T) {
	for name, tc := range map[string]struct {
		hostTimeout time.Duration
		override    time.Duration
		want        time.Duration
	}{
		"nothing set anywhere": {0, 0, DefaultTimeout},
		"host only":            {2 * time.Second, 0, 2 * time.Second},
		"override only":        {0, 3 * time.Second, 3 * time.Second},
		"override wins":        {2 * time.Second, 7 * time.Second, 7 * time.Second},
		// An override may also be SHORTER, which is the containment direction:
		// pinning one risky tool below the global bound.
		"override shortens": {30 * time.Second, time.Second, time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			tool := &Tool{Timeout: tc.override}
			host := tc.hostTimeout
			if host == 0 {
				host = DefaultTimeout
			}
			if got := tool.effectiveTimeout(host); got != tc.want {
				t.Errorf("effectiveTimeout = %v, want %v", got, tc.want)
			}
		})
	}
}

// The HTTP bound is derived from the time the call has left, so a tool granted a
// longer deadline is not silently held to a 4s request bound its operator never
// asked for — and one that has already spent most of its budget does not get a
// request bound longer than its remaining life.
func TestHTTPTimeoutTracksTheRemainingDeadline(t *testing.T) {
	t.Run("no deadline uses the default", func(t *testing.T) {
		if got := httpTimeoutFor(context.Background()); got != DefaultHTTPTimeout {
			t.Errorf("got %v, want %v", got, DefaultHTTPTimeout)
		}
	})

	t.Run("reproduces today's 4s-of-5s relationship", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		got := httpTimeoutFor(ctx)
		if got < 3900*time.Millisecond || got > 4*time.Second {
			t.Errorf("got %v, want ~4s (four fifths of 5s)", got)
		}
	})

	t.Run("scales with a longer per-tool deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		got := httpTimeoutFor(ctx)
		if got < 23*time.Second || got > 24*time.Second {
			t.Errorf("got %v, want ~24s", got)
		}
	})

	t.Run("always leaves room for the tool to report", func(t *testing.T) {
		for _, d := range []time.Duration{time.Second, 5 * time.Second, 30 * time.Second, time.Minute} {
			ctx, cancel := context.WithTimeout(context.Background(), d)
			got := httpTimeoutFor(ctx)
			cancel()
			if got >= d {
				t.Errorf("deadline %v: http bound %v is not below it, so the call dies before it can report", d, got)
			}
		}
	})

	t.Run("an exhausted deadline still yields something positive", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
		defer cancel()
		time.Sleep(time.Millisecond)
		if got := httpTimeoutFor(ctx); got <= 0 {
			t.Errorf("got %v, want a positive duration so the failure reads as a timeout", got)
		}
	})
}
