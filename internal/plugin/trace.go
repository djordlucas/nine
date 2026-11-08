package plugin

import (
	"context"

	"nine/internal/mcp"
)

// Trace-ID helpers live in internal/mcp so the MCP client can carry the ID on
// the wire (HeaderRequestID, socket transport); these keep the package's public
// API stable for the daemon's own logging.

const HeaderRequestID = mcp.HeaderRequestID

// ContextWithRequestID returns a context carrying id as the call's trace ID.
func ContextWithRequestID(ctx context.Context, id string) context.Context {
	return mcp.ContextWithRequestID(ctx, id)
}

// RequestIDFromContext returns the trace ID on ctx, or "" if none is set.
func RequestIDFromContext(ctx context.Context) string {
	return mcp.RequestIDFromContext(ctx)
}

// newRequestID returns a short random hex trace ID.
func newRequestID() string { return mcp.NewRequestID() }
