// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/watchdog"
	"github.com/bureau-foundation/bureau/messaging"
)

// watchdogMaxAge is the maximum age of a watchdog file that will be
// acted upon during startup. A watchdog older than this is treated as
// stale (from an unrelated restart) and silently cleared. Five minutes
// is generous — normal daemon startup completes in seconds.
const watchdogMaxAge = 5 * time.Minute

// daemonWatchdogPath returns the path to the daemon watchdog file.
func (d *Daemon) daemonWatchdogPath() string {
	return filepath.Join(d.stateDir, "daemon-watchdog.cbor")
}

// checkDaemonWatchdog reads the daemon watchdog file on startup and
// determines whether a previous exec() transition succeeded or failed.
//
// When the watchdog exists and is recent (within watchdogMaxAge):
//   - If daemonBinaryPath == state.NewBinary: the exec succeeded. The
//     new binary is running. Reports success to Matrix, clears watchdog.
//   - If daemonBinaryPath == state.PreviousBinary: the exec failed. The
//     old binary was restarted (by systemd or manual intervention).
//     Reports failure to Matrix, clears watchdog, returns the failed
//     store path so the caller can seed failedExecPaths.
//   - If daemonBinaryPath matches neither: unrelated restart (e.g.,
//     machine rebooted with a different binary deployed). Clears the
//     stale watchdog.
//
// When the watchdog does not exist or is stale: no-op (normal startup).
func checkDaemonWatchdog(
	watchdogPath string,
	daemonBinaryPath string,
	session *messaging.DirectSession,
	configRoomID ref.RoomID,
	logger *slog.Logger,
) (failedPath string) {
	state, found, err := watchdog.Check(watchdogPath, watchdogMaxAge)
	if err != nil {
		logger.Error("reading daemon watchdog", "path", watchdogPath, "error", err)
		return ""
	}
	if !found {
		return ""
	}

	// A recent watchdog exists — this process started after an exec()
	// transition attempt (or a crash during one).

	switch daemonBinaryPath {
	case state.NewBinary:
		// We are the new binary. The exec() succeeded.
		logger.Info("daemon exec() succeeded",
			"previous", state.PreviousBinary,
			"new", state.NewBinary,
		)
		if session != nil && !configRoomID.IsZero() {
			session.SendEvent(context.Background(), configRoomID, schema.MatrixEventTypeMessage,
				schema.NewDaemonSelfUpdateMessage(state.PreviousBinary, state.NewBinary, schema.SelfUpdateSucceeded, ""))
		}

	case state.PreviousBinary:
		// We are the old binary. The new binary crashed or failed to
		// start, and this process was restarted (by systemd, manual
		// intervention, etc.).
		logger.Error("daemon exec() failed: old binary restarted",
			"attempted", state.NewBinary,
			"current", state.PreviousBinary,
		)
		if session != nil && !configRoomID.IsZero() {
			session.SendEvent(context.Background(), configRoomID, schema.MatrixEventTypeMessage,
				schema.NewDaemonSelfUpdateMessage(state.PreviousBinary, state.NewBinary, schema.SelfUpdateFailed,
					fmt.Sprintf("%s crashed or failed to start", state.NewBinary)))
		}
		failedPath = state.NewBinary

	default:
		// Neither match — this process is a different binary entirely
		// (e.g., manually deployed a third version). The watchdog is
		// from a stale transition that no longer applies.
		logger.Info("clearing stale daemon watchdog (current binary matches neither previous nor new)",
			"current", daemonBinaryPath,
			"watchdog_previous", state.PreviousBinary,
			"watchdog_new", state.NewBinary,
		)
	}

	if err := watchdog.Clear(watchdogPath); err != nil {
		logger.Error("clearing daemon watchdog", "path", watchdogPath, "error", err)
	}

	return failedPath
}

// execDaemon performs the daemon self-update: writes the watchdog state
// file, then exec()'s the new binary with the same arguments and
// environment.
//
// On success, this function does not return (the process is replaced).
// On failure, it clears the watchdog, reports the error to Matrix, adds
// the path to failedExecPaths (preventing retries), and returns the
// error so the caller can continue running the current binary.
func (d *Daemon) execDaemon(ctx context.Context, newBinaryPath string) error {
	// Retry protection: don't attempt exec for a store path that
	// already failed during this process lifetime.
	if d.failedExecPaths[newBinaryPath] {
		d.logger.Warn("skipping daemon exec (previously failed for this store path)",
			"store_path", newBinaryPath,
		)
		return nil
	}

	// Guard: if we don't know our own path, we can't write a meaningful
	// watchdog (the startup check needs PreviousBinary to detect failure).
	if d.daemonBinaryPath == "" {
		return fmt.Errorf("daemon binary path unknown, cannot write watchdog for exec")
	}

	watchdogPath := d.daemonWatchdogPath()

	state := watchdog.State{
		Component:      "daemon",
		PreviousBinary: d.daemonBinaryPath,
		NewBinary:      newBinaryPath,
		Timestamp:      time.Now(),
	}

	if err := watchdog.Write(watchdogPath, state); err != nil {
		return fmt.Errorf("writing daemon watchdog: %w", err)
	}

	d.logger.Info("exec()'ing new daemon binary",
		"previous", d.daemonBinaryPath,
		"new", newBinaryPath,
	)

	// Report to Matrix before exec. If the exec succeeds, this is the
	// last message from the old process. The new process reports success
	// via checkDaemonWatchdog on startup.
	if _, err := d.sendEventRetry(ctx, d.configRoomID, schema.MatrixEventTypeMessage,
		schema.NewDaemonSelfUpdateMessage(d.daemonBinaryPath, newBinaryPath, schema.SelfUpdateInProgress, "")); err != nil {
		d.logger.Error("failed to post exec notification", "error", err)
	}

	execFunction := d.execFunc
	if execFunction == nil {
		execFunction = syscall.Exec
	}

	argv := append([]string{newBinaryPath}, os.Args[1:]...)
	err := execFunction(newBinaryPath, argv, os.Environ())

	// If we reach here, exec() failed. The process was NOT replaced.
	d.logger.Error("daemon exec() failed, continuing with current binary",
		"new_binary", newBinaryPath,
		"error", err,
	)

	// Clear the watchdog — we're not in a transition state.
	if clearErr := watchdog.Clear(watchdogPath); clearErr != nil {
		d.logger.Error("clearing watchdog after exec failure",
			"path", watchdogPath,
			"error", clearErr,
		)
	}

	// Record this path as failed to prevent retry loops.
	d.failedExecPaths[newBinaryPath] = true

	// Report the failure.
	if _, sendErr := d.sendEventRetry(ctx, d.configRoomID, schema.MatrixEventTypeMessage,
		schema.NewDaemonSelfUpdateMessage(d.daemonBinaryPath, newBinaryPath, schema.SelfUpdateFailed,
			fmt.Sprintf("exec() %s: %v", newBinaryPath, err))); sendErr != nil {
		d.logger.Error("failed to post exec failure notification", "error", sendErr)
	}

	return fmt.Errorf("exec %s: %w", newBinaryPath, err)
}
