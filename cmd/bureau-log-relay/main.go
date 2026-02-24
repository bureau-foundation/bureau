// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

func main() {
	os.Exit(run())
}

// run executes the child command and returns its exit code.
//
// The child inherits stdin/stdout/stderr directly (no PTY interposition).
// This keeps the outer PTY slave fds held open by this process until it
// exits, which only happens after waitpid collects the child's status.
//
// Signal forwarding ensures that signals sent to this process (the tmux
// pane PID) reach the child. The launcher's SignalPane sends SIGTERM to
// the pane PID for graceful drain; the relay forwards it to the child
// (bwrap), which forwards it to the sandboxed agent.
func run() int {
	command, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "bureau-log-relay: %v\n", err)
		return 1
	}

	child := exec.Command(command[0], command[1:]...)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr

	if err := child.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "bureau-log-relay: starting child: %v\n", err)
		return 126
	}

	// Forward signals to the child. The signal channel is buffered to
	// avoid missing signals delivered between channel creation and the
	// Notify call, though in practice the window is negligible.
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	go forwardSignals(signals, child.Process)

	// Wait for the child to exit. This blocks until waitpid completes,
	// which means the child's exit code is known before we return and
	// close our file descriptors (triggering PTY EOF for tmux).
	if err := child.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode()
		}
		// Wait failed for a non-exit reason (e.g., the child was
		// already reaped by something else, which shouldn't happen).
		fmt.Fprintf(os.Stderr, "bureau-log-relay: waiting for child: %v\n", err)
		return 1
	}

	return 0
}

// forwardSignals reads signals from the channel and sends them to the
// child process. Stops when the channel is closed or the process is nil.
// Errors are ignored: the child may have already exited, in which case
// the signal delivery fails harmlessly.
func forwardSignals(signals <-chan os.Signal, process *os.Process) {
	for sig := range signals {
		if sysSig, ok := sig.(syscall.Signal); ok {
			// Ignore send errors — the child may have already exited.
			_ = process.Signal(sysSig)
		}
	}
}

// parseArgs extracts the child command from the argument list. The relay
// accepts arguments in the form:
//
//	bureau-log-relay [--] <command> [args...]
//
// The optional "--" separator is consumed and not passed to the child.
// At least one argument (the command) must follow.
func parseArgs(args []string) ([]string, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("usage: bureau-log-relay [--] <command> [args...]\n\n" +
			"This binary is spawned by the launcher. It is not intended for direct use.")
	}

	// Skip the optional "--" separator.
	if args[0] == "--" {
		args = args[1:]
	}

	if len(args) == 0 {
		return nil, fmt.Errorf("no command specified after --")
	}

	return args, nil
}
