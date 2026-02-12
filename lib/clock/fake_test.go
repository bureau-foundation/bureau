// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package clock

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestFakeClockNow(t *testing.T) {
	clock := Fake(epoch)
	if got := clock.Now(); !got.Equal(epoch) {
		t.Fatalf("Now() = %v, want %v", got, epoch)
	}
	clock.Advance(5 * time.Second)
	want := epoch.Add(5 * time.Second)
	if got := clock.Now(); !got.Equal(want) {
		t.Fatalf("Now() after Advance = %v, want %v", got, want)
	}
}

func TestFakeClockAfterFiresOnAdvance(t *testing.T) {
	clock := Fake(epoch)
	channel := clock.After(3 * time.Second)

	// Should not fire yet.
	select {
	case <-channel:
		t.Fatal("After fired before Advance")
	default:
	}

	// Advance past the deadline.
	clock.Advance(3 * time.Second)

	select {
	case <-channel:
	default:
		t.Fatal("After did not fire after Advance")
	}
}

func TestFakeClockAfterZeroDuration(t *testing.T) {
	clock := Fake(epoch)
	channel := clock.After(0)

	select {
	case <-channel:
	default:
		t.Fatal("After(0) should fire immediately")
	}
}

func TestFakeClockAfterNegativeDuration(t *testing.T) {
	clock := Fake(epoch)
	channel := clock.After(-1 * time.Second)

	select {
	case <-channel:
	default:
		t.Fatal("After(-1s) should fire immediately")
	}
}

func TestFakeClockAfterPartialAdvance(t *testing.T) {
	clock := Fake(epoch)
	channel := clock.After(5 * time.Second)

	clock.Advance(3 * time.Second)
	select {
	case <-channel:
		t.Fatal("After fired before deadline")
	default:
	}

	clock.Advance(2 * time.Second)
	select {
	case <-channel:
	default:
		t.Fatal("After did not fire at exact deadline")
	}
}

func TestFakeClockAfterFuncInvokesCallback(t *testing.T) {
	clock := Fake(epoch)
	var called atomic.Bool
	clock.AfterFunc(2*time.Second, func() {
		called.Store(true)
	})

	clock.Advance(1 * time.Second)
	if called.Load() {
		t.Fatal("AfterFunc fired before deadline")
	}

	clock.Advance(1 * time.Second)
	if !called.Load() {
		t.Fatal("AfterFunc did not fire at deadline")
	}
}

func TestFakeClockAfterFuncZeroDuration(t *testing.T) {
	clock := Fake(epoch)
	var called atomic.Bool
	clock.AfterFunc(0, func() {
		called.Store(true)
	})

	if !called.Load() {
		t.Fatal("AfterFunc(0) should call f synchronously")
	}
}

func TestFakeClockAfterFuncStop(t *testing.T) {
	clock := Fake(epoch)
	var called atomic.Bool
	timer := clock.AfterFunc(2*time.Second, func() {
		called.Store(true)
	})

	stopped := timer.Stop()
	if !stopped {
		t.Fatal("Stop() should return true for unfired timer")
	}

	clock.Advance(5 * time.Second)
	if called.Load() {
		t.Fatal("callback invoked after Stop()")
	}
}

func TestFakeClockAfterFuncStopAlreadyFired(t *testing.T) {
	clock := Fake(epoch)
	timer := clock.AfterFunc(1*time.Second, func() {})

	clock.Advance(1 * time.Second)

	stopped := timer.Stop()
	if stopped {
		t.Fatal("Stop() should return false for already-fired timer")
	}
}

func TestFakeClockAfterFuncStopTwice(t *testing.T) {
	clock := Fake(epoch)
	timer := clock.AfterFunc(1*time.Second, func() {})

	if !timer.Stop() {
		t.Fatal("first Stop() should return true")
	}
	if timer.Stop() {
		t.Fatal("second Stop() should return false")
	}
}

func TestFakeClockAfterFuncReset(t *testing.T) {
	clock := Fake(epoch)
	var called atomic.Bool
	timer := clock.AfterFunc(5*time.Second, func() {
		called.Store(true)
	})

	// Reset to fire sooner.
	wasActive := timer.Reset(2 * time.Second)
	if !wasActive {
		t.Fatal("Reset() should return true for active timer")
	}

	clock.Advance(2 * time.Second)
	if !called.Load() {
		t.Fatal("callback should fire at new deadline after Reset")
	}
}

func TestFakeClockNewTicker(t *testing.T) {
	clock := Fake(epoch)
	ticker := clock.NewTicker(1 * time.Second)
	defer ticker.Stop()

	// No tick yet.
	select {
	case <-ticker.C:
		t.Fatal("ticker fired before first interval")
	default:
	}

	// First tick.
	clock.Advance(1 * time.Second)
	select {
	case <-ticker.C:
	default:
		t.Fatal("ticker did not fire after first interval")
	}

	// Second tick.
	clock.Advance(1 * time.Second)
	select {
	case <-ticker.C:
	default:
		t.Fatal("ticker did not fire after second interval")
	}
}

func TestFakeClockTickerStop(t *testing.T) {
	clock := Fake(epoch)
	ticker := clock.NewTicker(1 * time.Second)

	ticker.Stop()
	clock.Advance(5 * time.Second)

	select {
	case <-ticker.C:
		t.Fatal("ticker fired after Stop()")
	default:
	}
}

func TestFakeClockTickerReset(t *testing.T) {
	clock := Fake(epoch)
	ticker := clock.NewTicker(5 * time.Second)
	defer ticker.Stop()

	// Reset to shorter interval.
	ticker.Reset(1 * time.Second)

	clock.Advance(1 * time.Second)
	select {
	case <-ticker.C:
	default:
		t.Fatal("ticker did not fire after Reset to shorter interval")
	}
}

func TestFakeClockTickerPanicsOnNonPositive(t *testing.T) {
	clock := Fake(epoch)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewTicker(0) should panic")
		}
	}()
	clock.NewTicker(0)
}

func TestFakeClockSleep(t *testing.T) {
	clock := Fake(epoch)

	done := make(chan struct{})
	go func() {
		clock.Sleep(3 * time.Second)
		close(done)
	}()

	clock.WaitForTimers(1)
	clock.Advance(3 * time.Second)

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Sleep did not return after Advance")
	}
}

func TestFakeClockSleepZero(t *testing.T) {
	clock := Fake(epoch)
	// Should return immediately, no blocking.
	clock.Sleep(0)
}

func TestFakeClockSleepNegative(t *testing.T) {
	clock := Fake(epoch)
	// Should return immediately.
	clock.Sleep(-1 * time.Second)
}

func TestFakeClockWaitForTimers(t *testing.T) {
	clock := Fake(epoch)

	// Register timers from concurrent goroutines.
	for i := 0; i < 3; i++ {
		go func() {
			clock.Sleep(5 * time.Second)
		}()
	}

	// WaitForTimers blocks until all 3 are registered.
	clock.WaitForTimers(3)

	if got := clock.PendingCount(); got != 3 {
		t.Fatalf("PendingCount() = %d, want 3", got)
	}
}

func TestFakeClockMultipleTimersFireInOrder(t *testing.T) {
	clock := Fake(epoch)

	var order []int
	var mu sync.Mutex

	clock.AfterFunc(3*time.Second, func() {
		mu.Lock()
		order = append(order, 3)
		mu.Unlock()
	})
	clock.AfterFunc(1*time.Second, func() {
		mu.Lock()
		order = append(order, 1)
		mu.Unlock()
	})
	clock.AfterFunc(2*time.Second, func() {
		mu.Lock()
		order = append(order, 2)
		mu.Unlock()
	})

	clock.Advance(5 * time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 3 || order[0] != 1 || order[1] != 2 || order[2] != 3 {
		t.Fatalf("callbacks fired in wrong order: %v, want [1 2 3]", order)
	}
}

func TestFakeClockTickerDropsTicks(t *testing.T) {
	clock := Fake(epoch)
	ticker := clock.NewTicker(1 * time.Second)
	defer ticker.Stop()

	// Advance past multiple intervals without reading from C.
	// Channel buffer is 1, so at most 1 tick is buffered.
	clock.Advance(5 * time.Second)

	// Exactly one tick should be buffered.
	select {
	case <-ticker.C:
	default:
		t.Fatal("expected at least one buffered tick")
	}

	// No more ticks buffered (the rest were dropped).
	select {
	case <-ticker.C:
		t.Fatal("expected no more ticks (should have been dropped)")
	default:
	}
}

func TestFakeClockOneShotDoesNotRepeat(t *testing.T) {
	clock := Fake(epoch)
	var count atomic.Int32
	clock.AfterFunc(1*time.Second, func() {
		count.Add(1)
	})

	clock.Advance(1 * time.Second)
	clock.Advance(1 * time.Second)
	clock.Advance(1 * time.Second)

	if got := count.Load(); got != 1 {
		t.Fatalf("AfterFunc fired %d times, want 1", got)
	}
}

func TestFakeClockPendingCountExcludesStopped(t *testing.T) {
	clock := Fake(epoch)
	ticker := clock.NewTicker(1 * time.Second)
	clock.AfterFunc(2*time.Second, func() {})

	if got := clock.PendingCount(); got != 2 {
		t.Fatalf("PendingCount() = %d, want 2", got)
	}

	ticker.Stop()
	if got := clock.PendingCount(); got != 1 {
		t.Fatalf("PendingCount() after ticker stop = %d, want 1", got)
	}
}

func TestFakeClockPendingCountExcludesFired(t *testing.T) {
	clock := Fake(epoch)
	clock.After(1 * time.Second)
	clock.After(3 * time.Second)

	if got := clock.PendingCount(); got != 2 {
		t.Fatalf("PendingCount() = %d, want 2", got)
	}

	clock.Advance(2 * time.Second)
	if got := clock.PendingCount(); got != 1 {
		t.Fatalf("PendingCount() after first fires = %d, want 1", got)
	}
}

func TestFakeClockImplementsClock(t *testing.T) {
	// Compile-time check that *FakeClock satisfies Clock.
	var _ Clock = (*FakeClock)(nil)
}

func TestRealClockImplementsClock(t *testing.T) {
	// Compile-time check that realClock satisfies Clock.
	var _ Clock = Real()
}

func TestFakeClockConcurrentAccess(t *testing.T) {
	clock := Fake(epoch)
	const goroutines = 10

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			clock.After(1 * time.Second)
			clock.Now()
		}()
	}
	wg.Wait()

	clock.WaitForTimers(goroutines)
	clock.Advance(1 * time.Second)
}
