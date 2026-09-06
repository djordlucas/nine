package main

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"nine/internal/api"
	"nine/internal/config"
	"nine/internal/protocol"
)

// serveAPIAndExit runs the HTTP API server as a separate process.
// It connects to the daemon via Unix socket and provides HTTP/REST access to
// Nine's functionality with feature parity to the CLI.
//
// This is the entry point for `nine api serve` (spec/contracts/api.md).
func serveAPIAndExit() {
	// API process logs belong on stderr, which is the only output stream
	// available to the operator when running as a separate process.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	// Load configuration
	cfg := config.LoadDefault()

	// Parse API-specific command-line flags
	apiCfg := parseAPIFlags(os.Args[1:])

	// Merge with config file, with flags taking precedence
	mergedCfg := mergeAPIConfig(cfg.API, apiCfg)

	// Log configuration
	slog.Info("starting nine API server",
		"version", Version,
		"host", mergedCfg.Host(),
		"port", mergedCfg.Port(),
		"auth_enabled", mergedCfg.AuthToken != "",
		"tls_enabled", mergedCfg.TLSEnabled(),
		"rate_limit_enabled", mergedCfg.RateLimitEnabled())

	// Verify daemon is running and we can connect
	socketPath := cfg.SocketPath()
	if !protocol.CanConnect(socketPath) {
		// Try to start the daemon if it's not running
		if _, err := protocol.EnsureDaemon(socketPath, os.Args[0]); err != nil {
			slog.Error("daemon not running and could not be started", "err", err)
			os.Exit(1)
		}
	}

	// Create and run the API server
	startTime := time.Now()
	apiServer := api.NewServer(api.Config{
		APIConfig:  mergedCfg,
		SocketPath: socketPath,
		Version:   Version,
		StartTime: startTime,
	})

	if err := apiServer.Run(); err != nil {
		slog.Error("API server failed", "err", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// apiFlags holds command-line flags for the API server.
type apiFlags struct {
	port        int
	host        string
	authToken   string
	timeout     int
	maxConn     int
	corsOrigins []string
	tlsEnabled  bool
	tlsCert     string
	tlsKey      string
}

// parseAPIFlags parses API-specific command-line flags.
// Note: args[0] is intentionally skipped as it contains the command name ("serve").
// The actual flags start from args[1] onwards.
func parseAPIFlags(args []string) apiFlags {
	var flags apiFlags
	flags.port = config.DefaultAPIPort
	flags.host = config.DefaultAPIHost
	flags.timeout = config.DefaultAPITimeoutSeconds
	flags.maxConn = config.DefaultAPIMaxConnections

	// Start from index 1 to skip the command name ("serve")
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--port":
			if i+1 < len(args) {
				if p, err := strconv.Atoi(args[i+1]); err == nil {
					if p >= 1 && p <= 65535 {
						flags.port = p
					} else {
						slog.Warn("invalid port number, using default", "port", args[i+1], "default", config.DefaultAPIPort)
					}
				} else {
					slog.Warn("invalid port value, using default", "value", args[i+1], "default", config.DefaultAPIPort)
				}
				i++
			}
		case "--host":
			if i+1 < len(args) {
				host := args[i+1]
				// Basic validation: non-empty string
				if host != "" {
					flags.host = host
				} else {
					slog.Warn("empty host value, using default", "default", config.DefaultAPIHost)
				}
				i++
			}
		case "--auth-token":
			if i+1 < len(args) {
				flags.authToken = args[i+1]
				i++
			}
		case "--timeout":
			if i+1 < len(args) {
				if t, err := strconv.Atoi(args[i+1]); err == nil {
					if t > 0 {
						flags.timeout = t
					} else {
						slog.Warn("invalid timeout value, using default", "value", args[i+1], "default", config.DefaultAPITimeoutSeconds)
					}
				} else {
					slog.Warn("invalid timeout value, using default", "value", args[i+1], "default", config.DefaultAPITimeoutSeconds)
				}
				i++
			}
		case "--max-connections":
			if i+1 < len(args) {
				if m, err := strconv.Atoi(args[i+1]); err == nil {
					if m > 0 {
						flags.maxConn = m
					} else {
						slog.Warn("invalid max-connections value, using default", "value", args[i+1], "default", config.DefaultAPIMaxConnections)
					}
				} else {
					slog.Warn("invalid max-connections value, using default", "value", args[i+1], "default", config.DefaultAPIMaxConnections)
				}
				i++
			}
		case "--cors-origins":
			if i+1 < len(args) {
				flags.corsOrigins = strings.Split(args[i+1], ",")
				i++
			}
		case "--tls":
			flags.tlsEnabled = true
		case "--tls-cert":
			if i+1 < len(args) {
				flags.tlsCert = args[i+1]
				i++
			}
		case "--tls-key":
			if i+1 < len(args) {
				flags.tlsKey = args[i+1]
				i++
			}
		}
	}

	// Check environment variables
	if v := os.Getenv("NINE_API_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			if p >= 1 && p <= 65535 {
				flags.port = p
			} else {
				slog.Warn("invalid NINE_API_PORT value, using default", "value", v, "default", config.DefaultAPIPort)
			}
		} else {
			slog.Warn("invalid NINE_API_PORT value, using default", "value", v, "default", config.DefaultAPIPort)
		}
	}
	if v := os.Getenv("NINE_API_HOST"); v != "" {
		if v != "" {
			flags.host = v
		} else {
			slog.Warn("empty NINE_API_HOST value, using default", "default", config.DefaultAPIHost)
		}
	}
	if v := os.Getenv("NINE_API_AUTH_TOKEN"); v != "" {
		flags.authToken = v
	}
	if v := os.Getenv("NINE_API_TIMEOUT_SECONDS"); v != "" {
		if t, err := strconv.Atoi(v); err == nil {
			if t > 0 {
				flags.timeout = t
			} else {
				slog.Warn("invalid NINE_API_TIMEOUT_SECONDS value, using default", "value", v, "default", config.DefaultAPITimeoutSeconds)
			}
		} else {
			slog.Warn("invalid NINE_API_TIMEOUT_SECONDS value, using default", "value", v, "default", config.DefaultAPITimeoutSeconds)
		}
	}
	if v := os.Getenv("NINE_API_MAX_CONNECTIONS"); v != "" {
		if m, err := strconv.Atoi(v); err == nil {
			if m > 0 {
				flags.maxConn = m
			} else {
				slog.Warn("invalid NINE_API_MAX_CONNECTIONS value, using default", "value", v, "default", config.DefaultAPIMaxConnections)
			}
		} else {
			slog.Warn("invalid NINE_API_MAX_CONNECTIONS value, using default", "value", v, "default", config.DefaultAPIMaxConnections)
		}
	}
	if v := os.Getenv("NINE_API_CORS_ORIGINS"); v != "" {
		flags.corsOrigins = strings.Split(v, ",")
	}

	return flags
}

// mergeAPIConfig merges config file settings with command-line flags,
// with flags taking precedence.
func mergeAPIConfig(fileCfg config.APIConfig, flags apiFlags) config.APIConfig {
	merged := fileCfg

	// Override from flags
	if flags.port != 0 {
		merged.Port = flags.port
	}
	if flags.host != "" {
		merged.Host = flags.host
	}
	if flags.authToken != "" {
		merged.AuthToken = flags.authToken
	}
	if flags.timeout != 0 {
		merged.TimeoutSeconds = flags.timeout
	}
	if flags.maxConn != 0 {
		merged.MaxConnections = flags.maxConn
	}
	if len(flags.corsOrigins) > 0 {
		merged.CORSOrigins = flags.corsOrigins
	}
	if flags.tlsEnabled {
		merged.TLS.Enabled = true
		if flags.tlsCert != "" {
			merged.TLS.CertPath = flags.tlsCert
		}
		if flags.tlsKey != "" {
			merged.TLS.KeyPath = flags.tlsKey
		}
	}

	return merged
}
