// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
)

// fakeShipper records Ship calls and returns configurable errors.
// The called channel signals after every Ship invocation so tests
// can synchronize without polling.
type fakeShipper struct {
	mu       sync.Mutex
	calls    [][]byte
	errorSeq []error // errors to return in order; nil entries mean success
	index    int
	called   chan struct{} // signaled after each Ship call
}

func newFakeShipper(errorSeq []error, expectedCalls int) *fakeShipper {
	return &fakeShipper{
		errorSeq: errorSeq,
		called:   make(chan struct{}, expectedCalls),
	}
}

func (f *fakeShipper) Ship(_ context.Context, data []byte) error {
	f.mu.Lock()
	copied := make([]byte, len(data))
	copy(copied, data)
	f.calls = append(f.calls, copied)
	var err error
	if f.index < len(f.errorSeq) {
		err = f.errorSeq[f.index]
		f.index++
	}
	f.mu.Unlock()

	// Signal after releasing the lock so tests waiting on called
	// can read callCount without deadlocking.
	if f.called != nil {
		f.called <- struct{}{}
	}

	return err
}

func (f *fakeShipper) Close() {}

func (f *fakeShipper) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// waitForCalls blocks until the shipper has been called n times.
func (f *fakeShipper) waitForCalls(t *testing.T, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		<-f.called
	}
}

func TestShipperSuccessfulDrain(t *testing.T) {
	buffer := NewBuffer(4096)
	for i := byte(0); i < 5; i++ {
		if err := buffer.Push([]byte{i}); err != nil {
			t.Fatalf("Push: %v", err)
		}
	}

	shipper := newFakeShipper(nil, 5)
	var shipped atomic.Uint64
	logger := slog.Default()
	fakeClock := clock.Fake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		runShipper(ctx, buffer, shipper, fakeClock, &shipped, logger)
		close(done)
	}()

	// Wait for all 5 entries to be shipped. The initial Push calls
	// signaled the notify channel, so the shipper wakes up and
	// drains the buffer in a tight loop.
	shipper.waitForCalls(t, 5)

	cancel()
	<-done

	if shipped.Load() != 5 {
		t.Fatalf("expected 5 shipped, got %d", shipped.Load())
	}
	if shipper.callCount() != 5 {
		t.Fatalf("expected 5 ship calls, got %d", shipper.callCount())
	}
}

func TestShipperRetryOnFailure(t *testing.T) {
	buffer := NewBuffer(4096)
	if err := buffer.Push([]byte{1}); err != nil {
		t.Fatalf("Push: %v", err)
	}

	// Fail twice, then succeed.
	retryError := errors.New("temporary failure")
	shipper := newFakeShipper([]error{retryError, retryError, nil}, 3)
	var shipped atomic.Uint64
	logger := slog.Default()
	fakeClock := clock.Fake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		runShipper(ctx, buffer, shipper, fakeClock, &shipped, logger)
		close(done)
	}()

	// 1st call fails → shipper enters 1s backoff.
	shipper.waitForCalls(t, 1)
	fakeClock.WaitForTimers(1)
	fakeClock.Advance(1 * time.Second)

	// 2nd call fails → shipper enters 2s backoff.
	shipper.waitForCalls(t, 1)
	fakeClock.WaitForTimers(1)
	fakeClock.Advance(2 * time.Second)

	// 3rd call succeeds.
	shipper.waitForCalls(t, 1)

	cancel()
	<-done

	if shipped.Load() != 1 {
		t.Fatalf("expected 1 shipped, got %d", shipped.Load())
	}
	if shipper.callCount() != 3 {
		t.Fatalf("expected 3 ship calls, got %d", shipper.callCount())
	}
}

func TestShipperContextCancellation(t *testing.T) {
	buffer := NewBuffer(4096)
	shipper := newFakeShipper(nil, 0)
	var shipped atomic.Uint64
	logger := slog.Default()
	fakeClock := clock.Fake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		runShipper(ctx, buffer, shipper, fakeClock, &shipped, logger)
		close(done)
	}()

	// Cancel immediately — the shipper sees ctx.Done() and returns.
	cancel()
	<-done
}

func TestShipperDrainOnShutdown(t *testing.T) {
	buffer := NewBuffer(4096)
	for i := byte(0); i < 3; i++ {
		if err := buffer.Push([]byte{i}); err != nil {
			t.Fatalf("Push: %v", err)
		}
	}

	// First call fails (triggering backoff), then the context is
	// cancelled during the backoff, and the drain pass ships all 3
	// entries (the first entry is retried since it was Peek'd but
	// not Pop'd).
	shipper := newFakeShipper([]error{errors.New("fail"), nil, nil, nil}, 4)
	var shipped atomic.Uint64
	logger := slog.Default()
	fakeClock := clock.Fake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		runShipper(ctx, buffer, shipper, fakeClock, &shipped, logger)
		close(done)
	}()

	// Wait for the 1st call (fails) and for the backoff timer to
	// be registered.
	shipper.waitForCalls(t, 1)
	fakeClock.WaitForTimers(1)

	// Cancel while the shipper is in its backoff sleep. The drain
	// pass should ship all 3 entries.
	cancel()

	// Wait for the 3 drain calls.
	shipper.waitForCalls(t, 3)
	<-done

	if shipped.Load() != 3 {
		t.Fatalf("expected 3 shipped during drain, got %d", shipped.Load())
	}
}

func TestShipperBackoffCap(t *testing.T) {
	buffer := NewBuffer(4096)
	if err := buffer.Push([]byte{1}); err != nil {
		t.Fatalf("Push: %v", err)
	}

	// Fail 8 times to verify the exponential backoff reaches the
	// 30s cap and stays there, then succeed.
	//
	// Expected backoff sequence after each failure:
	//   1s → 2s → 4s → 8s → 16s → 30s(cap) → 30s → 30s
	failError := errors.New("keep failing")
	shipper := newFakeShipper([]error{
		failError, failError, failError, failError,
		failError, failError, failError, failError,
		nil, // 9th attempt succeeds
	}, 9)
	var shipped atomic.Uint64
	logger := slog.Default()
	fakeClock := clock.Fake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		runShipper(ctx, buffer, shipper, fakeClock, &shipped, logger)
		close(done)
	}()

	// Advance through all 8 backoff periods. After each advance the
	// shipper retries; the first 8 attempts fail, the 9th succeeds.
	expectedBackoffs := []time.Duration{
		1 * time.Second,  // after failure 1
		2 * time.Second,  // after failure 2
		4 * time.Second,  // after failure 3
		8 * time.Second,  // after failure 4
		16 * time.Second, // after failure 5
		30 * time.Second, // after failure 6 (would be 32s, capped)
		30 * time.Second, // after failure 7 (still capped)
		30 * time.Second, // after failure 8 (still capped)
	}

	for _, backoff := range expectedBackoffs {
		shipper.waitForCalls(t, 1)
		fakeClock.WaitForTimers(1)
		fakeClock.Advance(backoff)
	}

	// Wait for the 9th (successful) call.
	shipper.waitForCalls(t, 1)

	cancel()
	<-done

	if shipped.Load() != 1 {
		t.Fatalf("expected 1 shipped, got %d", shipped.Load())
	}
	// 8 failures + 1 success = 9 total calls.
	if shipper.callCount() != 9 {
		t.Fatalf("expected 9 ship calls, got %d", shipper.callCount())
	}
}
