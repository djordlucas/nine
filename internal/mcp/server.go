package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	mcptypes "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// The server side: serving Nine's own tools over MCP, built on the vendored
// mcp-go SDK (adr/modular_nine.md §9.5). It exists for tools that cross a
// process boundary — `nine mcp serve shell` today — so that MCP is the only
// wire protocol across one. Built-in tools that stay in-process never come
// here.

const (
	// TransportEnv selects the serve transport: "stdio", or empty for the
	// default streamable HTTP on SocketEnv (adr/modular_nine.md §9.6: unix-socket
	// HTTP by default, stdio as a per-server opt-in).
	TransportEnv = "NINE_MCP_TRANSPORT"
	// TransportStdio is the stdio transport opt-in.
	TransportStdio = "stdio"
	// SocketEnv is the per-instance Unix socket the plugin manager allocated for
	// this process; the HTTP transport listens on it.
	SocketEnv = "NINE_PLUGIN_SOCKET"
)

// ServerTool is one tool to serve, in the same shape the plugin contract uses:
// the input schema is a raw JSON schema, carried through unchanged (MCP spells
// the field inputSchema; only the envelope key differs).
type ServerTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// Handler is one tool's implementation, in the plugin contract's shape: raw
// JSON arguments in, one string out.
type Handler func(ctx context.Context, args json.RawMessage) (string, error)

// NewServer builds the MCP server advertising tools, each wired to its handler.
// A handler error becomes an in-band isError result, which the client maps back
// to a Go error — the same failure the model sees from the legacy envelope.
func NewServer(name, version string, tools []ServerTool, handlers map[string]Handler) *mcpserver.MCPServer {
	srv := mcpserver.NewMCPServer(name, version, mcpserver.WithToolCapabilities(false))
	for _, t := range tools {
		h := handlers[t.Name]
		if h == nil {
			continue
		}
		tool := mcptypes.NewToolWithRawSchema(t.Name, t.Description, t.InputSchema)
		srv.AddTool(tool, func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
			raw, err := json.Marshal(req.GetArguments())
			if err != nil {
				return nil, fmt.Errorf("marshal arguments: %w", err)
			}
			out, err := h(ctx, raw)
			if err != nil {
				return mcptypes.NewToolResultErrorFromErr(err.Error(), err), nil
			}
			return mcptypes.NewToolResultText(out), nil
		})
	}
	return srv
}

// ServeHTTP serves srv as MCP streamable HTTP on ln until ln is closed. The
// server runs stateless: one plugin socket fronts one daemon, so there is no
// session to hold across requests.
func ServeHTTP(ln net.Listener, srv *mcpserver.MCPServer) error {
	sh := mcpserver.NewStreamableHTTPServer(srv, mcpserver.WithStateLess(true))
	//nolint:gosec // G114: local listener (Unix socket or loopback test socket); per-call deadlines come from the client side
	return (&http.Server{Handler: sh}).Serve(ln)
}

// ServeStdio serves srv over the given pipes until ctx is cancelled.
func ServeStdio(ctx context.Context, stdin io.Reader, stdout io.Writer, srv *mcpserver.MCPServer) error {
	return mcpserver.NewStdioServer(srv).Listen(ctx, stdin, stdout)
}

// Serve blocks serving tools over the transport selected by the environment:
// stdio when NINE_MCP_TRANSPORT=stdio, otherwise streamable HTTP on
// NINE_PLUGIN_SOCKET. It returns only on transport failure; SIGTERM/SIGINT
// stop it cleanly and remove the socket. It is the body of `nine mcp serve
// <name>`, spawned by the plugin manager, not run by hand.
func Serve(name, version string, tools []ServerTool, handlers map[string]Handler) error {
	srv := NewServer(name, version, tools, handlers)

	if os.Getenv(TransportEnv) == TransportStdio {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		return ServeStdio(ctx, os.Stdin, os.Stdout, srv)
	}

	socketPath := os.Getenv(SocketEnv)
	if socketPath == "" {
		return fmt.Errorf("mcp serve: %s is not set and %s is not %q — this subcommand is spawned by the daemon, not run directly",
			SocketEnv, TransportEnv, TransportStdio)
	}
	os.Remove(socketPath) //nolint:errcheck // clear a stale socket before listening
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("mcp serve: listen %s: %w", socketPath, err)
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigs
		ln.Close()            //nolint:errcheck
		os.Remove(socketPath) //nolint:errcheck
		os.Exit(0)
	}()

	err = ServeHTTP(ln, srv)
	os.Remove(socketPath) //nolint:errcheck
	return err
}
