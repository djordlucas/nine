package plugin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

// HeaderRequestID carries a per-call trace ID from the daemon to the plugin for
// log correlation. It is not part of the RPC envelope — reply↔request
// correlation is already per-connection; this is purely for grepping one ID
// across daemon and plugin logs once calls run concurrently.
const HeaderRequestID = "X-Nine-Request-ID"

type requestIDKey struct{}

// ContextWithRequestID returns a context carrying id as the call's trace ID.
func ContextWithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFromContext returns the trace ID on ctx, or "" if none is set.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// newRequestID returns a short random hex trace ID.
func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}
