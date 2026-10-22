package llm

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// A rate-limited request is sent again after each wait, the provider's
// Retry-After winning over the default and capped; it fails once the waits run
// out, and any other error is returned at once.
func TestCompleteRetriesRateLimits(t *testing.T) {
	var waited []time.Duration
	sleep := func(_ context.Context, d time.Duration) error { waited = append(waited, d); return nil }
	waits := []time.Duration{time.Second, 2 * time.Second, 3 * time.Second}

	refusals := []error{
		&RateLimitError{Provider: "p"},
		&RateLimitError{Provider: "p", RetryAfter: 7 * time.Second},
		&RateLimitError{Provider: "p", RetryAfter: time.Hour},
	}
	n := 0
	p := ProviderFunc(func(context.Context, Request) (Response, error) {
		if n < len(refusals) {
			n++
			return Response{}, refusals[n-1]
		}
		return Response{Text: "ok"}, nil
	})
	resp, err := completeRetryingRateLimits(context.Background(), p, Request{}, waits, sleep)
	if err != nil || resp.Text != "ok" {
		t.Fatalf("got %q, %v; want the answer after three retries", resp.Text, err)
	}
	want := []time.Duration{time.Second, 7 * time.Second, maxRateLimitWait}
	if len(waited) != len(want) {
		t.Fatalf("waits = %v, want %v", waited, want)
	}
	for i := range want {
		if waited[i] != want[i] {
			t.Errorf("wait %d = %s, want %s", i, waited[i], want[i])
		}
	}

	always := ProviderFunc(func(context.Context, Request) (Response, error) { return Response{}, &RateLimitError{Provider: "p"} })
	waited = nil
	_, err = completeRetryingRateLimits(context.Background(), always, Request{}, waits, sleep)
	var rl *RateLimitError
	if !errors.As(err, &rl) || len(waited) != len(waits) {
		t.Errorf("err = %v after %d waits; want the rate limit after %d", err, len(waited), len(waits))
	}

	other := errors.New("bad request")
	calls := 0
	failing := ProviderFunc(func(context.Context, Request) (Response, error) { calls++; return Response{}, other })
	if _, err := completeRetryingRateLimits(context.Background(), failing, Request{}, waits, sleep); !errors.Is(err, other) || calls != 1 {
		t.Errorf("err = %v after %d calls; want the error at once", err, calls)
	}
}

// A cancelled wait ends the request with the context's error.
func TestRateLimitWaitEndsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := ProviderFunc(func(context.Context, Request) (Response, error) { return Response{}, &RateLimitError{Provider: "p"} })
	if _, err := completeRetryingRateLimits(ctx, p, Request{}, []time.Duration{time.Hour}, sleepCtx); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestRetryAfterReadsSecondsAndDates(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	cases := map[string]time.Duration{
		"":                              0,
		"30":                            30 * time.Second,
		"-3":                            0,
		"soon":                          0,
		"Thu, 22 Oct 2026 12:01:00 GMT": time.Minute,
		"Thu, 22 Oct 2026 11:00:00 GMT": 0,
	}
	for v, want := range cases {
		h := http.Header{}
		if v != "" {
			h.Set("Retry-After", v)
		}
		if got := RetryAfter(h, now); got != want {
			t.Errorf("RetryAfter(%q) = %s, want %s", v, got, want)
		}
	}
}
