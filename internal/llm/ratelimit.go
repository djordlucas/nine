package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// RateLimitError is a provider refusing a request for its rate limit (HTTP
// 429). It passes: the Queue waits and sends the request again, honouring the
// provider's Retry-After when it gives one.
type RateLimitError struct {
	Provider string
	// RetryAfter is the wait the provider asked for; zero when it named none.
	RetryAfter time.Duration
	// Detail is the provider's own message, when it sent one.
	Detail string
}

func (e *RateLimitError) Error() string {
	msg := fmt.Sprintf("%s: HTTP 429 rate limited", e.Provider)
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// RetryAfter reads a response's Retry-After header, in seconds or as an HTTP
// date, and returns zero when it is missing or unreadable.
func RetryAfter(h http.Header, now time.Time) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if at, err := http.ParseTime(v); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}

// rateLimitWaits are the Queue's waits before each retry of a rate-limited
// request, when the provider names none: about two minutes in all, enough for
// a per-minute token window to roll over.
var rateLimitWaits = []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second, 60 * time.Second}

// maxRateLimitWait caps a provider's Retry-After, so a provider asking for an
// hour fails the request rather than holding a slot that long.
const maxRateLimitWait = 2 * time.Minute

// completeRetryingRateLimits runs one request, sending it again after each
// rate-limit refusal, up to len(waits) times. The request holds its slot while
// it waits: the limit is the provider's, so another request would be refused
// too. Nothing has streamed when a 429 comes back, so a retry repeats nothing.
func completeRetryingRateLimits(ctx context.Context, p Provider, req Request, waits []time.Duration, sleep func(context.Context, time.Duration) error) (Response, error) {
	for attempt := 0; ; attempt++ {
		resp, err := p.Complete(ctx, req)
		var rl *RateLimitError
		if err == nil || !errors.As(err, &rl) || attempt >= len(waits) {
			return resp, err
		}
		wait := waits[attempt]
		if rl.RetryAfter > 0 {
			wait = min(rl.RetryAfter, maxRateLimitWait)
		}
		slog.Warn("llm rate limited; retrying", "provider", rl.Provider, "attempt", attempt+1, "of", len(waits), "wait", wait)
		if err := sleep(ctx, wait); err != nil {
			return Response{}, err
		}
	}
}

// sleepCtx waits for d, or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
