// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package clock

import "time"

// Clock abstracts time operations for testability. Production code
// injects Real(); tests inject Fake() with deterministic time control.
//
// Every production function that calls time.Now, time.After,
// time.NewTicker, time.AfterFunc, or time.Sleep should accept a Clock
// parameter (or be a method on a struct with a Clock field) instead of
// calling the time package directly.
type Clock interface {
	// Now returns the current time.
	Now() time.Time

	// After returns a channel that receives the current time after
	// duration d elapses. Equivalent to time.After. If d <= 0, the
	// channel receives immediately.
	After(d time.Duration) <-chan time.Time

	// AfterFunc waits for duration d, then calls f. Returns a Timer
	// that can cancel the pending call with Stop. The Timer's C field
	// is nil (matching time.AfterFunc). If d <= 0, f is called
	// immediately in a new goroutine (real) or synchronously (fake).
	AfterFunc(d time.Duration, f func()) *Timer

	// NewTicker returns a Ticker that delivers ticks on its C channel
	// at the specified interval. Panics if d <= 0. Equivalent to
	// time.NewTicker.
	NewTicker(d time.Duration) *Ticker

	// Sleep pauses the current goroutine for at least duration d.
	// Equivalent to time.Sleep.
	Sleep(d time.Duration)
}

// Ticker wraps a periodic timer. Read ticks from C. Call Stop when the
// Ticker is no longer needed to release resources.
//
// The C channel has capacity 1, matching time.Ticker. If the consumer
// falls behind, ticks are dropped rather than queued.
type Ticker struct {
	// C delivers ticks. Buffered with capacity 1.
	C <-chan time.Time

	stopFunc  func()
	resetFunc func(time.Duration)
}

// Stop turns off the ticker. No more ticks will be sent on C after
// Stop returns. Stop does not close C.
func (t *Ticker) Stop() { t.stopFunc() }

// Reset adjusts the ticker to a new interval and restarts the tick
// cycle. The next tick arrives after the new duration elapses.
func (t *Ticker) Reset(d time.Duration) { t.resetFunc(d) }

// Timer represents a scheduled event. For timers created by AfterFunc,
// C is nil. For timers created internally by After, the Timer is not
// exposed to callers (only the channel is returned).
type Timer struct {
	// C delivers the timer event. Nil for AfterFunc timers.
	C <-chan time.Time

	stopFunc  func() bool
	resetFunc func(time.Duration) bool
}

// Stop prevents the Timer from firing. Returns true if the call stops
// the timer, false if the timer has already fired or been stopped.
func (t *Timer) Stop() bool { return t.stopFunc() }

// Reset changes the timer to fire after duration d. Returns true if
// the timer was active before the reset.
func (t *Timer) Reset(d time.Duration) bool { return t.resetFunc(d) }
