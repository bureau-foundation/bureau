// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bureau-foundation/bureau/lib/secret"
	"github.com/bureau-foundation/bureau/lib/version"
	"github.com/bureau-foundation/bureau/messaging"
	"github.com/bureau-foundation/bureau/proxy"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var configPath string
	var credentialFile string
	var credentialPrefix string
	var credentialStdin bool
	var showVersion bool

	flag.StringVar(&configPath, "config", "", "path to config file (required)")
	flag.StringVar(&credentialFile, "credential-file", "", "path to credentials file (key=value format, more secure than env vars)")
	flag.StringVar(&credentialPrefix, "credential-prefix", "BUREAU_", "prefix for environment variable credentials (dev mode)")
	flag.BoolVar(&credentialStdin, "credential-stdin", false, "read JSON credential payload from stdin (production: launcher pipes credentials)")
	flag.BoolVar(&showVersion, "version", false, "print version information and exit")
	flag.Parse()

	if showVersion {
		fmt.Printf("bureau-proxy %s\n", version.Info())
		return nil
	}

	if configPath == "" {
		return fmt.Errorf("-config is required")
	}

	// Set up structured logging
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	// Load configuration
	config, err := proxy.LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if err := config.Validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	logger.Info("starting bureau-proxy",
		"version", version.Info(),
	)
	logger.Info("loaded configuration",
		"socket_path", config.SocketPath,
		"services", len(config.Services),
	)

	// Set up credential sources in priority order:
	// 1. Stdin pipe (production: launcher delivers credentials via stdin)
	// 2. systemd credentials (production alternative)
	// 3. File-based credentials (secure dev - file not visible in /proc)
	// 4. Environment variables (fallback - WARNING: visible in /proc/*/environ)
	sources := []proxy.CredentialSource{}
	var pipeSource *proxy.PipeCredentialSource
	if credentialStdin {
		var err error
		pipeSource, err = proxy.ReadPipeCredentials(os.Stdin)
		if err != nil {
			return fmt.Errorf("failed to read credentials from stdin: %w", err)
		}
		sources = append(sources, pipeSource)
		if userIDBuffer := pipeSource.Get("MATRIX_USER_ID"); userIDBuffer != nil {
			logger.Info("loaded credentials from stdin",
				"matrix_user_id", userIDBuffer.String(),
			)
		}
	}
	sources = append(sources, &proxy.SystemdCredentialSource{})
	if credentialFile != "" {
		sources = append(sources, &proxy.FileCredentialSource{Path: credentialFile})
		logger.Info("using credential file", "path", credentialFile)
	}
	sources = append(sources, &proxy.EnvCredentialSource{Prefix: credentialPrefix})
	credentialSource := &proxy.ChainCredentialSource{Sources: sources}
	defer credentialSource.Close()

	// Create server
	server, err := proxy.NewServer(proxy.ServerConfig{
		SocketPath:      config.SocketPath,
		AdminSocketPath: config.AdminSocketPath,
		ListenAddress:   config.ListenAddress,
		Logger:          logger,
	})
	if err != nil {
		return fmt.Errorf("failed to create server: %w", err)
	}

	// Register services from config.
	for name, serviceConfig := range config.Services {
		if name == "matrix" {
			return fmt.Errorf("service name %q is reserved for the built-in Matrix proxy", name)
		}

		switch serviceConfig.Type {
		case "cli":
			service, err := createCLIService(name, serviceConfig, credentialSource)
			if err != nil {
				return fmt.Errorf("failed to create CLI service %q: %w", name, err)
			}
			server.RegisterService(name, service)
			logger.Info("registered CLI service",
				"name", name,
				"binary", serviceConfig.Binary,
			)

		case "http":
			service, err := createHTTPService(name, serviceConfig, credentialSource, logger)
			if err != nil {
				return fmt.Errorf("failed to create HTTP service %q: %w", name, err)
			}
			server.RegisterHTTPService(name, service)
			logger.Info("registered HTTP service",
				"name", name,
				"upstream", serviceConfig.Upstream,
			)

		default:
			return fmt.Errorf("service %q: unknown type %q", name, serviceConfig.Type)
		}
	}

	// Register built-in Matrix proxy service. Every Bureau proxy forwards
	// Matrix client-server API calls to the homeserver with token injection.
	// The homeserver URL and token come from PipeCredentialSource (production)
	// or FileCredentialSource/EnvCredentialSource (dev). Agents inside sandboxes
	// reach Matrix via the structured /v1/matrix/* endpoints.
	if matrixService, err := createMatrixService(credentialSource, logger); err != nil {
		logger.Warn("matrix proxy service not registered", "reason", err)
	} else {
		server.RegisterHTTPService("matrix", matrixService)
		logAttrs := []any{"upstream", bufferStringOrEmpty(credentialSource.Get("MATRIX_HOMESERVER_URL"))}
		if userIDBuffer := credentialSource.Get("MATRIX_USER_ID"); userIDBuffer != nil {
			logAttrs = append(logAttrs, "matrix_user_id", userIDBuffer.String())
		}
		logger.Info("registered built-in Matrix proxy service", logAttrs...)
	}

	// Configure observation proxy if an observe socket path is set.
	// This enables sandboxed agents to observe other principals through
	// the proxy, which injects its own Matrix credentials so the agent
	// never touches authentication material.
	if config.ObserveSocketPath != "" {
		daemonSocket := config.DaemonObserveSocket
		if daemonSocket == "" {
			daemonSocket = "/run/bureau/observe.sock"
		}
		server.SetObserveConfig(config.ObserveSocketPath, daemonSocket, credentialSource)
	}

	// Set agent identity from credentials so the GET /v1/identity endpoint
	// can return the agent's Matrix user ID without a homeserver round-trip.
	// Includes the observe socket path when configured.
	if matrixUserIDBuffer := credentialSource.Get("MATRIX_USER_ID"); matrixUserIDBuffer != nil {
		matrixUserID := matrixUserIDBuffer.String()
		identity := proxy.IdentityInfo{
			UserID:        matrixUserID,
			ObserveSocket: config.ObserveSocketPath,
		}
		// Extract server name from the Matrix user ID: @localpart:server → server.
		// Use first colon — Matrix localparts cannot contain colons, but server
		// names can include a port (e.g., bureau.local:8448).
		if colonIndex := strings.Index(matrixUserID, ":"); colonIndex >= 0 {
			identity.ServerName = matrixUserID[colonIndex+1:]
		}
		server.SetIdentity(identity)
	}

	// Set pre-resolved authorization grants from the credential payload.
	// Grants control which Matrix operations (join, invite, room creation)
	// and service discovery the proxy allows. Only PipeCredentialSource
	// carries grants — other credential sources use default-deny.
	if credentialStdin && pipeSource != nil {
		server.SetGrants(pipeSource.Grants())
	}

	// Accept any pending room invites before starting the server. The
	// daemon invites principals to workspace rooms during reconciliation
	// (before create-sandbox), so by the time the proxy starts, the invite
	// is already pending. This one-time sync-and-join makes the principal
	// a full room member, enabling it to publish state events via the proxy.
	acceptPendingInvites(credentialSource, logger)

	// Start server
	if err := server.Start(); err != nil {
		return fmt.Errorf("failed to start server: %w", err)
	}

	// Wait for shutdown signal
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	<-ctx.Done()
	logger.Info("received shutdown signal")

	// Graceful shutdown with timeout
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown error: %w", err)
	}

	logger.Info("shutdown complete")
	return nil
}

// createCLIService creates a CLIService from configuration.
func createCLIService(name string, config proxy.ServiceConfig, credentials proxy.CredentialSource) (*proxy.CLIService, error) {
	var filter proxy.Filter
	if len(config.Allowed) > 0 || len(config.Blocked) > 0 {
		filter = &proxy.GlobFilter{
			Allowed: config.Allowed,
			Blocked: config.Blocked,
		}
	}

	return proxy.NewCLIService(proxy.CLIServiceConfig{
		Name:       name,
		Binary:     config.Binary,
		EnvVars:    config.EnvVars,
		Filter:     filter,
		Credential: credentials,
	})
}

// createMatrixService creates the built-in Matrix proxy service from credentials.
// Returns an error if the required Matrix credentials (homeserver URL and token)
// are not available — callers should treat this as "Matrix not configured" rather
// than a fatal error, since the proxy can still serve other services.
//
// In production (PipeCredentialSource), MATRIX_BEARER is derived automatically
// from matrix_token. In dev mode (FileCredentialSource, EnvCredentialSource),
// if MATRIX_BEARER is not set but MATRIX_TOKEN is, this function wraps the
// credential source to synthesize the Bearer value.
func createMatrixService(credentials proxy.CredentialSource, logger *slog.Logger) (*proxy.HTTPService, error) {
	homeserverURLBuffer := credentials.Get("MATRIX_HOMESERVER_URL")
	if homeserverURLBuffer == nil {
		return nil, fmt.Errorf("MATRIX_HOMESERVER_URL not available in credentials")
	}

	// Check for MATRIX_BEARER (available from PipeCredentialSource). If
	// absent, try to synthesize from MATRIX_TOKEN (file/env credential
	// sources store the raw token without Bearer prefix).
	effectiveCredentials := credentials
	if credentials.Get("MATRIX_BEARER") == nil {
		matrixTokenBuffer := credentials.Get("MATRIX_TOKEN")
		if matrixTokenBuffer == nil {
			return nil, fmt.Errorf("neither MATRIX_BEARER nor MATRIX_TOKEN available in credentials")
		}
		bearerSource, err := proxy.NewMapCredentialSource(map[string]string{
			"MATRIX_BEARER": "Bearer " + matrixTokenBuffer.String(),
		})
		if err != nil {
			return nil, fmt.Errorf("creating bearer credential: %w", err)
		}
		effectiveCredentials = &proxy.ChainCredentialSource{
			Sources: []proxy.CredentialSource{
				bearerSource,
				credentials,
			},
		}
	}

	return proxy.NewHTTPService(proxy.HTTPServiceConfig{
		Name:     "matrix",
		Upstream: homeserverURLBuffer.String(),
		InjectHeaders: map[string]string{
			"Authorization": "MATRIX_BEARER",
		},
		StripHeaders: []string{
			// Agents must not be able to override the injected token.
			"Authorization",
		},
		Filter:     matrixAPIFilter(),
		Credential: effectiveCredentials,
		Logger:     logger,
	})
}

// matrixAPIFilter returns a GlobFilter that restricts the raw Matrix HTTP
// passthrough (/http/matrix/) to a safe subset of client-server API
// endpoints. This is defense-in-depth: even with a matrix/raw-api grant,
// third-party Matrix client libraries cannot browse the room directory,
// enumerate users, or perform administrative operations.
//
// Bureau's own code uses the structured /v1/matrix/* endpoints which bypass
// this filter entirely. The raw passthrough exists for third-party Matrix
// client libraries running inside sandboxes that need direct homeserver access.
//
// The filter uses an allowlist — any endpoint not explicitly listed is blocked.
// The glob `*` matches any characters including `/`, so patterns like
// `"* /_matrix/client/v3/rooms/*/send/*"` match all methods, room IDs, event
// types, and transaction IDs.
//
// Room-level access control (restricting which specific rooms an agent can
// reach) requires room assignment data from the launcher's credential payload.
// That is tracked separately from this endpoint-level filtering.
func matrixAPIFilter() *proxy.GlobFilter {
	return &proxy.GlobFilter{
		Allowed: []string{
			// Send events (messages, state, etc.) to rooms.
			"PUT /_matrix/client/v3/rooms/*/send/*",

			// Read room messages (paginated history).
			"GET /_matrix/client/v3/rooms/*/messages*",

			// Read and write state events (service registration, config).
			"GET /_matrix/client/v3/rooms/*/state*",
			"PUT /_matrix/client/v3/rooms/*/state/*",

			// Read thread messages via relations API.
			"GET /_matrix/client/v3/rooms/*/relations/*",

			// Sync (long-poll for new events).
			"GET /_matrix/client/v3/sync*",

			// Identity — agent can discover its own Matrix user ID.
			"GET /_matrix/client/v3/account/whoami",

			// Resolve room aliases to room IDs. Room membership still
			// gates all subsequent operations on the resolved room ID.
			"GET /_matrix/client/v3/directory/room/*",

			// Join rooms the agent has been invited to.
			"POST /_matrix/client/v3/join/*",

			// Room membership (needed for joined_rooms check).
			"GET /_matrix/client/v3/joined_rooms",
		},
	}
}

// createHTTPService creates an HTTPService from configuration.
func createHTTPService(name string, config proxy.ServiceConfig, credentials proxy.CredentialSource, logger *slog.Logger) (*proxy.HTTPService, error) {
	var filter proxy.Filter
	if len(config.Allowed) > 0 || len(config.Blocked) > 0 {
		filter = &proxy.GlobFilter{
			Allowed: config.Allowed,
			Blocked: config.Blocked,
		}
	}

	return proxy.NewHTTPService(proxy.HTTPServiceConfig{
		Name:          name,
		Upstream:      config.Upstream,
		InjectHeaders: config.InjectHeaders,
		StripHeaders:  config.StripHeaders,
		Filter:        filter,
		Credential:    credentials,
		Logger:        logger,
	})
}

// acceptPendingInvites performs a one-time initial sync to discover rooms
// the principal has been invited to, and joins each one. This bridges the
// gap between the daemon's invite (ensurePrincipalRoomAccess) and the
// sandboxed agent's inability to call JoinRoom (authorization grants
// default-deny matrix/join). The daemon invites before create-sandbox,
// so the invite is already pending when the proxy starts.
//
// Best-effort: failures are logged but do not block proxy startup. The
// proxy can still serve other services, and the agent can still publish
// state events through the structured proxy endpoints.
func acceptPendingInvites(credentials proxy.CredentialSource, logger *slog.Logger) {
	homeserverURLBuffer := credentials.Get("MATRIX_HOMESERVER_URL")
	tokenBuffer := credentials.Get("MATRIX_TOKEN")
	userIDBuffer := credentials.Get("MATRIX_USER_ID")
	if homeserverURLBuffer == nil || tokenBuffer == nil || userIDBuffer == nil {
		return // Matrix credentials not available — nothing to join.
	}

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: homeserverURLBuffer.String(),
	})
	if err != nil {
		logger.Warn("failed to create messaging client for invite acceptance", "error", err)
		return
	}

	session, err := client.SessionFromToken(userIDBuffer.String(), tokenBuffer.String())
	if err != nil {
		logger.Warn("failed to create session for invite acceptance", "error", err)
		return
	}
	defer session.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Non-blocking initial sync: timeout=0 returns immediately with
	// current state, including any pending invites.
	response, err := session.Sync(ctx, messaging.SyncOptions{
		Timeout:    0,
		SetTimeout: true,
	})
	if err != nil {
		logger.Warn("initial sync for invite acceptance failed", "error", err)
		return
	}

	if len(response.Rooms.Invite) == 0 {
		return
	}

	for roomID := range response.Rooms.Invite {
		if _, err := session.JoinRoom(ctx, roomID); err != nil {
			logger.Warn("failed to accept room invite",
				"room_id", roomID,
				"error", err,
			)
			continue
		}
		logger.Info("accepted room invite", "room_id", roomID)
	}
}

// bufferStringOrEmpty returns the string value of a secret buffer, or an
// empty string if the buffer is nil. Used for logging non-secret values
// like homeserver URLs where nil means "not configured".
func bufferStringOrEmpty(buffer *secret.Buffer) string {
	if buffer == nil {
		return ""
	}
	return buffer.String()
}
