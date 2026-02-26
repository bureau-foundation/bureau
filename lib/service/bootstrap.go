// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/proxyclient"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/messaging"
)

// BootstrapResult holds the state produced by [BootstrapViaProxy]. The caller
// uses these fields to set up service-specific logic (initial sync,
// socket server, sync loop) and to populate the service struct.
type BootstrapResult struct {
	// Session is the authenticated Matrix session (ProxySession).
	// Closed by the cleanup function returned from BootstrapViaProxy.
	Session messaging.Session

	// Fleet is the typed fleet identity parsed from env vars.
	Fleet ref.Fleet

	// Machine is the typed machine identity.
	Machine ref.Machine

	// Service is the typed service identity.
	Service ref.Service

	// Namespace is the fleet's namespace, used for resolving
	// namespace-scoped room aliases (system, template, etc.).
	Namespace ref.Namespace

	// SystemRoomID is the namespace's system room, resolved during
	// bootstrap.
	SystemRoomID ref.RoomID

	// ServiceRoomID is the fleet's service directory room, resolved
	// during bootstrap.
	ServiceRoomID ref.RoomID

	// AuthConfig is the configured token verification for the
	// service's socket server. Uses the daemon's Ed25519 public
	// key fetched from the system room.
	AuthConfig *AuthConfig

	// Clock is the real clock used for token verification and
	// service timestamps.
	Clock clock.Clock

	// Logger is the structured logger for service output. Same
	// instance as ProxyBootstrapConfig.Logger if one was provided,
	// or the default created by [NewLogger].
	Logger *slog.Logger

	// SocketPath is the CBOR listener socket path, read from the
	// BUREAU_SERVICE_SOCKET environment variable.
	SocketPath string

	// PrincipalName is the service's localpart, read from the
	// BUREAU_PRINCIPAL_NAME environment variable.
	PrincipalName string

	// MachineName is the machine localpart, read from the
	// BUREAU_MACHINE_NAME environment variable.
	MachineName string

	// ServerName is the Matrix server name, read from the
	// BUREAU_SERVER_NAME environment variable, validated and typed.
	ServerName ref.ServerName

	// Telemetry is the telemetry emitter for this service. Nil when
	// the telemetry relay is not available (no RequiredServices:
	// ["telemetry"] in the template, or token not readable).
	// When non-nil, the flush goroutine is already running and will
	// be stopped by the cleanup function.
	Telemetry *TelemetryEmitter
}

// NewSocketServer creates a SocketServer from the bootstrap result,
// pre-configured with the service's socket path, logger, and auth
// config. If a telemetry emitter is available, it is automatically
// attached. This replaces the manual NewSocketServer(boot.SocketPath,
// boot.Logger, boot.AuthConfig) + SetTelemetry pattern.
func (b *BootstrapResult) NewSocketServer() *SocketServer {
	server := NewSocketServer(b.SocketPath, b.Logger, b.AuthConfig)
	if b.Telemetry != nil {
		server.SetTelemetry(b.Telemetry)
	}
	return server
}

// ProxyBootstrapConfig controls the [BootstrapViaProxy] process.
// Identity and topology are discovered from the environment
// (BUREAU_PROXY_SOCKET, BUREAU_MACHINE_NAME, BUREAU_SERVER_NAME,
// BUREAU_PRINCIPAL_NAME, BUREAU_FLEET, BUREAU_SERVICE_SOCKET);
// the config struct carries only service-specific parameters.
type ProxyBootstrapConfig struct {
	// Audience is the token verification scope (e.g., "ticket").
	Audience string

	// Description is the human-readable service description for
	// the fleet service directory entry.
	Description string

	// Capabilities lists feature tags for the service directory
	// entry (e.g., ["dependency-graph", "gate-evaluation"]).
	Capabilities []string

	// Logger is the structured logger for bootstrap output. If nil,
	// BootstrapViaProxy creates a default JSON handler on stderr.
	Logger *slog.Logger
}

// BootstrapViaProxy performs the common startup sequence for Bureau
// service binaries running inside a sandbox. It creates a
// [proxyclient.ProxySession] that delegates all Matrix operations to
// the per-principal proxy.
//
// Environment variables (set by the launcher via template variable
// expansion):
//
//   - BUREAU_PROXY_SOCKET — Unix socket path to the proxy
//   - BUREAU_MACHINE_NAME — fleet-scoped machine localpart
//   - BUREAU_SERVER_NAME — Matrix server name
//   - BUREAU_PRINCIPAL_NAME — service principal localpart
//   - BUREAU_FLEET — fleet prefix
//   - BUREAU_SERVICE_SOCKET — path for the CBOR listener socket
//
// The startup sequence:
//
//  1. Read and validate environment variables
//  2. Construct typed identity refs (fleet, machine, service)
//  3. Create a proxy client and ProxySession
//  4. Resolve the fleet's service and system rooms
//  5. Load the daemon's token signing public key
//  6. Configure token-based socket authentication
//  7. Register the service in the fleet directory
//
// Returns a [BootstrapResult] and a cleanup function. The cleanup
// function deregisters the service from the fleet directory.
func BootstrapViaProxy(ctx context.Context, config ProxyBootstrapConfig) (*BootstrapResult, func(), error) {
	logger := config.Logger
	if logger == nil {
		logger = NewLogger()
	}

	// Read and validate required environment variables.
	proxySocket := os.Getenv("BUREAU_PROXY_SOCKET")
	if proxySocket == "" {
		return nil, nil, fmt.Errorf("BUREAU_PROXY_SOCKET is required")
	}
	machineName := os.Getenv("BUREAU_MACHINE_NAME")
	if machineName == "" {
		return nil, nil, fmt.Errorf("BUREAU_MACHINE_NAME is required")
	}
	serverNameStr := os.Getenv("BUREAU_SERVER_NAME")
	if serverNameStr == "" {
		return nil, nil, fmt.Errorf("BUREAU_SERVER_NAME is required")
	}
	principalName := os.Getenv("BUREAU_PRINCIPAL_NAME")
	if principalName == "" {
		return nil, nil, fmt.Errorf("BUREAU_PRINCIPAL_NAME is required")
	}
	fleetPrefix := os.Getenv("BUREAU_FLEET")
	if fleetPrefix == "" {
		return nil, nil, fmt.Errorf("BUREAU_FLEET is required")
	}
	serviceSocket := os.Getenv("BUREAU_SERVICE_SOCKET")
	if serviceSocket == "" {
		return nil, nil, fmt.Errorf("BUREAU_SERVICE_SOCKET is required")
	}

	// Parse typed refs from the environment strings.
	serverName, err := ref.ParseServerName(serverNameStr)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid BUREAU_SERVER_NAME %q: %w", serverNameStr, err)
	}
	fleet, err := ref.ParseFleet(fleetPrefix, serverName)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid BUREAU_FLEET %q: %w", fleetPrefix, err)
	}
	_, bareMachineName, err := ref.ExtractEntityName(machineName)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid BUREAU_MACHINE_NAME %q: %w", machineName, err)
	}
	machineRef, err := ref.NewMachine(fleet, bareMachineName)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid machine ref: %w", err)
	}
	_, bareServiceName, err := ref.ExtractEntityName(principalName)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid BUREAU_PRINCIPAL_NAME %q: %w", principalName, err)
	}
	serviceRef, err := ref.NewService(fleet, bareServiceName)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid service ref: %w", err)
	}

	// Create the proxy client and session.
	proxy := proxyclient.New(proxySocket, serverName)
	userID, err := proxy.Whoami(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("proxy whoami: %w", err)
	}
	session := proxyclient.NewProxySession(proxy, userID)
	logger.Info("proxy session established", "user_id", userID, "proxy_socket", proxySocket)

	// Resolve fleet-scoped rooms by alias. Room membership is already
	// handled by the proxy's acceptPendingInvites — the daemon invited
	// this service to both rooms before creating the sandbox
	// (reconcile.go ensurePrincipalRoomAccess). We only need room IDs
	// here, not JoinRoom. The proxy's ResolveAlias endpoint is ungated
	// (no grant required), whereas JoinRoom requires a matrix/join
	// grant that services don't have.
	serviceRoomAlias := fleet.ServiceRoomAlias()
	serviceRoomID, err := session.ResolveAlias(ctx, serviceRoomAlias)
	if err != nil {
		return nil, nil, fmt.Errorf("resolving fleet service room %q: %w", serviceRoomAlias, err)
	}

	namespace := fleet.Namespace()
	systemRoomAlias := namespace.SystemRoomAlias()
	systemRoomID, err := session.ResolveAlias(ctx, systemRoomAlias)
	if err != nil {
		return nil, nil, fmt.Errorf("resolving system room %q: %w", systemRoomAlias, err)
	}
	logger.Info("global rooms ready",
		"service_room", serviceRoomID,
		"system_room", systemRoomID,
	)

	// Load the daemon's token signing public key.
	signingKey, err := LoadTokenSigningKey(ctx, session, systemRoomID, machineRef)
	if err != nil {
		return nil, nil, fmt.Errorf("loading token signing key: %w", err)
	}
	logger.Info("token signing key loaded", "machine", machineRef.Localpart())

	clk := clock.Real()

	authConfig := &AuthConfig{
		PublicKey: signingKey,
		Audience:  config.Audience,
		Blacklist: servicetoken.NewBlacklist(),
		Clock:     clk,
	}

	// Register in the fleet service room.
	if err := Register(ctx, session, serviceRoomID, serviceRef, Registration{
		Machine:      machineRef,
		Protocol:     "cbor",
		Description:  config.Description,
		Capabilities: config.Capabilities,
	}); err != nil {
		return nil, nil, fmt.Errorf("registering service: %w", err)
	}
	logger.Info("service registered",
		"principal", serviceRef.Localpart(),
		"machine", machineRef.UserID(),
	)

	// Probe for telemetry relay. Services that declare
	// RequiredServices: ["telemetry"] get the relay socket
	// bind-mounted into their sandbox. If present, create an
	// emitter and start the flush goroutine.
	var telemetryEmitter *TelemetryEmitter
	var telemetryCancel context.CancelFunc
	if _, statErr := os.Stat(TelemetryRelaySocketPath); statErr == nil {
		emitter, emitterErr := NewTelemetryEmitter(TelemetryConfig{
			SocketPath: TelemetryRelaySocketPath,
			TokenPath:  TelemetryRelayTokenPath,
			Fleet:      fleet,
			Machine:    machineRef,
			Source:     serviceRef.Entity(),
			Clock:      clk,
			Logger:     logger,
		})
		if emitterErr != nil {
			logger.Warn("telemetry relay socket present but emitter creation failed",
				"error", emitterErr,
			)
		} else {
			var telemetryContext context.Context
			telemetryContext, telemetryCancel = context.WithCancel(context.Background())
			go emitter.Run(telemetryContext, 5*time.Second)
			telemetryEmitter = emitter
			logger.Info("telemetry emitter started",
				"relay_socket", TelemetryRelaySocketPath,
			)
		}
	}

	cleanup := func() {
		// Stop the telemetry flush goroutine first. The drain
		// flush captures any spans from handlers that completed
		// before the socket server shut down.
		if telemetryCancel != nil {
			telemetryCancel()
			<-telemetryEmitter.Done()
		}

		deregisterContext, deregisterCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer deregisterCancel()
		if err := Deregister(deregisterContext, session, serviceRoomID, serviceRef); err != nil {
			logger.Error("failed to deregister service", "error", err)
		} else {
			logger.Info("service deregistered")
		}
		// ProxySession.Close is a no-op (the proxy manages credentials),
		// but call it for interface consistency.
		session.Close()
	}

	return &BootstrapResult{
		Session:       session,
		Fleet:         fleet,
		Machine:       machineRef,
		Service:       serviceRef,
		Namespace:     namespace,
		SystemRoomID:  systemRoomID,
		ServiceRoomID: serviceRoomID,
		AuthConfig:    authConfig,
		Clock:         clk,
		Logger:        logger,
		SocketPath:    serviceSocket,
		PrincipalName: principalName,
		MachineName:   machineName,
		ServerName:    serverName,
		Telemetry:     telemetryEmitter,
	}, cleanup, nil
}

// NewLogger creates the standard Bureau service logger: a JSON
// handler writing to stderr at Info level. It also sets the default
// slog logger so that third-party code using slog.Info etc. gets
// the same handler.
func NewLogger() *slog.Logger {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)
	return logger
}
