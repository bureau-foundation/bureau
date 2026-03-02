// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
)

// completionGateKey groups completion requests for coordinated
// dispatch. The credential is NOT part of the key because each
// completion still gets its own provider.Complete call — the gate
// only controls timing so the inference engine sees concurrent
// requests it can batch internally.
type completionGateKey struct {
	ProviderName  string
	ProviderModel string
}

// CompletionGate groups completion requests by (provider, model) and
// releases them simultaneously when a count or timer trigger fires.
// This lets local inference engines (llama.cpp, vLLM) batch the
// attention computation across multiple requests.
//
// Unlike the EmbedBatcher, the CompletionGate does not merge requests
// or split responses. Each released handler proceeds independently
// with its own provider.Complete call. The gate only controls when
// they start.
//
// Thread-safe: Wait is called from handler goroutines, timer callbacks
// call openGate from the clock's goroutine.
type CompletionGate struct {
	clock      clock.Clock
	logger     *slog.Logger
	flushDelay time.Duration

	// defaultMaxGateSize is the gate size when the provider declares
	// MaxBatchSize=0. Controls how many completions are grouped.
	defaultMaxGateSize int

	mu     sync.Mutex
	gates  map[completionGateKey]*pendingGate
	closed bool

	// waiterCond is signaled whenever a waiter is added to any gate.
	// Used by WaitForGateSize for deterministic test synchronization.
	waiterCond *sync.Cond
}

// pendingGate collects waiters until the gate opens.
type pendingGate struct {
	waiters    []chan struct{}
	flushTimer *clock.Timer
	maxSize    int
}

// NewCompletionGate creates a gate with the given flush parameters.
func NewCompletionGate(
	clk clock.Clock,
	logger *slog.Logger,
	flushDelay time.Duration,
	defaultMaxGateSize int,
) *CompletionGate {
	g := &CompletionGate{
		clock:              clk,
		logger:             logger,
		flushDelay:         flushDelay,
		defaultMaxGateSize: defaultMaxGateSize,
		gates:              make(map[completionGateKey]*pendingGate),
	}
	g.waiterCond = sync.NewCond(&g.mu)
	return g
}

// Wait blocks until the gate for the given key opens or ctx cancels.
// When the gate opens (count or timer), all waiters are released
// simultaneously. Returns nil on release, or ctx.Err() on
// cancellation.
func (g *CompletionGate) Wait(
	ctx context.Context,
	key completionGateKey,
	maxBatchSize int,
) error {
	waiterChan := make(chan struct{})

	g.mu.Lock()

	if g.closed {
		g.mu.Unlock()
		return context.Canceled
	}

	gate, exists := g.gates[key]
	if !exists {
		effectiveMax := maxBatchSize
		if effectiveMax <= 0 {
			effectiveMax = g.defaultMaxGateSize
		}

		gate = &pendingGate{
			maxSize: effectiveMax,
		}
		gate.flushTimer = g.clock.AfterFunc(g.flushDelay, func() {
			go g.openGate(key)
		})
		g.gates[key] = gate
	}

	gate.waiters = append(gate.waiters, waiterChan)
	g.waiterCond.Broadcast()

	if len(gate.waiters) >= gate.maxSize {
		gate.flushTimer.Stop()
		delete(g.gates, key)
		g.mu.Unlock()
		// Close all waiter channels to release them simultaneously.
		for _, ch := range gate.waiters {
			close(ch)
		}
		return nil
	}

	g.mu.Unlock()

	select {
	case <-waiterChan:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// openGate is called by the timer callback. It extracts the gate from
// the map (if it still exists) and releases all waiters.
func (g *CompletionGate) openGate(key completionGateKey) {
	g.mu.Lock()
	gate, exists := g.gates[key]
	if !exists {
		// Already opened by Wait (gate hit maxSize between timer
		// schedule and timer fire). Nothing to do.
		g.mu.Unlock()
		return
	}
	delete(g.gates, key)
	g.mu.Unlock()

	g.logger.Debug("opening completion gate",
		"gate_size", len(gate.waiters),
		"model", key.ProviderModel,
	)

	for _, ch := range gate.waiters {
		close(ch)
	}
}

// Close stops all timers and opens all pending gates. Waiters that are
// still blocked will be released. If they check their context (which
// should be the server context), they'll find it cancelled and return
// errors to clients.
func (g *CompletionGate) Close() {
	g.mu.Lock()
	g.closed = true
	pending := make([]*pendingGate, 0, len(g.gates))
	for _, gate := range g.gates {
		gate.flushTimer.Stop()
		pending = append(pending, gate)
	}
	g.gates = make(map[completionGateKey]*pendingGate)
	g.mu.Unlock()

	for _, gate := range pending {
		for _, ch := range gate.waiters {
			close(ch)
		}
	}
}

// WaitForGateSize blocks until the gate for the given key has at
// least count waiters queued. Used for deterministic test
// synchronization — ensures all goroutines have reached Wait()
// before the test advances the clock.
func (g *CompletionGate) WaitForGateSize(key completionGateKey, count int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for {
		if gate, ok := g.gates[key]; ok && len(gate.waiters) >= count {
			return
		}
		g.waiterCond.Wait()
	}
}
