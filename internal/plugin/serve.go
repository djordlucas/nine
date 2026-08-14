package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// RPCErr is a JSON-RPC 2.0 error object returned by plugin handlers.
type RPCErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCErr) Error() string { return e.Message }

// Errorf creates an internal error (-32603).
func Errorf(format string, a ...any) *RPCErr {
	return &RPCErr{Code: -32603, Message: fmt.Sprintf(format, a...)}
}

// InvalidArgs creates an invalid-params error (-32602).
func InvalidArgs(format string, a ...any) *RPCErr {
	return &RPCErr{Code: -32602, Message: fmt.Sprintf(format, a...)}
}

// Schema is a convenience wrapper for inline JSON schema literals.
func Schema(s string) json.RawMessage { return json.RawMessage(s) }

// ToolHandler handles a single tool call. The context is the inbound request's
// context, cancelled if the daemon cancels the call (e.g. task_timeout).
type ToolHandler func(ctx context.Context, args json.RawMessage) (string, error)

// ServeOption configures Serve.
type ServeOption func(*serveConfig)

type serveConfig struct {
	maxConcurrent int
	jobs          *Jobs
	jobHandlers   map[string]JobHandler
	onShutdown    func()
}

// WithOnShutdown registers cleanup to run when the plugin is signalled, before
// the process exits.
//
// A plugin that owns something beyond its socket — the MCP bridge owns a server
// process — cannot do this with its own signal handler: Serve's handler calls
// os.Exit as soon as it wakes, so two handlers racing means the cleanup may
// simply never run. Registering it here makes the ordering deterministic.
func WithOnShutdown(fn func()) ServeOption {
	return func(c *serveConfig) { c.onShutdown = fn }
}

// WithMaxConcurrent advertises a concurrency cap to the daemon (see
// DescribeResult.MaxConcurrent). Omit it for stateless plugins, which default
// to unbounded; pass a finite value only for plugins with shared state. The same
// cap bounds concurrent jobs (docs/plugin-capabilities.md §5).
func WithMaxConcurrent(n int) ServeOption {
	return func(c *serveConfig) { c.maxConcurrent = n }
}

// WithJobHandlers registers the plugin's long-running job tools by name
// (docs/plugin-capabilities.md §5). A tool with a job handler returns a job id
// from plugin.call instead of a result; the daemon then polls job_status. It is
// what makes Serve advertise async_jobs.
func WithJobHandlers(handlers map[string]JobHandler) ServeOption {
	return func(c *serveConfig) { c.jobHandlers = handlers }
}

// WithJobs installs the job registry Serve drives. Optional: with job handlers
// but no registry, Serve creates one. Pass an explicit NewJobs() when the plugin
// needs its own handle to it.
func WithJobs(jobs *Jobs) ServeOption {
	return func(c *serveConfig) { c.jobs = jobs }
}

// Serve runs the plugin's HTTP transport: it listens on the Unix socket named by
// NINE_PLUGIN_SOCKET and answers plugin.describe / plugin.call on POST /rpc, one
// goroutine per request. It blocks until the process is signalled. tools defines
// the advertised tool set; handlers maps tool names to functions.
func Serve(tools []ToolDefinition, handlers map[string]ToolHandler, opts ...ServeOption) {
	ServeDeferred(func(context.Context) ([]ToolDefinition, map[string]ToolHandler, error) {
		return tools, handlers, nil
	}, opts...)
}

// ReadyFunc resolves a plugin's tools and handlers. See ServeDeferred.
type ReadyFunc func(ctx context.Context) ([]ToolDefinition, map[string]ToolHandler, error)

// ServeDeferred is Serve for a plugin that cannot name its tools until it has
// done some work — typically talking to something else first.
//
// It listens **before** calling ready, then answers plugin.describe and
// plugin.call once ready returns. That ordering matters because the daemon
// bounds how long it waits for a plugin's socket to appear (~3s, R-PLUG.3),
// while plugin.describe itself has no such deadline. A plugin that handshakes
// with a remote service before listening spends that startup inside the socket
// budget and is declared dead; doing it after moves the same work into describe,
// where the daemon is willing to wait.
//
// The MCP bridge is the case this exists for: `npx`-launched servers routinely
// take longer than the socket budget to come up (R-PLUG.15).
//
// If ready fails, the plugin answers requests with that error — so the daemon
// logs the real reason rather than a bare connection failure — and then exits.
func ServeDeferred(ready ReadyFunc, opts ...ServeOption) {
	var cfg serveConfig
	for _, o := range opts {
		o(&cfg)
	}

	// Wire up jobs when the plugin registered any handler: honour the concurrency
	// cap for jobs and hand per-job dirs out of the plugin cache dir.
	async := len(cfg.jobHandlers) > 0
	var jobs *Jobs
	if async {
		jobs = cfg.jobs
		if jobs == nil {
			jobs = NewJobs()
		}
		jobs.configure(cfg.maxConcurrent, os.Getenv("NINE_PLUGIN_CACHE_DIR"))
	}

	socketPath := os.Getenv("NINE_PLUGIN_SOCKET")
	if socketPath == "" {
		fmt.Fprintln(os.Stderr, "plugin: NINE_PLUGIN_SOCKET not set")
		os.Exit(1)
	}

	os.Remove(socketPath) //nolint:errcheck // clear a stale socket before listening
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "plugin: listen %s: %v\n", socketPath, err)
		os.Exit(1)
	}

	// Clean up the socket on a graceful stop (the daemon sends SIGTERM).
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigs
		ln.Close()            //nolint:errcheck
		os.Remove(socketPath) //nolint:errcheck
		if cfg.onShutdown != nil {
			cfg.onShutdown()
		}
		os.Exit(0)
	}()

	// Resolve tools after the listener is up, so a slow ready() costs describe
	// time rather than socket-ready time.
	res := &deferredTools{done: make(chan struct{}), maxConcurrent: cfg.maxConcurrent, async: async}
	go res.resolve(ready)

	mux := http.NewServeMux()
	mux.HandleFunc("/rpc", func(w http.ResponseWriter, r *http.Request) {
		describe, handlers, err := res.await(r.Context())
		if err != nil {
			writeRPC(w, nil, Errorf("plugin not ready: %v", err))
			return
		}
		handleRPC(w, r, describe, handlers, cfg.jobHandlers, jobs)
	})

	if err := http.Serve(ln, mux); err != nil { //nolint:gosec // G114: local Unix-socket RPC server; per-call deadlines come from the daemon-side transport
		fmt.Fprintf(os.Stderr, "plugin: serve: %v\n", err)
	}
}

// deferredTools holds the once-resolved result of a ReadyFunc. Requests block on
// done rather than racing it, so describe and call see a fully-built tool set or
// the reason there isn't one.
type deferredTools struct {
	done          chan struct{}
	maxConcurrent int
	async         bool

	describe DescribeResult
	handlers map[string]ToolHandler
	err      error
}

// resolve runs ready once and publishes the outcome. On failure it gives the
// daemon a moment to collect the error from a describe reply before exiting —
// a plugin that cannot build its tools has nothing to serve, and lingering would
// leave it listed as healthy.
func (d *deferredTools) resolve(ready ReadyFunc) {
	tools, handlers, err := ready(context.Background())
	if err != nil {
		d.err = err
		close(d.done)
		fmt.Fprintf(os.Stderr, "plugin: not ready: %v\n", err)
		time.Sleep(readyFailureGrace)
		os.Exit(1)
	}
	d.describe = DescribeResult{
		ProtocolVersion: ProtocolVersion,
		Tools:           tools,
		MaxConcurrent:   d.maxConcurrent,
		AsyncJobs:       d.async,
	}
	d.handlers = handlers
	close(d.done)
}

// await blocks until the tool set is resolved, or the caller gives up.
func (d *deferredTools) await(ctx context.Context) (DescribeResult, map[string]ToolHandler, error) {
	select {
	case <-d.done:
		return d.describe, d.handlers, d.err
	case <-ctx.Done():
		return DescribeResult{}, nil, ctx.Err()
	}
}

// readyFailureGrace is how long a plugin whose ready() failed stays up so the
// daemon can read the reason off a describe reply instead of a dropped
// connection.
const readyFailureGrace = 2 * time.Second

// handleRPC decodes one request envelope and writes one reply envelope. There is
// no JSON-RPC id: each call owns its connection, so correlation is per-connection.
func handleRPC(w http.ResponseWriter, r *http.Request, describe DescribeResult, handlers map[string]ToolHandler, jobHandlers map[string]JobHandler, jobs *Jobs) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeRPC(w, nil, Errorf("read body: %v", err))
		return
	}

	var req struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPC(w, nil, InvalidArgs("invalid request: %v", err))
		return
	}

	// Carry the daemon's trace ID onto the handler ctx and log it, so the same
	// ID greps across daemon and plugin logs.
	reqID := r.Header.Get(HeaderRequestID)
	ctx := r.Context()
	if reqID != "" {
		ctx = ContextWithRequestID(ctx, reqID)
	}

	var result any
	var rpcErr *RPCErr

	switch req.Method {
	case "plugin.describe":
		result = describe

	case "plugin.call":
		var p struct {
			Tool string          `json:"tool"`
			Args json.RawMessage `json:"args"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			rpcErr = InvalidArgs("invalid params: %v", err)
			break
		}
		slog.Debug("plugin.call", "tool", p.Tool, "request_id", reqID) //nolint:gosec // G706: tool/request_id are internal daemon values, not untrusted input

		// A job tool validates synchronously, then returns a handle: the work runs
		// detached and the daemon polls job_status for the result.
		if jh, ok := jobHandlers[p.Tool]; ok {
			job, err := jh(ctx, p.Args)
			if err != nil {
				rpcErr = asRPCErr(err)
				break
			}
			id := jobs.start(job)
			result = map[string]string{"job_id": id, "output": job.Ack}
			break
		}

		h, ok := handlers[p.Tool]
		if !ok {
			rpcErr = &RPCErr{-32601, "unknown tool: " + p.Tool}
			break
		}
		output, err := h(ctx, p.Args)
		if err != nil {
			rpcErr = asRPCErr(err)
			break
		}
		result = map[string]string{"output": output}

	case "plugin.job_status":
		if jobs == nil {
			rpcErr = &RPCErr{-32601, "plugin does not support jobs"}
			break
		}
		var p struct {
			JobID string `json:"job_id"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			rpcErr = InvalidArgs("invalid params: %v", err)
			break
		}
		result = jobs.status(p.JobID)

	case "plugin.job_cancel":
		if jobs == nil {
			rpcErr = &RPCErr{-32601, "plugin does not support jobs"}
			break
		}
		var p struct {
			JobID string `json:"job_id"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			rpcErr = InvalidArgs("invalid params: %v", err)
			break
		}
		result = map[string]bool{"cancelled": jobs.cancel(p.JobID)}

	default:
		rpcErr = &RPCErr{-32601, "method not found: " + req.Method}
	}

	writeRPC(w, result, rpcErr)
}

// asRPCErr maps a handler error to an RPC error, preserving an explicit *RPCErr
// (with its code) and wrapping anything else as an internal error.
func asRPCErr(err error) *RPCErr {
	if re, ok := err.(*RPCErr); ok {
		return re
	}
	return Errorf("%v", err)
}

// writeRPC encodes the reply envelope. The HTTP status is always 200; failures
// live in the body's "error" field.
func writeRPC(w http.ResponseWriter, result any, rpcErr *RPCErr) {
	resp := map[string]any{}
	if rpcErr != nil {
		resp["error"] = rpcErr
	} else {
		resp["result"] = result
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp) //nolint:errcheck,gosec // write to response; errors unrecoverable
}
