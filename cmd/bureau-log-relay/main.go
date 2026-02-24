// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
)

func main() {
	os.Exit(run())
}

// relayConfig holds parsed command-line options for the relay.
type relayConfig struct {
	command      []string
	exitCodeFile string
}

// run executes the child command and returns its exit code.
//
// The child inherits stdin/stdout/stderr directly (no PTY interposition).
// This keeps the outer PTY slave fds held open by this process until it
// exits, which only happens after waitpid collects the child's status.
//
// When exitCodeFile is configured, the relay writes the child's exit code
// to the specified path (as ASCII decimal + newline) after child.Wait()
// completes but before os.Exit(). The write uses temp file + rename for
// atomicity: the inotify IN_MOVED_TO event in the session watcher fires
// only when the file content is complete.
//
// Signal forwarding ensures that signals sent to this process (the tmux
// pane PID) reach the child. The launcher's SignalPane sends SIGTERM to
// the pane PID for graceful drain; the relay forwards it to the child
// (bwrap), which forwards it to the sandboxed agent.
func run() int {
	config, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "bureau-log-relay: %v\n", err)
		return 1
	}

	child := exec.Command(config.command[0], config.command[1:]...)
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
	exitCode := 0
	if err := child.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			// Wait failed for a non-exit reason (e.g., the child was
			// already reaped by something else, which shouldn't happen).
			fmt.Fprintf(os.Stderr, "bureau-log-relay: waiting for child: %v\n", err)
			exitCode = 1
		}
	}

	// Write the exit code to a file before exiting. The launcher's
	// session watcher reads this via inotify to get the authoritative
	// exit code, bypassing the tmux #{pane_dead_status} race condition
	// where tmux reports a stale exit code under load.
	//
	// The write happens after child.Wait() (exit code known) and before
	// os.Exit() (PTY fds still open). This causal ordering guarantees
	// the file is complete before tmux detects pane death.
	if config.exitCodeFile != "" {
		if writeErr := writeExitCode(config.exitCodeFile, exitCode); writeErr != nil {
			fmt.Fprintf(os.Stderr, "bureau-log-relay: %v\n", writeErr)
			// Don't change the exit code — the child's exit code is
			// the correct one to propagate via the PTY.
		}
	}

	return exitCode
}

// writeExitCode writes an exit code to the specified path using atomic
// rename. The temp file is written first, then renamed into place so
// that the inotify IN_MOVED_TO event fires only when the content is
// complete.
func writeExitCode(path string, exitCode int) error {
	content := []byte(strconv.Itoa(exitCode) + "\n")

	tempPath := path + ".tmp"
	if err := os.WriteFile(tempPath, content, 0644); err != nil {
		return fmt.Errorf("writing exit code temp file %s: %w", tempPath, err)
	}

	if err := os.Rename(tempPath, path); err != nil {
		// Clean up the temp file on rename failure.
		os.Remove(tempPath)
		return fmt.Errorf("renaming exit code file %s -> %s: %w", tempPath, path, err)
	}

	return nil
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

// parseArgs extracts relay configuration from the argument list. The relay
// accepts arguments in the form:
//
//	bureau-log-relay [--exit-code-file=<path>] [--] <command> [args...]
//
// Flags must appear before the "--" separator (or before the command if
// no separator is used). The optional "--" separator is consumed and not
// passed to the child. At least one argument (the command) must follow.
func parseArgs(args []string) (relayConfig, error) {
	if len(args) == 0 {
		return relayConfig{}, fmt.Errorf("usage: bureau-log-relay [--exit-code-file=<path>] [--] <command> [args...]\n\n" +
			"This binary is spawned by the launcher. It is not intended for direct use.")
	}

	var config relayConfig

	// Parse flags before the command or "--" separator.
	for len(args) > 0 {
		if args[0] == "--" {
			args = args[1:]
			break
		}
		if strings.HasPrefix(args[0], "--exit-code-file=") {
			config.exitCodeFile = strings.TrimPrefix(args[0], "--exit-code-file=")
			if config.exitCodeFile == "" {
				return relayConfig{}, fmt.Errorf("--exit-code-file requires a non-empty path")
			}
			args = args[1:]
			continue
		}
		// Not a known flag and not "--" — this is the start of the command.
		break
	}

	if len(args) == 0 {
		return relayConfig{}, fmt.Errorf("no command specified after flags")
	}

	config.command = args
	return config, nil
}
