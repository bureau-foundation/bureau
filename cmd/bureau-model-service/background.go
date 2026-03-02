// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"sync"
)

// BackgroundScheduler defers background-policy requests until the
// provider has no active immediate/batch requests in flight. This
// ensures that interactive and batched work always takes priority
// over background processing.
//
// The scheduler tracks a per-provider active request count. Immediate
// and batch handlers call RecordStart/RecordEnd. Background handlers
// call WaitForIdle, which blocks until the active count for their
// provider drops to zero.
//
// When an immediate/batch request arrives while background requests
// are in flight, the immediate/batch request proceeds immediately —
// it does not wait. Background requests that are already running are
// not preempted. Only background requests that have not yet started
// wait for idle.
//
// Thread-safe.
type BackgroundScheduler struct {
	logger *slog.Logger

	mu          sync.Mutex
	cond        *sync.Cond
	activeCount map[string]int64 // providerName → in-flight immediate/batch requests

	// waitingCount tracks how many goroutines are inside WaitForIdle
	// per provider. waiterCond is signaled whenever a goroutine
	// enters WaitForIdle. Used by WaitForWaiters for deterministic
	// test synchronization.
	waitingCount map[string]int
	waiterCond   *sync.Cond
}

// NewBackgroundScheduler creates a scheduler.
func NewBackgroundScheduler(logger *slog.Logger) *BackgroundScheduler {
	scheduler := &BackgroundScheduler{
		logger:       logger,
		activeCount:  make(map[string]int64),
		waitingCount: make(map[string]int),
	}
	scheduler.cond = sync.NewCond(&scheduler.mu)
	scheduler.waiterCond = sync.NewCond(&scheduler.mu)
	return scheduler
}

// RecordStart increments the active count for a provider. Called when
// an immediate or batch request begins its provider API call.
func (s *BackgroundScheduler) RecordStart(providerName string) {
	s.mu.Lock()
	s.activeCount[providerName]++
	s.mu.Unlock()
}

// RecordEnd decrements the active count for a provider and wakes any
// background waiters. Called when an immediate or batch request
// completes its provider API call.
func (s *BackgroundScheduler) RecordEnd(providerName string) {
	s.mu.Lock()
	s.activeCount[providerName]--
	if s.activeCount[providerName] <= 0 {
		delete(s.activeCount, providerName)
	}
	s.cond.Broadcast()
	s.mu.Unlock()
}

// WaitForIdle blocks until there are no active immediate/batch
// requests for the given provider, or the context is cancelled.
// Returns nil when the provider is idle, or ctx.Err() on
// cancellation.
func (s *BackgroundScheduler) WaitForIdle(ctx context.Context, providerName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Track that this goroutine is waiting, for test synchronization.
	s.waitingCount[providerName]++
	s.waiterCond.Broadcast()
	defer func() {
		s.waitingCount[providerName]--
		if s.waitingCount[providerName] <= 0 {
			delete(s.waitingCount, providerName)
		}
	}()

	// Spawn a goroutine that broadcasts when the context cancels,
	// waking this waiter so it can check ctx.Err(). The goroutine
	// exits when either the context cancels or the done channel
	// closes (this function returns).
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			s.cond.Broadcast()
		case <-done:
		}
	}()

	for s.activeCount[providerName] > 0 {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.cond.Wait()
	}

	s.logger.Debug("background request proceeding (provider idle)",
		"provider", providerName,
	)
	return nil
}

// WaitForWaiters blocks until at least count goroutines are inside
// WaitForIdle for the given provider. Used for deterministic test
// synchronization — ensures goroutines have reached the wait point
// before the test modifies state they depend on.
func (s *BackgroundScheduler) WaitForWaiters(providerName string, count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.waitingCount[providerName] < count {
		s.waiterCond.Wait()
	}
}

// Close wakes all background waiters. They should observe their
// context being cancelled (from server shutdown) and return errors.
func (s *BackgroundScheduler) Close() {
	s.cond.Broadcast()
}
