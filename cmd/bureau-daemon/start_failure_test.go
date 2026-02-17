// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
	"time"
)

func TestRecordStartFailure_ExponentialBackoff(t *testing.T) {
	t.Parallel()

	daemon, fakeClock := newTestDaemon(t)
	localpart := "agent-alpha"

	// First failure: backoff should be 1s.
	daemon.recordStartFailure(localpart, failureCategoryServiceResolution, "no binding found")
	failure := daemon.startFailures[localpart]
	if failure == nil {
		t.Fatal("expected start failure to be recorded")
	}
	if failure.attempts != 1 {
		t.Errorf("attempts = %d, want 1", failure.attempts)
	}
	if failure.category != failureCategoryServiceResolution {
		t.Errorf("category = %q, want %q", failure.category, failureCategoryServiceResolution)
	}
	expectedRetryAt := testDaemonEpoch.Add(1 * time.Second)
	if !failure.nextRetryAt.Equal(expectedRetryAt) {
		t.Errorf("nextRetryAt = %v, want %v", failure.nextRetryAt, expectedRetryAt)
	}

	// Second failure: backoff should be 2s.
	fakeClock.Advance(2 * time.Second)
	daemon.recordStartFailure(localpart, failureCategoryServiceResolution, "still no binding")
	failure = daemon.startFailures[localpart]
	if failure.attempts != 2 {
		t.Errorf("attempts = %d, want 2", failure.attempts)
	}
	expectedRetryAt = testDaemonEpoch.Add(2*time.Second + 2*time.Second)
	if !failure.nextRetryAt.Equal(expectedRetryAt) {
		t.Errorf("nextRetryAt = %v, want %v", failure.nextRetryAt, expectedRetryAt)
	}

	// Third failure: backoff should be 4s.
	fakeClock.Advance(3 * time.Second)
	daemon.recordStartFailure(localpart, failureCategoryServiceResolution, "still no binding")
	failure = daemon.startFailures[localpart]
	if failure.attempts != 3 {
		t.Errorf("attempts = %d, want 3", failure.attempts)
	}
	expectedRetryAt = testDaemonEpoch.Add(5*time.Second + 4*time.Second)
	if !failure.nextRetryAt.Equal(expectedRetryAt) {
		t.Errorf("nextRetryAt = %v, want %v", failure.nextRetryAt, expectedRetryAt)
	}
}

func TestRecordStartFailure_BackoffCap(t *testing.T) {
	t.Parallel()

	daemon, fakeClock := newTestDaemon(t)
	localpart := "agent-beta"

	// Drive through enough failures to hit the cap.
	// Backoffs: 1s, 2s, 4s, 8s, 16s, 30s (cap), 30s ...
	for range 9 {
		daemon.recordStartFailure(localpart, failureCategoryTemplate, "template not found")
		fakeClock.Advance(1 * time.Minute) // advance past any backoff
	}

	// 10th failure: capture the time before recording so we can
	// compute the expected nextRetryAt.
	timeAtFailure := fakeClock.Now()
	daemon.recordStartFailure(localpart, failureCategoryTemplate, "template not found")

	failure := daemon.startFailures[localpart]
	if failure.attempts != 10 {
		t.Errorf("attempts = %d, want 10", failure.attempts)
	}

	// The backoff at attempt 10 should be capped at 30s.
	expectedRetryAt := timeAtFailure.Add(startFailureBackoffCap)
	if !failure.nextRetryAt.Equal(expectedRetryAt) {
		t.Errorf("nextRetryAt = %v, want %v (capped at %v)", failure.nextRetryAt, expectedRetryAt, startFailureBackoffCap)
	}
}

func TestClearStartFailures(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)

	daemon.recordStartFailure("agent-1", failureCategoryCredentials, "missing")
	daemon.recordStartFailure("agent-2", failureCategoryServiceResolution, "no binding")
	daemon.recordStartFailure("agent-3", failureCategoryTemplate, "not found")

	if count := len(daemon.startFailures); count != 3 {
		t.Fatalf("expected 3 failures, got %d", count)
	}

	daemon.clearStartFailures()

	if count := len(daemon.startFailures); count != 0 {
		t.Errorf("expected 0 failures after clear, got %d", count)
	}
}

func TestClearStartFailuresByCategory(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)

	daemon.recordStartFailure("agent-1", failureCategoryServiceResolution, "no binding for ticket")
	daemon.recordStartFailure("agent-2", failureCategoryServiceResolution, "no binding for stt")
	daemon.recordStartFailure("agent-3", failureCategoryCredentials, "missing credentials")
	daemon.recordStartFailure("agent-4", failureCategoryTemplate, "template not found")

	cleared := daemon.clearStartFailuresByCategory(failureCategoryServiceResolution)
	if cleared != 2 {
		t.Errorf("cleared = %d, want 2", cleared)
	}

	if count := len(daemon.startFailures); count != 2 {
		t.Errorf("expected 2 remaining failures, got %d", count)
	}

	// Verify the right ones remain.
	if daemon.startFailures["agent-3"] == nil {
		t.Error("agent-3 (credentials) should still have a failure entry")
	}
	if daemon.startFailures["agent-4"] == nil {
		t.Error("agent-4 (template) should still have a failure entry")
	}
}

func TestClearStartFailure_SinglePrincipal(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)

	daemon.recordStartFailure("agent-1", failureCategoryCredentials, "missing")
	daemon.recordStartFailure("agent-2", failureCategoryTemplate, "not found")

	daemon.clearStartFailure("agent-1")

	if daemon.startFailures["agent-1"] != nil {
		t.Error("agent-1 should have been cleared")
	}
	if daemon.startFailures["agent-2"] == nil {
		t.Error("agent-2 should still have a failure entry")
	}
}

func TestStartFailure_BackoffCheckInReconcileLoop(t *testing.T) {
	t.Parallel()

	daemon, fakeClock := newTestDaemon(t)

	// Record a failure with 1s backoff.
	daemon.recordStartFailure("agent-1", failureCategoryServiceResolution, "no binding")

	// Before backoff expires: clock.Now() < nextRetryAt.
	failure := daemon.startFailures["agent-1"]
	if !fakeClock.Now().Before(failure.nextRetryAt) {
		t.Fatal("expected current time to be before nextRetryAt")
	}

	// Advance past the backoff.
	fakeClock.Advance(2 * time.Second)
	if fakeClock.Now().Before(failure.nextRetryAt) {
		t.Fatal("expected current time to be after nextRetryAt")
	}
}

func TestRecordStartFailure_CategoryChange(t *testing.T) {
	t.Parallel()

	daemon, fakeClock := newTestDaemon(t)
	localpart := "agent-gamma"

	// First failure is service resolution.
	daemon.recordStartFailure(localpart, failureCategoryServiceResolution, "no binding")
	if daemon.startFailures[localpart].category != failureCategoryServiceResolution {
		t.Error("expected service_resolution category")
	}
	if daemon.startFailures[localpart].attempts != 1 {
		t.Error("expected 1 attempt")
	}

	fakeClock.Advance(2 * time.Second)

	// Second failure is a different category (e.g., the service appeared
	// but now tokens can't be minted). The attempt count still
	// increments because it's the same principal — the backoff tracks
	// the principal, not the category. This prevents a principal from
	// resetting its backoff by failing at a different stage.
	daemon.recordStartFailure(localpart, failureCategoryTokenMinting, "signing key error")
	if daemon.startFailures[localpart].category != failureCategoryTokenMinting {
		t.Error("expected token_minting category")
	}
	if daemon.startFailures[localpart].attempts != 2 {
		t.Errorf("attempts = %d, want 2", daemon.startFailures[localpart].attempts)
	}
}
