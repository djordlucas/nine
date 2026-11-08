package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"nine/internal/builtins"
	"nine/internal/cli"
	"nine/internal/config"
	"nine/internal/mcp"
)

// Version is the release version. It is overridden at build time via
// -ldflags "-X main.Version=$(VERSION)" (see the Makefile); the default below
// applies to plain `go build` / `go run` invocations.
var Version = "dev"

func main() {
	// An MCP-serving built-in child does one thing and nothing else. Dispatch it
	// before the logger and the config are touched, so none of the process-wide
	// startup below ever runs in a plugin: see serveMCPAndExit for why each
	// piece of it is inappropriate there (spec/contracts/plugin.md R-PLUG.13).
	if name, isServe := mcpServeArgs(os.Args[1:]); isServe {
		serveMCPAndExit(name)
	}

	// API server mode: `nine api serve` starts the HTTP API server as a
	// separate process that communicates with the daemon via Unix socket.
	if apiServeArgs(os.Args[1:]) {
		serveAPIAndExit()
	}

	setupLogger(len(os.Args) > 1 && os.Args[1] == "daemon")

	cfg, err := config.LoadDefault()
	if err != nil {
		// A config Nine cannot understand is a stop, not a warning: running on
		// defaults would look like a clean boot while silently dropping every
		// setting the operator wrote.
		fmt.Fprintln(os.Stderr, "nine: "+err.Error())
		os.Exit(1)
	}

	c := cli.New(os.Args[0])
	c.Version = Version
	c.StartDaemon = runDaemon

	if err := c.Run(os.Args[1:], cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// mcpServeArgs reports whether argv invokes `nine mcp serve [name]`, and the
// name if one was given. A bare `mcp serve` still reports true so the missing
// name is answered by serveMCPAndExit — which knows the MCP-servable roster —
// rather than falling through to the CLI.
func mcpServeArgs(args []string) (name string, isServe bool) {
	if len(args) >= 2 && args[0] == "mcp" && args[1] == "serve" {
		if len(args) >= 3 {
			return args[2], true
		}
		return "", true
	}
	return "", false
}

// serveMCPAndExit runs one MCP-serving built-in and never returns. It is the
// whole of what an `nine mcp serve <name>` child process does; see
// servePluginAndExit for why a plugin child must not run the ordinary startup.
func serveMCPAndExit(name string) {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	// Fail closed outside the daemon. An MCP-serving built-in is only ever
	// spawned by the plugin manager, which allocates the socket first; with no
	// socket and no stdio opt-in there is nothing to serve and no caller to
	// answer. mcp.Serve enforces this too.
	stdio := os.Getenv(mcp.TransportEnv) == mcp.TransportStdio
	if !stdio && os.Getenv(mcp.SocketEnv) == "" {
		fmt.Fprintf(os.Stderr, "nine mcp serve: %s is not set — this subcommand is spawned by the daemon, not run directly (set %s=%s to serve over stdio)\n",
			mcp.SocketEnv, mcp.TransportEnv, mcp.TransportStdio)
		os.Exit(1)
	}
	if name == "" {
		fmt.Fprintf(os.Stderr, "usage: nine mcp serve <%s>\n", strings.Join(builtins.MCPNames(), "|"))
		os.Exit(1)
	}
	if err := builtins.ServeMCP(name, Version); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// apiServeArgs reports whether argv invokes `nine api serve`.
func apiServeArgs(args []string) bool {
	return len(args) >= 1 && args[0] == "api" && (len(args) == 1 || args[1] == "serve")
}

// setupLogger configures the global slog logger. The daemon's log is its
// account of itself; a client command (`nine status`, which the container's
// health check runs every 30 seconds) logs only warnings, to stderr, and never
// touches the file, so it neither floods the daemon's log nor creates the file
// as a user the daemon cannot write as.
//
// The daemon's log destination (in priority order):
//  1. NINE_LOG_FILE=off  → stderr only (disables file logging)
//  2. default             → <binary-dir>/nine.log (appended)
//
// The file is a best-effort default: if it cannot be opened — an installed
// binary in a directory the user cannot write to is the common case — this
// silently falls back to stderr, so the stderr case is not only the explicit
// opt-out.
//
// Other env vars:
//
//	NINE_LOG_LEVEL=debug   verbose output
//	NINE_LOG_FORMAT=json   machine-readable JSON
func setupLogger(daemon bool) {
	level := slog.LevelInfo
	if !daemon {
		level = slog.LevelWarn
	}
	if v := os.Getenv("NINE_LOG_LEVEL"); v != "" {
		var l slog.Level
		if err := l.UnmarshalText([]byte(v)); err == nil {
			level = l
		}
	}

	var w io.Writer = os.Stderr

	if daemon && os.Getenv("NINE_LOG_FILE") != "off" {
		if exePath, err := os.Executable(); err == nil {
			logPath := filepath.Join(filepath.Dir(exePath), "nine.log")
			if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600); err == nil {
				w = f // file stays open for process lifetime
			}
		}
	}

	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if os.Getenv("NINE_LOG_FORMAT") == "json" {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	slog.SetDefault(slog.New(h))
	if daemon {
		slog.Info("nine", "version", Version)
	}
}
