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
	principal := testEntity(t, daemon.fleet, "agent/alpha")

	// First failure: backoff should be 1s.
	daemon.recordStartFailure(principal, failureCategoryServiceResolution, "no binding found")
	failure := daemon.startFailures[principal]
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
	daemon.recordStartFailure(principal, failureCategoryServiceResolution, "still no binding")
	failure = daemon.startFailures[principal]
	if failure.attempts != 2 {
		t.Errorf("attempts = %d, want 2", failure.attempts)
	}
	expectedRetryAt = testDaemonEpoch.Add(2*time.Second + 2*time.Second)
	if !failure.nextRetryAt.Equal(expectedRetryAt) {
		t.Errorf("nextRetryAt = %v, want %v", failure.nextRetryAt, expectedRetryAt)
	}

	// Third failure: backoff should be 4s.
	fakeClock.Advance(3 * time.Second)
	daemon.recordStartFailure(principal, failureCategoryServiceResolution, "still no binding")
	failure = daemon.startFailures[principal]
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
	principal := testEntity(t, daemon.fleet, "agent/beta")

	// Drive through enough failures to hit the cap.
	// Backoffs: 1s, 2s, 4s, 8s, 16s, 30s (cap), 30s ...
	for range 9 {
		daemon.recordStartFailure(principal, failureCategoryTemplate, "template not found")
		fakeClock.Advance(1 * time.Minute) // advance past any backoff
	}

	// 10th failure: capture the time before recording so we can
	// compute the expected nextRetryAt.
	timeAtFailure := fakeClock.Now()
	daemon.recordStartFailure(principal, failureCategoryTemplate, "template not found")

	failure := daemon.startFailures[principal]
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

	daemon.recordStartFailure(testEntity(t, daemon.fleet, "agent/one"), failureCategoryCredentials, "missing")
	daemon.recordStartFailure(testEntity(t, daemon.fleet, "agent/two"), failureCategoryServiceResolution, "no binding")
	daemon.recordStartFailure(testEntity(t, daemon.fleet, "agent/three"), failureCategoryTemplate, "not found")

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

	agent3 := testEntity(t, daemon.fleet, "agent/three")
	agent4 := testEntity(t, daemon.fleet, "agent/four")

	daemon.recordStartFailure(testEntity(t, daemon.fleet, "agent/one"), failureCategoryServiceResolution, "no binding for ticket")
	daemon.recordStartFailure(testEntity(t, daemon.fleet, "agent/two"), failureCategoryServiceResolution, "no binding for stt")
	daemon.recordStartFailure(agent3, failureCategoryCredentials, "missing credentials")
	daemon.recordStartFailure(agent4, failureCategoryTemplate, "template not found")

	cleared := daemon.clearStartFailuresByCategory(failureCategoryServiceResolution)
	if cleared != 2 {
		t.Errorf("cleared = %d, want 2", cleared)
	}

	if count := len(daemon.startFailures); count != 2 {
		t.Errorf("expected 2 remaining failures, got %d", count)
	}

	// Verify the right ones remain.
	if daemon.startFailures[agent3] == nil {
		t.Error("agent/three (credentials) should still have a failure entry")
	}
	if daemon.startFailures[agent4] == nil {
		t.Error("agent/four (template) should still have a failure entry")
	}
}

func TestClearStartFailure_SinglePrincipal(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)

	agent1 := testEntity(t, daemon.fleet, "agent/one")
	agent2 := testEntity(t, daemon.fleet, "agent/two")

	daemon.recordStartFailure(agent1, failureCategoryCredentials, "missing")
	daemon.recordStartFailure(agent2, failureCategoryTemplate, "not found")

	daemon.clearStartFailure(agent1)

	if daemon.startFailures[agent1] != nil {
		t.Error("agent/one should have been cleared")
	}
	if daemon.startFailures[agent2] == nil {
		t.Error("agent/two should still have a failure entry")
	}
}

func TestStartFailure_BackoffCheckInReconcileLoop(t *testing.T) {
	t.Parallel()

	daemon, fakeClock := newTestDaemon(t)

	principal := testEntity(t, daemon.fleet, "agent/one")

	// Record a failure with 1s backoff.
	daemon.recordStartFailure(principal, failureCategoryServiceResolution, "no binding")

	// Before backoff expires: clock.Now() < nextRetryAt.
	failure := daemon.startFailures[principal]
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
	principal := testEntity(t, daemon.fleet, "agent/gamma")

	// First failure is service resolution.
	daemon.recordStartFailure(principal, failureCategoryServiceResolution, "no binding")
	if daemon.startFailures[principal].category != failureCategoryServiceResolution {
		t.Error("expected service_resolution category")
	}
	if daemon.startFailures[principal].attempts != 1 {
		t.Error("expected 1 attempt")
	}

	fakeClock.Advance(2 * time.Second)

	// Second failure is a different category (e.g., the service appeared
	// but now tokens can't be minted). The attempt count still
	// increments because it's the same principal — the backoff tracks
	// the principal, not the category. This prevents a principal from
	// resetting its backoff by failing at a different stage.
	daemon.recordStartFailure(principal, failureCategoryTokenMinting, "signing key error")
	if daemon.startFailures[principal].category != failureCategoryTokenMinting {
		t.Error("expected token_minting category")
	}
	if daemon.startFailures[principal].attempts != 2 {
		t.Errorf("attempts = %d, want 2", daemon.startFailures[principal].attempts)
	}
}

func TestSandboxCrashBackoff(t *testing.T) {
	t.Parallel()

	daemon, fakeClock := newTestDaemon(t)
	principal := testEntity(t, daemon.fleet, "agent/crashy")

	// First crash: 1s backoff.
	daemon.recordStartFailure(principal, failureCategorySandboxCrash, "sandbox command exited with code 1")
	failure := daemon.startFailures[principal]
	if failure == nil {
		t.Fatal("expected crash failure to be recorded")
	}
	if failure.category != failureCategorySandboxCrash {
		t.Errorf("category = %q, want %q", failure.category, failureCategorySandboxCrash)
	}
	if failure.attempts != 1 {
		t.Errorf("attempts = %d, want 1", failure.attempts)
	}
	expectedRetryAt := testDaemonEpoch.Add(1 * time.Second)
	if !failure.nextRetryAt.Equal(expectedRetryAt) {
		t.Errorf("nextRetryAt = %v, want %v", failure.nextRetryAt, expectedRetryAt)
	}

	// Backoff not yet expired: reconcile should skip.
	if !fakeClock.Now().Before(failure.nextRetryAt) {
		t.Fatal("expected current time to be before nextRetryAt")
	}

	// Second crash after first backoff expires: 2s backoff.
	fakeClock.Advance(2 * time.Second)
	daemon.recordStartFailure(principal, failureCategorySandboxCrash, "sandbox command exited with code 1")
	failure = daemon.startFailures[principal]
	if failure.attempts != 2 {
		t.Errorf("attempts = %d, want 2", failure.attempts)
	}
	expectedRetryAt = testDaemonEpoch.Add(2*time.Second + 2*time.Second)
	if !failure.nextRetryAt.Equal(expectedRetryAt) {
		t.Errorf("nextRetryAt = %v, want %v", failure.nextRetryAt, expectedRetryAt)
	}

	// Third crash: 4s backoff.
	fakeClock.Advance(3 * time.Second)
	daemon.recordStartFailure(principal, failureCategorySandboxCrash, "sandbox command exited with code 1")
	failure = daemon.startFailures[principal]
	if failure.attempts != 3 {
		t.Errorf("attempts = %d, want 3", failure.attempts)
	}
	expectedRetryAt = testDaemonEpoch.Add(5*time.Second + 4*time.Second)
	if !failure.nextRetryAt.Equal(expectedRetryAt) {
		t.Errorf("nextRetryAt = %v, want %v", failure.nextRetryAt, expectedRetryAt)
	}
}

func TestSandboxCrashBackoff_ClearedByConfigChange(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	principal := testEntity(t, daemon.fleet, "agent/fixable")

	// Record several crashes to build up backoff.
	for range 5 {
		daemon.recordStartFailure(principal, failureCategorySandboxCrash, "exited with code 1")
	}
	if daemon.startFailures[principal].attempts != 5 {
		t.Fatalf("attempts = %d, want 5", daemon.startFailures[principal].attempts)
	}

	// A config change clears all start failures (including crash
	// backoff). This is correct: if someone fixes the template or
	// credentials, the principal should retry immediately.
	daemon.clearStartFailures()

	if daemon.startFailures[principal] != nil {
		t.Error("expected crash failure to be cleared by config change")
	}
}

func TestProxyCrashBackoff(t *testing.T) {
	t.Parallel()

	daemon, fakeClock := newTestDaemon(t)
	principal := testEntity(t, daemon.fleet, "agent/proxy-crash")

	// First proxy crash: 1s backoff.
	daemon.recordStartFailure(principal, failureCategoryProxyCrash, "proxy exited with code 1")
	failure := daemon.startFailures[principal]
	if failure == nil {
		t.Fatal("expected proxy crash failure to be recorded")
	}
	if failure.category != failureCategoryProxyCrash {
		t.Errorf("category = %q, want %q", failure.category, failureCategoryProxyCrash)
	}
	if failure.attempts != 1 {
		t.Errorf("attempts = %d, want 1", failure.attempts)
	}

	// Crash backoff accumulates across proxy crashes like any other failure.
	fakeClock.Advance(2 * time.Second)
	daemon.recordStartFailure(principal, failureCategoryProxyCrash, "proxy exited with code 1")
	if daemon.startFailures[principal].attempts != 2 {
		t.Errorf("attempts = %d, want 2", daemon.startFailures[principal].attempts)
	}
}

func TestSandboxNormalExit_ClearsCrashBackoff(t *testing.T) {
	t.Parallel()

	daemon, fakeClock := newTestDaemon(t)
	principal := testEntity(t, daemon.fleet, "agent/one-shot")

	// Build up crash backoff from previous failures.
	daemon.recordStartFailure(principal, failureCategorySandboxCrash, "exited with code 1")
	fakeClock.Advance(2 * time.Second)
	daemon.recordStartFailure(principal, failureCategorySandboxCrash, "exited with code 1")
	if daemon.startFailures[principal].attempts != 2 {
		t.Fatalf("attempts = %d, want 2", daemon.startFailures[principal].attempts)
	}

	// A normal exit (code 0) clears the crash backoff. One-shot
	// principals (setup/teardown) should be able to re-evaluate
	// conditions immediately after completing.
	daemon.clearStartFailure(principal)

	if daemon.startFailures[principal] != nil {
		t.Error("expected crash backoff to be cleared by normal exit")
	}
}
