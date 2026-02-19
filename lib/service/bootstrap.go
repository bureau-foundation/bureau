// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/messaging"
)

// CommonFlags holds the flag values shared by all Bureau service
// binaries. Call [RegisterCommonFlags] to bind these to the default
// flag set before calling flag.Parse.
type CommonFlags struct {
	HomeserverURL string
	MachineName   string
	PrincipalName string
	ServerName    string
	FleetPrefix   string
	RunDir        string
	StateDir      string
	ShowVersion   bool
}

// RegisterCommonFlags binds [CommonFlags] fields to the default flag
// set with standard names, defaults, and help text. Service binaries
// call this before flag.Parse, then register any service-specific
// flags before parsing.
func RegisterCommonFlags(flags *CommonFlags) {
	flag.StringVar(&flags.HomeserverURL, "homeserver", "http://localhost:6167", "Matrix homeserver URL")
	flag.StringVar(&flags.MachineName, "machine-name", "", "machine localpart (required)")
	flag.StringVar(&flags.PrincipalName, "principal-name", "", "service principal localpart (required)")
	flag.StringVar(&flags.ServerName, "server-name", "bureau.local", "Matrix server name")
	flag.StringVar(&flags.FleetPrefix, "fleet", "", "fleet prefix (required)")
	flag.StringVar(&flags.RunDir, "run-dir", principal.DefaultRunDir, "runtime directory for sockets")
	flag.StringVar(&flags.StateDir, "state-dir", "", "directory containing session.json (required)")
	flag.BoolVar(&flags.ShowVersion, "version", false, "print version information and exit")
}

// BootstrapConfig controls the [Bootstrap] process.
type BootstrapConfig struct {
	// Flags are the parsed common flag values. Call
	// [RegisterCommonFlags] and flag.Parse before Bootstrap.
	Flags CommonFlags

	// Audience is the token verification scope for this service
	// (e.g., "ticket", "artifact", "fleet", "agent"). Tokens
	// scoped to a different audience are rejected.
	Audience string

	// Description is the human-readable service description for
	// the fleet service directory entry.
	Description string

	// Capabilities lists feature tags for the service directory
	// entry (e.g., ["dependency-graph", "gate-evaluation"]).
	Capabilities []string

	// Logger is the structured logger for Bootstrap output. If nil,
	// Bootstrap creates a default JSON handler on stderr via
	// [NewLogger]. Pass an explicit logger when the service needs
	// to log before calling Bootstrap (e.g., reading secrets from
	// stdin).
	Logger *slog.Logger
}

// BootstrapResult holds the state produced by [Bootstrap]. The caller
// uses these fields to set up service-specific logic (initial sync,
// socket server, sync loop) and to populate the service struct.
type BootstrapResult struct {
	// Session is the authenticated Matrix session. Closed by the
	// cleanup function returned from Bootstrap.
	Session *messaging.DirectSession

	// Fleet is the typed fleet identity parsed from the flag values.
	Fleet ref.Fleet

	// Machine is the typed machine identity.
	Machine ref.Machine

	// Service is the typed service identity.
	Service ref.Service

	// Namespace is the fleet's namespace, used for resolving
	// namespace-scoped room aliases (system, template, etc.).
	Namespace ref.Namespace

	// SystemRoomID is the namespace's system room, joined during
	// bootstrap.
	SystemRoomID ref.RoomID

	// ServiceRoomID is the fleet's service directory room, joined
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
	// instance as BootstrapConfig.Logger if one was provided, or
	// the default created by [NewLogger].
	Logger *slog.Logger

	// SocketPath is the computed socket path for this service,
	// derived from RunDir and PrincipalName.
	SocketPath string

	// PrincipalName is the service's localpart as passed via
	// --principal-name.
	PrincipalName string

	// MachineName is the machine localpart as passed via
	// --machine-name.
	MachineName string

	// ServerName is the Matrix server name as passed via
	// --server-name.
	ServerName string

	// RunDir is the validated runtime directory for sockets.
	RunDir string
}

// Bootstrap performs the common startup sequence for Bureau service
// binaries:
//
//  1. Validate common flag values (required fields, localpart format)
//  2. Construct typed identity refs (fleet, machine, service)
//  3. Validate the runtime socket directory
//  4. Load and validate the Matrix session
//  5. Resolve and join the fleet's service and system rooms
//  6. Load the daemon's token signing public key from the system room
//  7. Configure token-based socket authentication
//  8. Register the service in the fleet directory
//
// Returns a [BootstrapResult] and a cleanup function. The cleanup
// function deregisters the service from the fleet directory and
// closes the Matrix session. The caller must defer the cleanup
// function.
//
// Bootstrap does not create a signal context — callers create their
// own before calling Bootstrap. This allows services that need
// pre-Bootstrap work (reading secrets from stdin, etc.) to control
// context lifecycle.
func Bootstrap(ctx context.Context, config BootstrapConfig) (*BootstrapResult, func(), error) {
	flags := config.Flags
	logger := config.Logger
	if logger == nil {
		logger = NewLogger()
	}

	// Validate required flags.
	if flags.MachineName == "" {
		return nil, nil, fmt.Errorf("--machine-name is required")
	}
	if err := principal.ValidateLocalpart(flags.MachineName); err != nil {
		return nil, nil, fmt.Errorf("invalid machine name: %w", err)
	}
	if flags.PrincipalName == "" {
		return nil, nil, fmt.Errorf("--principal-name is required")
	}
	if err := principal.ValidateLocalpart(flags.PrincipalName); err != nil {
		return nil, nil, fmt.Errorf("invalid principal name: %w", err)
	}
	if flags.StateDir == "" {
		return nil, nil, fmt.Errorf("--state-dir is required")
	}
	if flags.FleetPrefix == "" {
		return nil, nil, fmt.Errorf("--fleet is required")
	}

	// Construct typed identity refs from the string flags.
	fleet, err := ref.ParseFleet(flags.FleetPrefix, flags.ServerName)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid fleet: %w", err)
	}
	_, bareMachineName, err := ref.ExtractEntityName(flags.MachineName)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid machine name: %w", err)
	}
	machineRef, err := ref.NewMachine(fleet, bareMachineName)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid machine ref: %w", err)
	}
	_, bareServiceName, err := ref.ExtractEntityName(flags.PrincipalName)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid principal name: %w", err)
	}
	serviceRef, err := ref.NewService(fleet, bareServiceName)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid service ref: %w", err)
	}

	if err := principal.ValidateRunDir(flags.RunDir); err != nil {
		return nil, nil, fmt.Errorf("run directory validation: %w", err)
	}

	// Load and validate the Matrix session.
	_, session, err := LoadSession(flags.StateDir, flags.HomeserverURL, logger)
	if err != nil {
		return nil, nil, fmt.Errorf("loading session: %w", err)
	}

	// From this point, the session is open and must be closed on
	// any error path. Track success so the deferred close only
	// fires on failure — on success, the caller owns cleanup.
	success := false
	defer func() {
		if !success {
			session.Close()
		}
	}()

	userID, err := ValidateSession(ctx, session)
	if err != nil {
		return nil, nil, err
	}
	logger.Info("matrix session valid", "user_id", userID)

	// Resolve and join fleet-scoped rooms the service needs.
	serviceRoomID, err := ResolveFleetServiceRoom(ctx, session, fleet)
	if err != nil {
		return nil, nil, fmt.Errorf("resolving fleet service room: %w", err)
	}

	namespace := fleet.Namespace()
	systemRoomID, err := ResolveSystemRoom(ctx, session, namespace)
	if err != nil {
		return nil, nil, fmt.Errorf("resolving system room: %w", err)
	}
	logger.Info("global rooms ready",
		"service_room", serviceRoomID,
		"system_room", systemRoomID,
	)

	// Load the daemon's token signing public key for authenticating
	// incoming service requests.
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

	socketPath := principal.RunDirSocketPath(flags.RunDir, flags.PrincipalName)

	// Register in the fleet service room so daemons can discover us.
	if err := Register(ctx, session, serviceRoomID, serviceRef, Registration{
		Machine:      machineRef.UserID(),
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

	cleanup := func() {
		deregisterContext, deregisterCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer deregisterCancel()
		if err := Deregister(deregisterContext, session, serviceRoomID, serviceRef); err != nil {
			logger.Error("failed to deregister service", "error", err)
		} else {
			logger.Info("service deregistered")
		}
		session.Close()
	}

	success = true
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
		SocketPath:    socketPath,
		PrincipalName: flags.PrincipalName,
		MachineName:   flags.MachineName,
		ServerName:    flags.ServerName,
		RunDir:        flags.RunDir,
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
