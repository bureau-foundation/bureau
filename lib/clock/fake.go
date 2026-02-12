// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package clock

import (
	"sort"
	"sync"
	"time"
)

// Fake returns a FakeClock initialized to the given time. Time stands
// still until Advance is called. All timer, ticker, and sleep
// operations register pending waiters that fire when the clock advances
// past their deadline.
//
// FakeClock is safe for concurrent use by multiple goroutines.
func Fake(initial time.Time) *FakeClock {
	clock := &FakeClock{
		current: initial,
	}
	clock.waitersChanged = sync.NewCond(&clock.mu)
	return clock
}

// FakeClock is a deterministic Clock for testing. Time advances only
// when Advance is called. Timers, tickers, and sleeps block until the
// clock is advanced past their deadline.
//
// AfterFunc callbacks are invoked synchronously during Advance in
// deadline order. Do not call Sleep or Advance from within an
// AfterFunc callback — that would deadlock.
type FakeClock struct {
	mu             sync.Mutex
	current        time.Time
	waiters        []*fakeWaiter
	waitersChanged *sync.Cond
}

// fakeWaiter represents a pending timer, ticker, or sleep operation.
type fakeWaiter struct {
	deadline time.Time

	// channel receives the fire time for After, Sleep, and Ticker
	// waiters. Nil for AfterFunc waiters.
	channel chan time.Time

	// callback is invoked synchronously during Advance for AfterFunc
	// waiters. Nil for After, Sleep, and Ticker waiters.
	callback func()

	// interval is non-zero for ticker waiters. After firing, the
	// waiter is rescheduled at deadline + interval.
	interval time.Duration

	// stopped is set by Timer.Stop or Ticker.Stop. Stopped waiters
	// are skipped during Advance and garbage-collected.
	stopped bool

	// fired is set after a one-shot waiter (After, AfterFunc) fires.
	// Prevents double-firing on overlapping Advance calls.
	fired bool
}

// Now returns the current fake time.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

// After returns a channel that receives after duration d elapses. If
// d <= 0, the channel receives immediately without registering a
// waiter.
func (c *FakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	channel := make(chan time.Time, 1)
	if d <= 0 {
		channel <- c.current
		return channel
	}

	c.waiters = append(c.waiters, &fakeWaiter{
		deadline: c.current.Add(d),
		channel:  channel,
	})
	c.waitersChanged.Broadcast()
	return channel
}

// AfterFunc schedules f to be called after duration d. The returned
// Timer's C field is nil. If d <= 0, f is called synchronously before
// AfterFunc returns.
func (c *FakeClock) AfterFunc(d time.Duration, f func()) *Timer {
	c.mu.Lock()
	defer c.mu.Unlock()

	if d <= 0 {
		c.mu.Unlock()
		f()
		c.mu.Lock()
		return &Timer{
			C:         nil,
			stopFunc:  func() bool { return false },
			resetFunc: func(time.Duration) bool { return false },
		}
	}

	waiter := &fakeWaiter{
		deadline: c.current.Add(d),
		callback: f,
	}
	c.waiters = append(c.waiters, waiter)
	c.waitersChanged.Broadcast()

	return &Timer{
		C: nil,
		stopFunc: func() bool {
			c.mu.Lock()
			defer c.mu.Unlock()
			if waiter.stopped || waiter.fired {
				return false
			}
			waiter.stopped = true
			return true
		},
		resetFunc: func(d time.Duration) bool {
			c.mu.Lock()
			defer c.mu.Unlock()
			wasActive := !waiter.stopped && !waiter.fired
			waiter.stopped = false
			waiter.fired = false
			waiter.deadline = c.current.Add(d)
			// Re-add if it was previously removed after firing.
			if !wasActive {
				c.waiters = append(c.waiters, waiter)
				c.waitersChanged.Broadcast()
			}
			return wasActive
		},
	}
}

// NewTicker returns a Ticker that delivers ticks on its C channel at
// the specified interval. Panics if d <= 0.
func (c *FakeClock) NewTicker(d time.Duration) *Ticker {
	if d <= 0 {
		panic("clock: non-positive interval for NewTicker")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	channel := make(chan time.Time, 1)
	waiter := &fakeWaiter{
		deadline: c.current.Add(d),
		channel:  channel,
		interval: d,
	}
	c.waiters = append(c.waiters, waiter)
	c.waitersChanged.Broadcast()

	return &Ticker{
		C: channel,
		stopFunc: func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			waiter.stopped = true
		},
		resetFunc: func(d time.Duration) {
			c.mu.Lock()
			defer c.mu.Unlock()
			waiter.interval = d
			waiter.deadline = c.current.Add(d)
			waiter.stopped = false
		},
	}
}

// Sleep pauses the calling goroutine until the clock advances past
// the deadline. If d <= 0, returns immediately.
func (c *FakeClock) Sleep(d time.Duration) {
	if d <= 0 {
		return
	}
	<-c.After(d)
}

// Advance moves the clock forward by d and fires all timers, tickers,
// and sleeps whose deadlines fall within the new time. Waiters fire in
// deadline order for determinism.
//
// AfterFunc callbacks are invoked synchronously in the calling
// goroutine. Channel sends for After, Sleep, and Ticker are
// non-blocking (matching time.Ticker's drop-if-full behavior).
//
// For tickers, if the advance spans multiple intervals, the ticker
// fires once per interval. Ticks that overflow the channel buffer are
// dropped.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.current = c.current.Add(d)
	target := c.current
	c.mu.Unlock()

	for {
		toFire := c.collectExpired(target)
		if len(toFire) == 0 {
			return
		}

		// Fire in deadline order for determinism.
		sort.Slice(toFire, func(i, j int) bool {
			return toFire[i].deadline.Before(toFire[j].deadline)
		})

		for _, waiter := range toFire {
			if waiter.callback != nil {
				waiter.callback()
			} else if waiter.channel != nil {
				select {
				case waiter.channel <- target:
				default:
				}
			}
		}
	}
}

// collectExpired removes expired waiters from the pending list,
// reschedules tickers, and returns the waiters that should fire.
// Must be called without c.mu held (acquires it internally).
func (c *FakeClock) collectExpired(target time.Time) []*fakeWaiter {
	c.mu.Lock()
	defer c.mu.Unlock()

	var toFire []*fakeWaiter
	var remaining []*fakeWaiter

	for _, waiter := range c.waiters {
		if waiter.stopped {
			continue
		}
		if !waiter.deadline.After(target) {
			toFire = append(toFire, waiter)
		} else {
			remaining = append(remaining, waiter)
		}
	}

	// Reschedule tickers for the next interval. One-shot waiters
	// (After, AfterFunc, Sleep) are removed from the list.
	for _, waiter := range toFire {
		if waiter.interval > 0 {
			waiter.deadline = waiter.deadline.Add(waiter.interval)
			remaining = append(remaining, waiter)
		} else {
			waiter.fired = true
		}
	}

	c.waiters = remaining
	return toFire
}

// WaitForTimers blocks until at least n timers, tickers, or sleeps
// are pending (registered but not yet fired). This synchronization
// primitive eliminates the race between a goroutine registering a
// timer and the test advancing the clock.
//
// Example:
//
//	go func() { fakeClock.Sleep(5 * time.Second) }()
//	fakeClock.WaitForTimers(1)         // blocks until Sleep registers
//	fakeClock.Advance(5 * time.Second) // deterministically fires
func (c *FakeClock) WaitForTimers(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.pendingCountLocked() < n {
		c.waitersChanged.Wait()
	}
}

// PendingCount returns the number of active (non-stopped, non-fired)
// pending waiters. Useful for test assertions.
func (c *FakeClock) PendingCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pendingCountLocked()
}

// pendingCountLocked returns the number of active waiters. Must be
// called with c.mu held.
func (c *FakeClock) pendingCountLocked() int {
	count := 0
	for _, waiter := range c.waiters {
		if !waiter.stopped {
			count++
		}
	}
	return count
}
