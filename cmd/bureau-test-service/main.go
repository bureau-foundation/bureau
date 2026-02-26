// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Bureau-test-service exercises the full Bureau service lifecycle for
// integration testing. It calls BootstrapViaProxy, creates a CBOR socket
// server with token authentication, registers in the fleet service
// directory, and handles clean SIGTERM shutdown.
//
// The binary exposes two actions:
//   - status (unauthenticated): uptime and principal name
//   - info (authenticated): full bootstrap identity and token details
//
// This is the service-entity counterpart to bureau-test-agent. Where the
// test agent verifies proxy and Matrix messaging, the test service verifies
// BootstrapViaProxy, CBOR socket servers, token authentication, fleet
// service directory registration, and graceful shutdown.
package main

import (
	"context"
	"os/signal"
	"syscall"

	"github.com/bureau-foundation/bureau/lib/process"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

func main() {
	if err := run(); err != nil {
		process.Fatal(err)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	boot, cleanup, err := service.BootstrapViaProxy(ctx, service.ProxyBootstrapConfig{
		Audience:     "test",
		Description:  "Integration test service",
		Capabilities: []string{"test"},
	})
	if err != nil {
		return err
	}
	defer cleanup()

	startedAt := boot.Clock.Now()

	socketServer := service.NewSocketServer(boot.SocketPath, boot.Logger, boot.AuthConfig)
	socketServer.RegisterRevocationHandler()

	// Unauthenticated liveness check matching the pattern from
	// production services. Returns only non-sensitive operational data.
	socketServer.Handle("status", func(_ context.Context, _ []byte) (any, error) {
		return statusResponse{
			UptimeSeconds: boot.Clock.Now().Sub(startedAt).Seconds(),
			Principal:     boot.PrincipalName,
		}, nil
	})

	// Authenticated action that returns the full bootstrap identity.
	// Tests use this to verify that BootstrapViaProxy completed, the
	// daemon minted a valid token, and the socket server verified it.
	// The subject and token_machine fields come from the verified
	// servicetoken.Token, proving the end-to-end token auth chain.
	socketServer.HandleAuth("info", func(_ context.Context, token *servicetoken.Token, _ []byte) (any, error) {
		return infoResponse{
			Fleet:        boot.Fleet.String(),
			Machine:      boot.Machine.Localpart(),
			Service:      boot.Service.Localpart(),
			Audience:     boot.AuthConfig.Audience,
			SocketPath:   boot.SocketPath,
			ServerName:   boot.ServerName.String(),
			Subject:      token.Subject.String(),
			TokenMachine: token.Machine.String(),
		}, nil
	})

	socketDone := make(chan error, 1)
	go func() {
		socketDone <- socketServer.Serve(ctx)
	}()

	boot.Logger.Info("test service running",
		"principal", boot.PrincipalName,
		"socket", boot.SocketPath,
	)

	<-ctx.Done()
	boot.Logger.Info("shutting down")

	if err := <-socketDone; err != nil {
		boot.Logger.Error("socket server error", "error", err)
	}

	return nil
}

// statusResponse is the CBOR response for the unauthenticated "status"
// action. Contains only non-sensitive operational data.
type statusResponse struct {
	UptimeSeconds float64 `cbor:"uptime_seconds"`
	Principal     string  `cbor:"principal"`
}

// infoResponse is the CBOR response for the authenticated "info" action.
// Returns the full bootstrap identity including token verification details
// so tests can verify the complete service lifecycle.
type infoResponse struct {
	Fleet        string `cbor:"fleet"`
	Machine      string `cbor:"machine"`
	Service      string `cbor:"service"`
	Audience     string `cbor:"audience"`
	SocketPath   string `cbor:"socket_path"`
	ServerName   string `cbor:"server_name"`
	Subject      string `cbor:"subject"`
	TokenMachine string `cbor:"token_machine"`
}
