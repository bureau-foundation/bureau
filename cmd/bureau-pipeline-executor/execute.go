// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema"
)

// defaultStepTimeout is used when a step does not specify its own timeout.
const defaultStepTimeout = 5 * time.Minute

// stepResult captures the outcome of executing a single pipeline step.
type stepResult struct {
	status   string // "ok", "failed", "skipped"
	duration time.Duration
	err      error
}

// executeStep runs a single pipeline step: evaluates the when guard, runs
// the command or publishes a state event, and runs the check command.
// Returns the step result.
func executeStep(ctx context.Context, step schema.PipelineStep, index, total int, proxy *proxyClient, logger *threadLogger) stepResult {
	startTime := time.Now()

	// Parse timeout.
	timeout := defaultStepTimeout
	if step.Timeout != "" {
		parsed, err := time.ParseDuration(step.Timeout)
		if err != nil {
			// Validate should have caught this, but fail loud if not.
			return stepResult{
				status:   "failed",
				duration: time.Since(startTime),
				err:      fmt.Errorf("invalid timeout %q: %w", step.Timeout, err),
			}
		}
		timeout = parsed
	}

	// Parse grace period for graceful step termination.
	var gracePeriod time.Duration
	if step.GracePeriod != "" {
		parsed, err := time.ParseDuration(step.GracePeriod)
		if err != nil {
			// Validate should have caught this, but fail loud if not.
			return stepResult{
				status:   "failed",
				duration: time.Since(startTime),
				err:      fmt.Errorf("invalid grace_period %q: %w", step.GracePeriod, err),
			}
		}
		gracePeriod = parsed
	}

	stepContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Interactive steps are deferred from the MVP — fail with a clear message.
	if step.Interactive {
		return stepResult{
			status:   "failed",
			duration: time.Since(startTime),
			err:      fmt.Errorf("interactive steps are not yet supported (step %q)", step.Name),
		}
	}

	// Evaluate when guard (run steps only). Guards are quick verification
	// commands — always use immediate SIGKILL on timeout (gracePeriod 0).
	if step.When != "" {
		exitCode, err := runShellCommand(stepContext, step.When, step.Env, 0)
		if err != nil {
			return stepResult{
				status:   "failed",
				duration: time.Since(startTime),
				err:      fmt.Errorf("when guard: %w", err),
			}
		}
		if exitCode != 0 {
			duration := time.Since(startTime)
			fmt.Printf("[pipeline] step %d/%d: %s... skipped (guard condition not met)\n", index+1, total, step.Name)
			logger.logStep(ctx, index, total, step.Name, "skipped", duration)
			return stepResult{status: "skipped", duration: duration}
		}
	}

	// Execute run command or publish step.
	if step.Run != "" {
		exitCode, err := runShellCommand(stepContext, step.Run, step.Env, gracePeriod)
		if err != nil {
			return stepResult{
				status:   "failed",
				duration: time.Since(startTime),
				err:      fmt.Errorf("run: %w", err),
			}
		}
		if exitCode != 0 {
			return stepResult{
				status:   "failed",
				duration: time.Since(startTime),
				err:      fmt.Errorf("run: exit code %d", exitCode),
			}
		}

		// Run check command if present. Checks are quick verification
		// commands — always use immediate SIGKILL on timeout.
		if step.Check != "" {
			checkExitCode, err := runShellCommand(stepContext, step.Check, step.Env, 0)
			if err != nil {
				return stepResult{
					status:   "failed",
					duration: time.Since(startTime),
					err:      fmt.Errorf("check: %w", err),
				}
			}
			if checkExitCode != 0 {
				return stepResult{
					status:   "failed",
					duration: time.Since(startTime),
					err:      fmt.Errorf("check: exit code %d", checkExitCode),
				}
			}
		}
	} else if step.Publish != nil {
		_, err := proxy.putState(stepContext, step.Publish.Room, step.Publish.EventType, step.Publish.StateKey, step.Publish.Content)
		if err != nil {
			return stepResult{
				status:   "failed",
				duration: time.Since(startTime),
				err:      fmt.Errorf("publish: %w", err),
			}
		}
	}

	duration := time.Since(startTime)
	fmt.Printf("[pipeline] step %d/%d: %s... ok (%s)\n", index+1, total, step.Name, formatDuration(duration))
	logger.logStep(ctx, index, total, step.Name, "ok", duration)
	return stepResult{status: "ok", duration: duration}
}

// runShellCommand executes a command via sh -c with stdout and stderr
// inherited from the executor process. Additional environment variables
// from the step's env map are set on the command. Returns the exit code
// and any error (signals, context cancellation, etc.).
//
// The shell is resolved via PATH, not hardcoded to /bin/sh. Inside bwrap
// sandboxes the Nix environment's bin directory is on PATH but /bin may
// not exist. PATH lookup is also more correct on NixOS hosts where
// /bin/sh may be a different shell than the environment's.
//
// The command runs in its own process group so that context cancellation
// (timeout) kills the shell and all its children. Without Setpgid, only
// the shell receives the signal — child processes survive and hold open
// the inherited stdout/stderr file descriptors, blocking the parent from
// exiting until the children finish.
//
// When gracePeriod is zero, SIGKILL is sent immediately on timeout. This
// is the default for most sandbox steps: sandbox processes are ephemeral
// and should not hold the pipeline hostage.
//
// When gracePeriod is positive, SIGTERM is sent first to give the process
// a chance to clean up (flush buffers, commit transactions, close
// connections). If the process has not exited after gracePeriod, SIGKILL
// is sent to force termination. Use this for steps that perform
// irreversible operations where abrupt termination could leave state
// inconsistent.
func runShellCommand(ctx context.Context, command string, env map[string]string, gracePeriod time.Duration) (int, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Put the command in its own process group so that signals reach
	// the shell and all its children (negative PID = all processes
	// in the group).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if gracePeriod > 0 {
		// Graceful: SIGTERM the process group first. A background
		// goroutine escalates to SIGKILL after the grace period
		// if the process has not exited. The SIGKILL targets the
		// process group (not just the shell) so children spawned
		// by the command are also terminated.
		cmd.Cancel = func() error {
			processGroupID := -cmd.Process.Pid
			if err := syscall.Kill(processGroupID, syscall.SIGTERM); err != nil {
				// SIGTERM failed (process group already gone), escalate.
				return syscall.Kill(processGroupID, syscall.SIGKILL)
			}
			go func() {
				time.Sleep(gracePeriod)
				// Best-effort: the process group may have already exited.
				// ESRCH from a dead process group is harmless.
				_ = syscall.Kill(processGroupID, syscall.SIGKILL)
			}()
			return nil
		}
	} else {
		// Immediate: SIGKILL the entire process group.
		cmd.Cancel = func() error {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}

	// Set step-level environment variables.
	if len(env) > 0 {
		cmd.Env = os.Environ()
		for name, value := range env {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}

	err := cmd.Run()
	if err == nil {
		return 0, nil
	}

	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return exitError.ExitCode(), nil
	}

	// Non-exit errors: context cancellation (timeout), signal, etc.
	return -1, err
}
