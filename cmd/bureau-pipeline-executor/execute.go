// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/bureau-foundation/bureau/lib/artifact"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// defaultStepTimeout is used when a step does not specify its own timeout.
const defaultStepTimeout = 5 * time.Minute

// maxInlineOutputSize is the maximum size for inline (non-artifact) output
// files. 64 KB is well within Matrix event size limits and is sufficient
// for commit SHAs, branch names, version strings, and other small text
// values that pipelines typically produce. Larger outputs should use
// artifact mode.
const maxInlineOutputSize = 64 * 1024

// stepResult captures the outcome of executing a single pipeline step.
type stepResult struct {
	status   string // "ok", "failed", "skipped", "aborted"
	duration time.Duration
	err      error
	outputs  map[string]string
}

// executeStep runs a single pipeline step: evaluates the when guard, runs
// the command or publishes a state event, runs the check command, and
// captures declared outputs. Returns the step result.
//
// The artifacts client may be nil — artifact-mode outputs will fail with
// a clear error if the artifact service is not available.
func executeStep(ctx context.Context, step schema.PipelineStep, index, total int, session messaging.Session, artifacts *artifact.Client, logger *threadLogger) stepResult {
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
		publishRoomID, err := ref.ParseRoomID(step.Publish.Room)
		if err != nil {
			return stepResult{
				status:   "failed",
				duration: time.Since(startTime),
				err:      fmt.Errorf("publish: invalid room ID %q: %w", step.Publish.Room, err),
			}
		}
		_, err = session.SendStateEvent(stepContext, publishRoomID, step.Publish.EventType, step.Publish.StateKey, step.Publish.Content)
		if err != nil {
			return stepResult{
				status:   "failed",
				duration: time.Since(startTime),
				err:      fmt.Errorf("publish: %w", err),
			}
		}
	} else if step.AssertState != nil {
		assertResult := executeAssertState(stepContext, step.AssertState, session)
		if assertResult.err != nil {
			return stepResult{
				status:   assertResult.status,
				duration: time.Since(startTime),
				err:      assertResult.err,
			}
		}
	}

	// Capture declared outputs after the step action succeeds. Output
	// capture runs inside the step's timeout context — a slow artifact
	// store counts against the step's time budget.
	var outputs map[string]string
	if len(step.Outputs) > 0 {
		parsed, err := schema.ParseStepOutputs(step.Outputs)
		if err != nil {
			return stepResult{
				status:   "failed",
				duration: time.Since(startTime),
				err:      fmt.Errorf("parsing output declarations: %w", err),
			}
		}
		outputs, err = captureStepOutputs(stepContext, parsed, artifacts)
		if err != nil {
			return stepResult{
				status:   "failed",
				duration: time.Since(startTime),
				err:      fmt.Errorf("capturing outputs: %w", err),
			}
		}
	}

	duration := time.Since(startTime)
	fmt.Printf("[pipeline] step %d/%d: %s... ok (%s)\n", index+1, total, step.Name, formatDuration(duration))
	logger.logStep(ctx, index, total, step.Name, "ok", duration)
	return stepResult{status: "ok", duration: duration, outputs: outputs}
}

// executeAssertState reads a Matrix state event and checks a condition against
// a field value. Returns a result with status "ok" on match, or "failed" /
// "aborted" on mismatch depending on the OnMismatch setting.
//
// The "abort" status is semantically different from "fail": abort means the
// pipeline's precondition is no longer valid (e.g., the resource state changed
// between when the pipeline was queued and when it started executing). The
// pipeline exits cleanly with no error — there is nothing broken, the work
// is simply no longer needed.
//
// The "fail" status (default) means the assertion is a hard requirement that
// was not met, and the pipeline should report failure.
func executeAssertState(ctx context.Context, assertion *schema.PipelineAssertState, session messaging.Session) stepResult {
	// Parse the room ID from the pipeline step definition.
	assertRoomID, err := ref.ParseRoomID(assertion.Room)
	if err != nil {
		return stepResult{
			status: "failed",
			err:    fmt.Errorf("assert_state: invalid room ID %q: %w", assertion.Room, err),
		}
	}

	// Read the state event.
	rawContent, err := session.GetStateEvent(ctx, assertRoomID, assertion.EventType, assertion.StateKey)
	if err != nil {
		return stepResult{
			status: "failed",
			err:    fmt.Errorf("assert_state: reading state event %s/%s in %s: %w", assertion.EventType, assertion.StateKey, assertion.Room, err),
		}
	}

	// Parse the content as a generic map and extract the field value.
	var content map[string]any
	if err := json.Unmarshal(rawContent, &content); err != nil {
		return stepResult{
			status: "failed",
			err:    fmt.Errorf("assert_state: parsing state event content: %w", err),
		}
	}

	rawFieldValue, exists := content[assertion.Field]
	if !exists {
		return stepResult{
			status: "failed",
			err:    fmt.Errorf("assert_state: field %q not found in state event content", assertion.Field),
		}
	}

	// Stringify the field value for comparison. Pipeline assertions operate
	// on string equality — the field value is coerced to its JSON string
	// representation. This covers the common case (string status fields)
	// and gives predictable behavior for other types.
	fieldValue := fmt.Sprintf("%v", rawFieldValue)

	// Evaluate the condition.
	matched := false
	var conditionDescription string

	switch {
	case assertion.Equals != "":
		matched = fieldValue == assertion.Equals
		conditionDescription = fmt.Sprintf("equals %q", assertion.Equals)
	case assertion.NotEquals != "":
		matched = fieldValue != assertion.NotEquals
		conditionDescription = fmt.Sprintf("not_equals %q", assertion.NotEquals)
	case len(assertion.In) > 0:
		for _, allowed := range assertion.In {
			if fieldValue == allowed {
				matched = true
				break
			}
		}
		conditionDescription = fmt.Sprintf("in %v", assertion.In)
	case len(assertion.NotIn) > 0:
		matched = true
		for _, forbidden := range assertion.NotIn {
			if fieldValue == forbidden {
				matched = false
				break
			}
		}
		conditionDescription = fmt.Sprintf("not_in %v", assertion.NotIn)
	}

	if matched {
		return stepResult{status: "ok"}
	}

	// Determine the mismatch behavior.
	status := "failed"
	if assertion.OnMismatch == "abort" {
		status = "aborted"
	}

	message := assertion.Message
	if message == "" {
		message = fmt.Sprintf("field %q is %q, expected %s", assertion.Field, fieldValue, conditionDescription)
	}

	return stepResult{
		status: status,
		err:    fmt.Errorf("assert_state: %s", message),
	}
}

// captureStepOutputs reads the declared output files and returns a map
// of output name → value. For inline outputs, the file content is read
// as a string (64 KB limit, trailing whitespace trimmed). For artifact
// outputs, the file is streamed to the artifact service and the returned
// art-* reference becomes the value.
func captureStepOutputs(ctx context.Context, outputs map[string]schema.PipelineStepOutput, artifacts *artifact.Client) (map[string]string, error) {
	result := make(map[string]string, len(outputs))

	for name, output := range outputs {
		value, err := captureOneOutput(ctx, name, output, artifacts)
		if err != nil {
			return nil, fmt.Errorf("output %q: %w", name, err)
		}
		result[name] = value
	}

	return result, nil
}

// captureOneOutput reads a single output file and returns its value as
// either inline content or an artifact reference.
func captureOneOutput(ctx context.Context, name string, output schema.PipelineStepOutput, artifacts *artifact.Client) (string, error) {
	if output.Artifact {
		return captureArtifactOutput(ctx, name, output, artifacts)
	}
	return captureInlineOutput(output.Path)
}

// captureInlineOutput reads a file as an inline string value. The file
// must exist and be at most maxInlineOutputSize bytes. Trailing
// whitespace (newlines, spaces) is trimmed — most commands write a
// trailing newline that callers don't want in their variables.
func captureInlineOutput(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("output file %s: %w", path, err)
	}
	if info.Size() > maxInlineOutputSize {
		return "", fmt.Errorf(
			"output file %s is %d bytes, exceeding the %d byte limit for inline outputs; "+
				"use artifact mode for large outputs",
			path, info.Size(), maxInlineOutputSize,
		)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading output file %s: %w", path, err)
	}

	return strings.TrimRight(string(data), " \t\n\r"), nil
}

// captureArtifactOutput streams a file to the artifact service and returns
// the art-* content-addressed reference. The artifact client must be
// non-nil — the executor creates it at startup when the artifact service
// socket is available in the sandbox.
func captureArtifactOutput(ctx context.Context, name string, output schema.PipelineStepOutput, artifacts *artifact.Client) (string, error) {
	if artifacts == nil {
		return "", fmt.Errorf(
			"artifact mode requires the artifact service, but no artifact socket is available in this sandbox; " +
				"ensure the artifact service is running and the executor sandbox includes the artifact socket",
		)
	}

	file, err := os.Open(output.Path)
	if err != nil {
		return "", fmt.Errorf("opening output file %s: %w", output.Path, err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("stat output file %s: %w", output.Path, err)
	}

	header := &artifact.StoreHeader{
		Action:      "store",
		ContentType: output.ContentType,
		Filename:    name,
		Size:        info.Size(),
		Description: output.Description,
	}

	// For small files, embed the content directly in the header
	// (avoids the streaming protocol overhead).
	var reader io.Reader
	if info.Size() <= artifact.SmallArtifactThreshold {
		data, readErr := io.ReadAll(file)
		if readErr != nil {
			return "", fmt.Errorf("reading output file %s: %w", output.Path, readErr)
		}
		header.Data = data
	} else {
		reader = file
	}

	response, err := artifacts.Store(ctx, header, reader)
	if err != nil {
		return "", fmt.Errorf("storing artifact for output %q: %w", name, err)
	}

	return response.Ref, nil
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
