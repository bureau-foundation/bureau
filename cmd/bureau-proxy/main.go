// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Bureau-proxy is a credential proxy service for agents.
// It allows agents to use CLI tools without seeing credentials.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bureau-foundation/bureau/lib/version"
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
	var showVersion bool

	flag.StringVar(&configPath, "config", "", "path to config file (required)")
	flag.StringVar(&credentialFile, "credential-file", "", "path to credentials file (key=value format, more secure than env vars)")
	flag.StringVar(&credentialPrefix, "credential-prefix", "BUREAU_", "prefix for environment variable credentials (dev mode)")
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
	// 1. systemd credentials (production)
	// 2. File-based credentials (secure dev - file not visible in /proc)
	// 3. Environment variables (fallback - WARNING: visible in /proc/*/environ)
	sources := []proxy.CredentialSource{
		&proxy.SystemdCredentialSource{},
	}
	if credentialFile != "" {
		sources = append(sources, &proxy.FileCredentialSource{Path: credentialFile})
		logger.Info("using credential file", "path", credentialFile)
	}
	sources = append(sources, &proxy.EnvCredentialSource{Prefix: credentialPrefix})
	credentialSource := &proxy.ChainCredentialSource{Sources: sources}

	// Create server
	server, err := proxy.NewServer(proxy.ServerConfig{
		SocketPath:    config.SocketPath,
		ListenAddress: config.ListenAddress,
		Logger:        logger,
	})
	if err != nil {
		return fmt.Errorf("failed to create server: %w", err)
	}

	// Register services from config
	for name, serviceConfig := range config.Services {
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
