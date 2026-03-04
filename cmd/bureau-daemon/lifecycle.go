// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/ref"
)

// lifecyclePhase represents the mutually exclusive lifecycle state of a
// principal in the daemon's tracking. A principal is in exactly one phase
// at any time — mutual exclusion is structural (a single map entry with
// a phase field) rather than relying on coordinated inserts/deletes
// across four separate maps.
type lifecyclePhase int

const (
	// phaseAbsent is the zero value: the principal is not tracked.
	// A map lookup for a missing key returns this.
	phaseAbsent lifecyclePhase = iota

	// phaseRunning means the principal has an active sandbox and proxy.
	// Set after a successful create-sandbox IPC response or when
	// adopting a pre-existing sandbox from the launcher.
	phaseRunning

	// phaseDraining means the principal was sent SIGTERM and is in its
	// grace period before force-kill. The drainCancel function aborts
	// the grace period timer. Both the sandbox and proxy are still alive.
	phaseDraining

	// phaseCompleted means the principal exited and its RestartPolicy
	// prevents restart (on-failure with exit 0, or never with any exit).
	// The principal will not be recreated until the config changes.
	phaseCompleted

	// phaseStartFailed means the most recent sandbox creation attempt
	// failed. The startFailure field records the category, attempt count,
	// and exponential backoff timing.
	phaseStartFailed
)

// String returns a human-readable name for the phase. Used in logging
// and test failure messages.
func (p lifecyclePhase) String() string {
	switch p {
	case phaseAbsent:
		return "absent"
	case phaseRunning:
		return "running"
	case phaseDraining:
		return "draining"
	case phaseCompleted:
		return "completed"
	case phaseStartFailed:
		return "start_failed"
	default:
		return fmt.Sprintf("lifecyclePhase(%d)", int(p))
	}
}

// principalLifecycle holds the lifecycle state for a single principal.
// Phase-specific fields are only valid when the phase matches:
//   - drainCancel: valid when phase == phaseDraining
//   - startFailure: valid when phase == phaseStartFailed
type principalLifecycle struct {
	phase        lifecyclePhase
	drainCancel  context.CancelFunc
	startFailure *startFailure
}

// isAlive reports whether the principal has an active sandbox process.
// Both running and draining principals have live sandbox processes;
// the difference is whether SIGTERM has been sent.
func (lc principalLifecycle) isAlive() bool {
	return lc.phase == phaseRunning || lc.phase == phaseDraining
}

// --- Daemon transition methods ---
//
// These methods encapsulate all writes to d.lifecycle. Production code
// and tests use these instead of direct map manipulation, so lifecycle
// transitions are always consistent.
//
// All methods require the caller to hold reconcileMu unless noted otherwise.

// setRunning transitions a principal to the running phase. Any previous
// lifecycle state for this principal is replaced.
func (d *Daemon) setRunning(principal ref.Entity) {
	d.lifecycle[principal] = principalLifecycle{phase: phaseRunning}
}

// setDraining transitions a principal to the draining phase with the
// given cancel function for the grace period timer.
func (d *Daemon) setDraining(principal ref.Entity, cancel context.CancelFunc) {
	d.lifecycle[principal] = principalLifecycle{
		phase:       phaseDraining,
		drainCancel: cancel,
	}
}

// setCompleted transitions a principal to the completed phase. The
// principal will not be recreated until the config changes.
func (d *Daemon) setCompleted(principal ref.Entity) {
	d.lifecycle[principal] = principalLifecycle{phase: phaseCompleted}
}

// clearLifecycle removes a principal from lifecycle tracking entirely.
// After this call, d.phase(principal) returns phaseAbsent.
func (d *Daemon) clearLifecycle(principal ref.Entity) {
	delete(d.lifecycle, principal)
}

// cancelDrain cancels the drain grace period timer if the principal is
// in the draining phase. Returns true if the principal was draining.
// Does not change the lifecycle phase — the caller is responsible for
// either clearing the lifecycle entry or transitioning to another phase.
func (d *Daemon) cancelDrain(principal ref.Entity) bool {
	lc := d.lifecycle[principal]
	if lc.phase != phaseDraining {
		return false
	}
	if lc.drainCancel != nil {
		lc.drainCancel()
	}
	return true
}

// --- Daemon query methods ---

// isAlive reports whether the principal has an active sandbox (running
// or draining).
func (d *Daemon) isAlive(principal ref.Entity) bool {
	return d.lifecycle[principal].isAlive()
}

// phase returns the lifecycle phase of a principal. Returns phaseAbsent
// for principals not in the lifecycle map.
func (d *Daemon) phase(principal ref.Entity) lifecyclePhase {
	return d.lifecycle[principal].phase
}

// isDraining reports whether the principal is in the draining phase.
func (d *Daemon) isDraining(principal ref.Entity) bool {
	return d.lifecycle[principal].phase == phaseDraining
}

// isCompleted reports whether the principal has completed and should
// not be restarted.
func (d *Daemon) isCompleted(principal ref.Entity) bool {
	return d.lifecycle[principal].phase == phaseCompleted
}

// startFailureFor returns the start failure record for a principal, or
// nil if the principal is not in the start-failed phase.
func (d *Daemon) startFailureFor(principal ref.Entity) *startFailure {
	lc := d.lifecycle[principal]
	if lc.phase != phaseStartFailed {
		return nil
	}
	return lc.startFailure
}

// --- Daemon aggregate methods ---

// aliveCount returns the number of principals with active sandboxes
// (running or draining).
func (d *Daemon) aliveCount() int {
	count := 0
	for _, lc := range d.lifecycle {
		if lc.isAlive() {
			count++
		}
	}
	return count
}

// alivePrincipals returns a snapshot of all principals with active
// sandboxes (running or draining). The returned slice is safe to use
// after releasing reconcileMu.
func (d *Daemon) alivePrincipals() []ref.Entity {
	principals := make([]ref.Entity, 0, len(d.lifecycle))
	for principal, lc := range d.lifecycle {
		if lc.isAlive() {
			principals = append(principals, principal)
		}
	}
	return principals
}

// startFailureCount returns the number of principals in the start-failed
// phase.
func (d *Daemon) startFailureCount() int {
	count := 0
	for _, lc := range d.lifecycle {
		if lc.phase == phaseStartFailed {
			count++
		}
	}
	return count
}

// clearCompletedPrincipals removes all phaseCompleted entries from the
// lifecycle map, allowing those principals to be recreated on the next
// reconcile cycle. Called when config room state changes.
func (d *Daemon) clearCompletedPrincipals() {
	for principal, lc := range d.lifecycle {
		if lc.phase == phaseCompleted {
			delete(d.lifecycle, principal)
		}
	}
}
