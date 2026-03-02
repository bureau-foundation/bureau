// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"runtime"
	"testing"
)

func TestBackgroundSchedulerIdleImmediately(t *testing.T) {
	scheduler := NewBackgroundScheduler(slog.Default())
	defer scheduler.Close()

	// No active requests — should return immediately.
	err := scheduler.WaitForIdle(context.Background(), "provider-a")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestBackgroundSchedulerWaitsForActive(t *testing.T) {
	scheduler := NewBackgroundScheduler(slog.Default())
	defer scheduler.Close()

	scheduler.RecordStart("provider-a")

	// WaitForIdle should block until RecordEnd is called.
	proceedChan := make(chan error, 1)
	go func() {
		proceedChan <- scheduler.WaitForIdle(context.Background(), "provider-a")
	}()

	// Wait for the goroutine to enter WaitForIdle.
	scheduler.WaitForWaiters("provider-a", 1)

	// Verify still blocked.
	select {
	case <-proceedChan:
		t.Fatal("background request should be blocked while provider is active")
	default:
	}

	// End the active request.
	scheduler.RecordEnd("provider-a")

	// The background request should now proceed.
	select {
	case err := <-proceedChan:
		if err != nil {
			t.Fatalf("expected nil error, got %v", err)
		}
	case <-t.Context().Done():
		t.Fatal("timed out waiting for background request to proceed")
	}
}

func TestBackgroundSchedulerContextCancel(t *testing.T) {
	scheduler := NewBackgroundScheduler(slog.Default())
	defer scheduler.Close()

	scheduler.RecordStart("provider-a")

	ctx, cancel := context.WithCancel(context.Background())
	errChan := make(chan error, 1)
	go func() {
		errChan <- scheduler.WaitForIdle(ctx, "provider-a")
	}()

	// Wait for the goroutine to enter WaitForIdle.
	scheduler.WaitForWaiters("provider-a", 1)

	cancel()

	select {
	case err := <-errChan:
		if err != context.Canceled {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-t.Context().Done():
		t.Fatal("timed out waiting for cancelled waiter")
	}

	// Clean up.
	scheduler.RecordEnd("provider-a")
}

func TestBackgroundSchedulerMultipleProviders(t *testing.T) {
	scheduler := NewBackgroundScheduler(slog.Default())
	defer scheduler.Close()

	// Provider A is active, provider B is idle.
	scheduler.RecordStart("provider-a")

	// Background request for provider B should proceed immediately.
	err := scheduler.WaitForIdle(context.Background(), "provider-b")
	if err != nil {
		t.Fatalf("provider-b should be idle, got error: %v", err)
	}

	// Background request for provider A should block.
	proceedChan := make(chan struct{})
	go func() {
		_ = scheduler.WaitForIdle(context.Background(), "provider-a")
		close(proceedChan)
	}()

	// Wait for the goroutine to enter WaitForIdle.
	scheduler.WaitForWaiters("provider-a", 1)

	// Verify still blocked.
	select {
	case <-proceedChan:
		t.Fatal("provider-a background should be blocked")
	default:
	}

	scheduler.RecordEnd("provider-a")

	select {
	case <-proceedChan:
		// OK
	case <-t.Context().Done():
		t.Fatal("timed out waiting for provider-a background")
	}
}

func TestBackgroundSchedulerMultipleActiveRequests(t *testing.T) {
	scheduler := NewBackgroundScheduler(slog.Default())
	defer scheduler.Close()

	scheduler.RecordStart("provider-a")
	scheduler.RecordStart("provider-a")

	proceedChan := make(chan struct{})
	go func() {
		_ = scheduler.WaitForIdle(context.Background(), "provider-a")
		close(proceedChan)
	}()

	// Wait for the goroutine to enter WaitForIdle.
	scheduler.WaitForWaiters("provider-a", 1)

	// End one request — still one active.
	scheduler.RecordEnd("provider-a")
	// Give the goroutine a chance to wake and re-check.
	runtime.Gosched()

	select {
	case <-proceedChan:
		t.Fatal("should still be blocked (1 active request remaining)")
	default:
	}

	// End the second request — now idle.
	scheduler.RecordEnd("provider-a")

	select {
	case <-proceedChan:
		// OK
	case <-t.Context().Done():
		t.Fatal("timed out waiting for background to proceed")
	}
}
