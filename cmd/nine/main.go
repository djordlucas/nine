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
	"nine/internal/tui"
)

// Version is the release version. It is overridden at build time via
// -ldflags "-X main.Version=$(VERSION)" (see the Makefile); the default below
// applies to plain `go build` / `go run` invocations.
var Version = "dev"

func main() {
	// A built-in plugin child does one thing and nothing else. Dispatch it
	// before the logger and the config are touched, so none of the process-wide
	// startup below ever runs in a plugin: see servePluginAndExit for why each
	// piece of it is inappropriate there (spec/contracts/plugin.md R-PLUG.13).
	if name, isServe := pluginServeArgs(os.Args[1:]); isServe {
		servePluginAndExit(name)
	}

	logsToStderr := setupLogger()

	cfg := config.LoadDefault()

	c := cli.New(os.Args[0])
	c.Version = Version
	c.StartDaemon = runDaemon
	c.StartTUI = func(attachID string) error {
		// The TUI renders a full-screen UI over this terminal, so stderr is a
		// display surface here, not a log sink — a single slog line painted
		// into the chat area corrupts the frame. When that is where logs are
		// going (NINE_LOG_FILE=off, or the log file could not be opened), drop
		// them for the life of the TUI. Nothing is lost that matters: the
		// client side logs almost nothing, and the daemon is a separate
		// process with its own destination.
		if logsToStderr {
			slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
		}
		return tui.Run(cfg.SocketPath(), os.Args[0], cfg, attachID, Version)
	}

	if err := c.Run(os.Args[1:], cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// pluginServeArgs reports whether argv invokes `nine plugin serve [name]`, and
// the name if one was given. A bare `plugin serve` still reports true so the
// missing name is answered by servePluginAndExit — which knows the built-in
// roster — rather than falling through to the CLI's `plugin validate` usage.
func pluginServeArgs(args []string) (name string, isServe bool) {
	if len(args) >= 2 && args[0] == "plugin" && args[1] == "serve" {
		if len(args) >= 3 {
			return args[2], true
		}
		return "", true
	}
	return "", false
}

// servePluginAndExit runs one built-in plugin and never returns. It is the whole
// of what a plugin child process does.
//
// `nine plugin serve <name>` is spawned by the daemon's plugin manager
// (spec/contracts/plugin.md R-PLUG.13), not typed by an operator. Because the
// built-ins share the nine binary, a plugin child would otherwise inherit the
// binary's entire startup, and three parts of that are actively wrong for it:
//
//   - **The operator config.** config.LoadDefault searches the cwd and $HOME,
//     both of which a plugin inherits, and the file it finds carries
//     [embeddings].api_key and every [plugin.<name>.settings] block — including
//     the settings of *other* plugins. Handing that to a plugin process would
//     undo internal/plugin.sanitizedHostEnv, which exists precisely to withhold
//     the daemon's secrets from plugins. So a plugin never loads config; the
//     manager passes it exactly the env it is entitled to.
//   - **The log file.** setupLogger appends to <binary-dir>/nine.log. With the
//     built-ins in the same binary that would be five processes interleaving in
//     one file. The manager already wires a plugin's stderr to the daemon's, so
//     stderr is the correct destination and the daemon's log stays one process's
//     account of itself.
//   - **The CLI.** Building it would give a plugin child a StartDaemon and a
//     StartTUI it has no business holding.
//
// This is confinement against mistakes, not against a hostile plugin: `shell`
// runs arbitrary commands by design, so no entry-point check can bound what a
// compromised handler does. What it does guarantee is that starting a plugin
// has no side effects beyond that plugin.
func servePluginAndExit(name string) {
	// Plugin logs belong on stderr, which the manager wires to the daemon's.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	// Fail closed outside the daemon. A built-in is only ever spawned by the
	// plugin manager, which allocates the socket first; with no socket there is
	// nothing to serve and no caller to answer. plugin.Serve enforces this too —
	// saying so here makes the reason legible to whoever ran it by hand.
	if os.Getenv("NINE_PLUGIN_SOCKET") == "" {
		fmt.Fprintln(os.Stderr, "nine plugin serve: NINE_PLUGIN_SOCKET is not set — this subcommand is spawned by the daemon, not run directly")
		os.Exit(1)
	}
	if name == "" {
		fmt.Fprintf(os.Stderr, "usage: nine plugin serve <%s>\n", strings.Join(builtins.Names(), "|"))
		os.Exit(1)
	}
	if err := builtins.Serve(name); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// setupLogger configures the global slog logger and reports whether it ended up
// writing to stderr — which callers that own the terminal (the TUI) must know,
// since for them stderr is the screen.
//
// Log destination (in priority order):
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
func setupLogger() (logsToStderr bool) {
	level := slog.LevelInfo
	if v := os.Getenv("NINE_LOG_LEVEL"); v != "" {
		var l slog.Level
		if err := l.UnmarshalText([]byte(v)); err == nil {
			level = l
		}
	}

	var w io.Writer = os.Stderr
	logsToStderr = true

	if os.Getenv("NINE_LOG_FILE") != "off" {
		if exePath, err := os.Executable(); err == nil {
			logPath := filepath.Join(filepath.Dir(exePath), "nine.log")
			if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600); err == nil {
				w = f // file stays open for process lifetime
				logsToStderr = false
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
	slog.Info("nine", "version", Version)
	return logsToStderr
}
