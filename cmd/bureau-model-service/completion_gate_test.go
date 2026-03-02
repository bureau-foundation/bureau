// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
)

func TestCompletionGateOpenOnMaxSize(t *testing.T) {
	fakeClock := clock.Fake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	logger := slog.Default()

	gate := NewCompletionGate(fakeClock, logger, 50*time.Millisecond, 3)
	defer gate.Close()

	key := completionGateKey{ProviderName: "p", ProviderModel: "m"}

	// Launch 3 waiters in goroutines.
	var waitGroup sync.WaitGroup
	errors := make([]error, 3)
	for i := range 3 {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			errors[index] = gate.Wait(context.Background(), key, 0)
		}(i)
	}

	// All 3 should complete (gate opens at maxSize=3).
	waitGroup.Wait()

	for i, err := range errors {
		if err != nil {
			t.Fatalf("waiter %d: unexpected error: %v", i, err)
		}
	}
}

func TestCompletionGateOpenOnTimer(t *testing.T) {
	fakeClock := clock.Fake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	logger := slog.Default()

	gate := NewCompletionGate(fakeClock, logger, 50*time.Millisecond, 64)
	defer gate.Close()

	key := completionGateKey{ProviderName: "p", ProviderModel: "m"}

	// Launch 2 waiters (less than max).
	var waitGroup sync.WaitGroup
	errors := make([]error, 2)
	for i := range 2 {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			errors[index] = gate.Wait(context.Background(), key, 0)
		}(i)
	}

	// Wait for both waiters to be queued. This is deterministic:
	// WaitForGateSize blocks on a condition variable that fires
	// when any waiter is added, so no scheduling races.
	gate.WaitForGateSize(key, 2)

	// Advance past flush delay.
	fakeClock.Advance(50 * time.Millisecond)

	// Both should complete.
	waitGroup.Wait()

	for i, err := range errors {
		if err != nil {
			t.Fatalf("waiter %d: unexpected error: %v", i, err)
		}
	}
}

func TestCompletionGateContextCancel(t *testing.T) {
	fakeClock := clock.Fake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	logger := slog.Default()

	gate := NewCompletionGate(fakeClock, logger, 50*time.Millisecond, 64)
	defer gate.Close()

	key := completionGateKey{ProviderName: "p", ProviderModel: "m"}

	ctx, cancel := context.WithCancel(context.Background())

	errChan := make(chan error, 1)
	go func() {
		errChan <- gate.Wait(ctx, key, 0)
	}()

	// Wait for the waiter to be queued.
	gate.WaitForGateSize(key, 1)

	// Cancel the waiter's context.
	cancel()

	select {
	case err := <-errChan:
		if err != context.Canceled {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-t.Context().Done():
		t.Fatal("timed out waiting for cancelled waiter")
	}
}

func TestCompletionGateIndependentKeys(t *testing.T) {
	fakeClock := clock.Fake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	logger := slog.Default()

	gate := NewCompletionGate(fakeClock, logger, 50*time.Millisecond, 2)
	defer gate.Close()

	keyA := completionGateKey{ProviderName: "p", ProviderModel: "modelA"}
	keyB := completionGateKey{ProviderName: "p", ProviderModel: "modelB"}

	// Submit 1 waiter to key A — won't reach maxSize alone.
	errChanA := make(chan error, 1)
	go func() {
		errChanA <- gate.Wait(context.Background(), keyA, 0)
	}()

	// Submit 2 waiters to key B — should open immediately (maxSize=2).
	var waitGroupB sync.WaitGroup
	errorsB := make([]error, 2)
	for i := range 2 {
		waitGroupB.Add(1)
		go func(index int) {
			defer waitGroupB.Done()
			errorsB[index] = gate.Wait(context.Background(), keyB, 0)
		}(i)
	}

	// Key B should open immediately (hit maxSize).
	waitGroupB.Wait()
	for i, err := range errorsB {
		if err != nil {
			t.Fatalf("B waiter %d: unexpected error: %v", i, err)
		}
	}

	// Key A is still pending. Wait for its waiter and advance the timer.
	gate.WaitForGateSize(keyA, 1)
	fakeClock.Advance(50 * time.Millisecond)

	select {
	case err := <-errChanA:
		if err != nil {
			t.Fatalf("A waiter: unexpected error: %v", err)
		}
	case <-t.Context().Done():
		t.Fatal("A waiter: timed out")
	}
}

func TestCompletionGateClose(t *testing.T) {
	fakeClock := clock.Fake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	logger := slog.Default()

	gate := NewCompletionGate(fakeClock, logger, 50*time.Millisecond, 64)

	key := completionGateKey{ProviderName: "p", ProviderModel: "m"}

	errChan := make(chan error, 1)
	go func() {
		errChan <- gate.Wait(context.Background(), key, 0)
	}()

	// Wait for the waiter to be queued.
	gate.WaitForGateSize(key, 1)

	// Close the gate — should release the waiter.
	gate.Close()

	select {
	case err := <-errChan:
		// Released by Close — should return nil (channel was closed).
		if err != nil {
			t.Fatalf("expected nil error from close, got %v", err)
		}
	case <-t.Context().Done():
		t.Fatal("timed out waiting for waiter after Close")
	}

	// Wait after close returns error immediately.
	err := gate.Wait(context.Background(), key, 0)
	if err == nil {
		t.Fatal("expected error from Wait after Close")
	}
}
