// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/bureau-foundation/bureau/lib/process"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/version"
)

func main() {
	if err := run(); err != nil {
		process.Fatal(err)
	}
}

func run() error {
	var showVersion bool
	flag.BoolVar(&showVersion, "version", false, "print version information and exit")
	flag.Parse()

	if showVersion {
		version.Print("bureau-github-service")
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Read the webhook HMAC secret before bootstrap. The secret file
	// is provided by the launcher's credential bundle and must contain
	// the raw secret (no trailing newline expected, but trimmed for
	// robustness).
	webhookSecret, err := readWebhookSecret()
	if err != nil {
		return err
	}

	// Resolve the webhook listen address. Defaults to localhost:9876 —
	// external access requires a reverse proxy or Bureau bridge.
	webhookAddress := os.Getenv("BUREAU_GITHUB_WEBHOOK_LISTEN")
	if webhookAddress == "" {
		webhookAddress = "127.0.0.1:9876"
	}

	boot, cleanup, err := service.BootstrapViaProxy(ctx, service.ProxyBootstrapConfig{
		Audience:    "github",
		Description: "GitHub forge connector service",
	})
	if err != nil {
		return err
	}
	defer cleanup()

	githubService := &GitHubService{
		session:       boot.Session,
		service:       boot.Service,
		serviceRoomID: boot.ServiceRoomID,
		logger:        boot.Logger,
	}

	// Create the webhook handler. Events are dispatched to the
	// service's handleEvent method, which will route to the
	// subscription manager and entity mapping handlers.
	webhookHandler := NewWebhookHandler(webhookSecret, boot.Logger, githubService.handleEvent)

	// Start the HTTP server for webhook ingestion.
	httpServer := service.NewHTTPServer(service.HTTPServerConfig{
		Address: webhookAddress,
		Handler: webhookHandler,
		Logger:  boot.Logger,
	})

	httpDone := make(chan error, 1)
	go func() {
		httpDone <- httpServer.Serve(ctx)
	}()

	// Wait for the HTTP server to be ready before announcing.
	select {
	case <-httpServer.Ready():
		boot.Logger.Info("webhook listener ready",
			"address", httpServer.Addr().String(),
		)
	case <-ctx.Done():
		return ctx.Err()
	}

	// Start the CBOR socket server for agent operations.
	socketServer := boot.NewSocketServer()
	socketServer.RegisterRevocationHandler()
	githubService.registerActions(socketServer)

	socketDone := make(chan error, 1)
	go func() {
		socketDone <- socketServer.Serve(ctx)
	}()

	// Start the incremental sync loop.
	go service.RunSyncLoop(ctx, boot.Session, service.SyncConfig{
		Filter: syncFilter,
	}, "", githubService.handleSync, boot.Clock, boot.Logger)

	boot.Logger.Info("github service running",
		"principal", boot.PrincipalName,
		"socket", boot.SocketPath,
		"webhook_address", webhookAddress,
	)

	// Wait for shutdown signal.
	<-ctx.Done()
	boot.Logger.Info("shutting down")

	// Wait for both servers to drain.
	if err := <-httpDone; err != nil {
		boot.Logger.Error("http server error", "error", err)
	}
	if err := <-socketDone; err != nil {
		boot.Logger.Error("socket server error", "error", err)
	}

	return nil
}

// readWebhookSecret reads the HMAC secret from the file specified by
// BUREAU_GITHUB_WEBHOOK_SECRET_FILE. The secret must be non-empty.
func readWebhookSecret() ([]byte, error) {
	secretFile := os.Getenv("BUREAU_GITHUB_WEBHOOK_SECRET_FILE")
	if secretFile == "" {
		return nil, fmt.Errorf("BUREAU_GITHUB_WEBHOOK_SECRET_FILE is required")
	}

	data, err := os.ReadFile(secretFile)
	if err != nil {
		return nil, fmt.Errorf("reading webhook secret from %s: %w", secretFile, err)
	}

	secret := []byte(strings.TrimSpace(string(data)))
	if len(secret) == 0 {
		return nil, fmt.Errorf("webhook secret file %s is empty", secretFile)
	}

	return secret, nil
}
