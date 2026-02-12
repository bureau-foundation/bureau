// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package clock provides an injectable time abstraction for testability.
//
// Production code accepts a Clock interface parameter instead of calling
// time.Now, time.After, time.NewTicker, time.AfterFunc, or time.Sleep
// directly. In production, Real() provides the standard library
// behavior. In tests, Fake() provides a deterministic clock that
// advances only when Advance is called.
//
// # Wiring Pattern
//
// Add a Clock field to structs that use time:
//
//	type Server struct {
//	    clock clock.Clock
//	    // ...
//	}
//
// In production:
//
//	s := &Server{clock: clock.Real()}
//
// In tests:
//
//	c := clock.Fake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
//	s := &Server{clock: c}
//	// ... start goroutines ...
//	c.WaitForTimers(1) // wait for goroutine to register a timer
//	c.Advance(5 * time.Second) // fire the timer deterministically
//
// # FakeClock Synchronization
//
// When a goroutine calls Sleep, After, NewTicker, or AfterFunc on a
// FakeClock, it registers a pending timer. Use WaitForTimers to block
// until a specific number of timers are registered before calling
// Advance. This eliminates the race between timer registration and
// time advancement that plagues tests using time.Sleep for
// synchronization.
package clock
