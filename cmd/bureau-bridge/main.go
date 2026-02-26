// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/bureau-foundation/bureau/bridge"
	"github.com/bureau-foundation/bureau/lib/version"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Parse flags manually to handle -- separator.
	listenAddr := "127.0.0.1:8642"
	socketPath := "/run/bureau/proxy.sock"
	verbose := false
	var execCommand []string

	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			// Everything after -- is the command to exec.
			execCommand = args[i+1:]
			i = len(args) // Stop parsing.
		case arg == "--listen" || arg == "-l":
			if i+1 >= len(args) {
				return fmt.Errorf("--listen requires an argument")
			}
			i++
			listenAddr = args[i]
		case arg == "--socket" || arg == "-s":
			if i+1 >= len(args) {
				return fmt.Errorf("--socket requires an argument")
			}
			i++
			socketPath = args[i]
		case arg == "--verbose" || arg == "-v":
			verbose = true
		case arg == "--help" || arg == "-h":
			printUsage()
			return nil
		case arg == "--version":
			fmt.Printf("bureau-bridge %s\n", version.Info())
			return nil
		default:
			return fmt.Errorf("unknown flag: %s", arg)
		}
	}

	// Configure logger: verbose enables Debug level for per-connection
	// events; default Info level shows only lifecycle and errors.
	logLevel := slog.LevelInfo
	if verbose {
		logLevel = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel,
	}))
	slog.SetDefault(logger)

	b := &bridge.Bridge{
		ListenAddr: listenAddr,
		SocketPath: socketPath,
		Logger:     logger,
	}

	if len(execCommand) > 0 {
		return runExecMode(b, execCommand)
	}

	return runStandalone(b)
}

func printUsage() {
	fmt.Print(`bureau-bridge - Bridge TCP to Unix socket

USAGE
    bureau-bridge [flags]
    bureau-bridge [flags] -- <command> [args...]

FLAGS
    -l, --listen <addr>    TCP address to listen on (default: 127.0.0.1:8642)
    -s, --socket <path>    Unix socket to forward to (default: /run/bureau/proxy.sock)
    -v, --verbose          Enable per-connection debug logging
    -h, --help             Show this help

EXAMPLES
    # Run as standalone bridge
    bureau-bridge --listen 127.0.0.1:8642 --socket /run/bureau/proxy.sock

    # Run bridge and then exec a command (for sandbox use)
    bureau-bridge -- claude --dangerously-skip-permissions

In exec mode, the bridge runs in the background and the command is exec'd.
The command inherits the bridge's environment, so HTTP clients can reach
the proxy via localhost even in a network-isolated sandbox.
`)
}

// runExecMode starts the bridge, runs a subprocess, then stops the bridge
// when the subprocess exits.
func runExecMode(b *bridge.Bridge, command []string) error {
	ctx := context.Background()

	if err := b.Start(ctx); err != nil {
		return err
	}
	defer b.Stop()

	cmdPath, err := exec.LookPath(command[0])
	if err != nil {
		return fmt.Errorf("command not found: %s", command[0])
	}

	cmd := exec.Command(cmdPath, command[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Forward signals to child.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for sig := range sigChan {
			if cmd.Process != nil {
				cmd.Process.Signal(sig)
			}
		}
	}()

	err = cmd.Run()
	signal.Stop(sigChan)

	// Propagate exit code. Stop the bridge explicitly before os.Exit
	// because os.Exit does not run deferred functions.
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			b.Stop()
			os.Exit(exitErr.ExitCode())
		}
		return err
	}

	return nil
}

// runStandalone runs the bridge until interrupted by SIGINT or SIGTERM.
func runStandalone(b *bridge.Bridge) error {
	ctx := context.Background()

	if err := b.Start(ctx); err != nil {
		return err
	}

	// Wait for shutdown signal.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	b.Stop()
	return nil
}
