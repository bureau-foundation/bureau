// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"os/signal"
	"syscall"
	"time"

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
		version.Print("bureau-model-service")
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Load credentials for model provider API keys. The launcher
	// delivers these as files in a directory, one file per credential
	// ref. The env var BUREAU_MODEL_CREDENTIALS_DIR points to this
	// directory.
	credentials, err := loadCredentials()
	if err != nil {
		return err
	}
	defer credentials.Close()

	boot, cleanup, err := service.BootstrapViaProxy(ctx, service.ProxyBootstrapConfig{
		Audience:     "model",
		Description:  "Model inference gateway — completion, embedding, streaming",
		Capabilities: []string{"completion", "embedding", "streaming"},
	})
	if err != nil {
		return err
	}
	defer cleanup()

	modelService := newModelService(boot, credentials)
	modelService.latencyRouter = NewLatencyRouter(
		ctx, boot.Clock, boot.Logger,
		DefaultLatencyConfig(),
	)

	// Perform initial /sync to populate the model registry with
	// provider, alias, and account configuration.
	sinceToken, err := modelService.initialSync(ctx)
	if err != nil {
		return fmt.Errorf("initial sync: %w", err)
	}

	// Start the socket server in a goroutine.
	socketServer := boot.NewSocketServer()
	socketServer.RegisterRevocationHandler()
	modelService.registerActions(socketServer)

	socketDone := make(chan error, 1)
	go func() {
		socketDone <- socketServer.Serve(ctx)
	}()

	// Start the incremental sync loop in a goroutine.
	go service.RunSyncLoop(ctx, boot.Session, service.SyncConfig{
		Filter: syncFilter,
	}, sinceToken, modelService.handleSync, boot.Clock, boot.Logger)

	// Start the periodic quota cleanup goroutine. Old spend entries
	// from expired daily/monthly windows are removed to prevent
	// unbounded growth of the spend map.
	go modelService.runQuotaCleanup(ctx)

	boot.Logger.Info("model service running",
		"principal", boot.PrincipalName,
		"socket", boot.SocketPath,
		"providers", modelService.registry.ProviderCount(),
		"aliases", modelService.registry.AliasCount(),
		"accounts", modelService.registry.AccountCount(),
	)

	// Wait for shutdown signal.
	<-ctx.Done()
	boot.Logger.Info("shutting down")

	// Close the latency router first: flushes pending embed batches
	// (last-chance service), releases completion gate waiters, and
	// wakes background waiters. Must happen before closing providers
	// since pending flushes may make provider API calls.
	modelService.latencyRouter.Close()

	// Close all provider HTTP clients.
	modelService.closeProviders()

	// Wait for the socket server to drain active connections.
	if err := <-socketDone; err != nil {
		boot.Logger.Error("socket server error", "error", err)
	}

	return nil
}

// runQuotaCleanup periodically removes spend entries from expired time
// windows. Runs every hour until the context is cancelled.
func (ms *ModelService) runQuotaCleanup(ctx context.Context) {
	ticker := ms.clock.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ms.quotaTracker.Cleanup()
		}
	}
}
