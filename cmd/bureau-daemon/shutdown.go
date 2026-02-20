// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"time"

	"github.com/bureau-foundation/bureau/lib/ref"
)

// emergencyShutdown destroys all running sandboxes and cancels the daemon
// context. Called when the daemon detects an unrecoverable condition:
//   - Matrix account deactivated (M_UNKNOWN_TOKEN or M_FORBIDDEN from /sync)
//   - Evicted from the config room (kicked by admin during revocation or
//     decommission — the daemon can no longer read credentials or config)
//
// Uses a fresh context with a 10-second timeout since the daemon's own
// context may be cancelled or the Matrix session may be dead. The launcher
// IPC is local (Unix socket) and doesn't depend on Matrix, so destroying
// sandboxes should succeed even when the homeserver is unreachable.
func (d *Daemon) emergencyShutdown() {
	d.logger.Error("emergency shutdown: destroying all sandboxes")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	d.reconcileMu.Lock()
	// Collect principals first — destroyPrincipal modifies the map.
	principals := make([]ref.Entity, 0, len(d.running))
	for principal := range d.running {
		principals = append(principals, principal)
	}
	for _, principal := range principals {
		if err := d.destroyPrincipal(ctx, principal); err != nil {
			d.logger.Error("emergency shutdown: failed to destroy sandbox",
				"principal", principal, "error", err)
			// Continue — best effort, destroy as many as possible.
		}
	}
	d.reconcileMu.Unlock()

	d.logger.Error("emergency shutdown complete",
		"destroyed", len(principals))

	// Cancel the daemon's top-level context to unblock run()'s <-ctx.Done().
	if d.shutdownCancel != nil {
		d.shutdownCancel()
	}
}
