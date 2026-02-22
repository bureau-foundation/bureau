// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package agentdriver

import (
	"context"
	"io"
	"os"
)

// Process represents a running agent process. Driver implementations return
// this from Start. The lifecycle manager uses it to wait for completion,
// write to stdin (for message injection), and send signals.
type Process interface {
	// Wait blocks until the process exits and returns its exit error.
	// Returns nil if the process exited with status 0.
	Wait() error

	// Stdin returns the write end of the process's stdin pipe. Writing to
	// this pipe injects content into the agent. The semantics of injected
	// content are agent-specific (e.g., Claude Code reads newline-delimited
	// prompts from stdin in print mode).
	Stdin() io.Writer

	// Signal sends an OS signal to the process.
	Signal(signal os.Signal) error
}

// DriverConfig holds the configuration passed to Driver.Start.
type DriverConfig struct {
	// Prompt is the initial prompt to give the agent.
	Prompt string

	// SystemPromptFile is the path to a file containing the Bureau system
	// prompt. The driver appends this to the agent's system prompt via
	// the appropriate flag (e.g., --append-system-prompt-file for Claude Code).
	SystemPromptFile string

	// SessionID is a unique identifier for this agent session, used for
	// log file naming and Matrix message correlation.
	SessionID string

	// WorkingDirectory is the directory the agent process should start in.
	WorkingDirectory string

	// ExtraEnv is additional environment variables to set for the agent
	// process, in "KEY=VALUE" format.
	ExtraEnv []string
}

// Driver is the abstraction boundary between Bureau lifecycle management
// and agent-specific behavior. Each agent runtime (Claude Code, Codex,
// Gemini, etc.) implements this interface.
type Driver interface {
	// Start spawns the agent process with the given configuration.
	// Returns a Process handle and the process's stdout reader. The
	// stdout reader is passed to ParseOutput for event extraction.
	// The caller is responsible for reading stdout to completion
	// (via ParseOutput) before calling Process.Wait.
	Start(ctx context.Context, config DriverConfig) (Process, io.ReadCloser, error)

	// ParseOutput reads the agent's stdout stream and emits structured
	// events on the provided channel. Called in a goroutine — should
	// block until the reader returns EOF or the context is cancelled.
	// The caller closes the events channel after ParseOutput returns.
	ParseOutput(ctx context.Context, stdout io.Reader, events chan<- Event) error

	// Interrupt requests the agent to stop gracefully. The implementation
	// should send the appropriate signal for the runtime (e.g., SIGINT for
	// Claude Code, which finishes the current tool call and exits).
	Interrupt(process Process) error
}
