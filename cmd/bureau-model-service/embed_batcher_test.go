// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/modelprovider"
	"github.com/bureau-foundation/bureau/lib/schema/model"
)

// mockProvider implements modelprovider.Provider for testing. Records
// calls and returns configurable results.
type mockProvider struct {
	mu        sync.Mutex
	embedFunc func(ctx context.Context, request *modelprovider.EmbedRequest) (*modelprovider.EmbedResult, error)
	// embedCalls records all Embed invocations for assertions.
	embedCalls []*modelprovider.EmbedRequest
}

func (m *mockProvider) Complete(_ context.Context, _ *modelprovider.CompleteRequest) (modelprovider.CompletionStream, error) {
	return nil, fmt.Errorf("Complete not implemented in mock")
}

func (m *mockProvider) Embed(ctx context.Context, request *modelprovider.EmbedRequest) (*modelprovider.EmbedResult, error) {
	m.mu.Lock()
	m.embedCalls = append(m.embedCalls, request)
	embedFunc := m.embedFunc
	m.mu.Unlock()

	if embedFunc != nil {
		return embedFunc(ctx, request)
	}
	return nil, fmt.Errorf("no embedFunc configured")
}

func (m *mockProvider) Close() error { return nil }

func (m *mockProvider) EmbedCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.embedCalls)
}

func (m *mockProvider) LastEmbedCall() *modelprovider.EmbedRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.embedCalls) == 0 {
		return nil
	}
	return m.embedCalls[len(m.embedCalls)-1]
}

// waitForEmbedCalls waits for a flush goroutine to call the provider.
// Uses runtime.Gosched to yield and a polling loop since the flush
// runs in a separate goroutine. Falls back to t.Context() for timeout.
func waitForEmbedCalls(t *testing.T, provider *mockProvider, expected int) {
	t.Helper()
	for provider.EmbedCallCount() < expected {
		if t.Context().Err() != nil {
			t.Fatalf("timed out waiting for %d embed calls (got %d)", expected, provider.EmbedCallCount())
		}
		runtime.Gosched()
	}
}

func TestEmbedBatcherFlushOnTimer(t *testing.T) {
	fakeClock := clock.Fake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	ctx := context.Background()
	logger := slog.Default()

	provider := &mockProvider{
		embedFunc: func(_ context.Context, request *modelprovider.EmbedRequest) (*modelprovider.EmbedResult, error) {
			// Return one embedding per input.
			embeddings := make([][]byte, len(request.Input))
			for i := range request.Input {
				embeddings[i] = []byte(fmt.Sprintf("vec-%d", i))
			}
			return &modelprovider.EmbedResult{
				Embeddings: embeddings,
				Model:      "test-model",
				Usage:      model.Usage{InputTokens: int64(len(request.Input) * 10)},
			}, nil
		},
	}

	batcher := NewEmbedBatcher(ctx, fakeClock, logger, 50*time.Millisecond, 64)
	defer batcher.Close()

	key := embedBatchKey{
		ProviderName:  "test-provider",
		ProviderModel: "test-model",
		Credential:    "test-cred",
	}

	// Submit one request with 2 inputs.
	resultChan := batcher.Submit(ctx, key, provider, [][]byte{
		[]byte("hello"),
		[]byte("world"),
	}, 0)

	// Timer should be registered.
	fakeClock.WaitForTimers(1)

	// Advance past flush delay.
	fakeClock.Advance(50 * time.Millisecond)

	// Wait for the flush goroutine.
	waitForEmbedCalls(t, provider, 1)

	select {
	case result := <-resultChan:
		if result.Error != nil {
			t.Fatalf("unexpected error: %v", result.Error)
		}
		if len(result.Embeddings) != 2 {
			t.Fatalf("expected 2 embeddings, got %d", len(result.Embeddings))
		}
		if result.BatchSize != 1 {
			t.Fatalf("expected batch size 1, got %d", result.BatchSize)
		}
		if result.Model != "test-model" {
			t.Fatalf("expected model test-model, got %s", result.Model)
		}
		if result.Usage.InputTokens != 20 {
			t.Fatalf("expected 20 input tokens, got %d", result.Usage.InputTokens)
		}
	case <-t.Context().Done():
		t.Fatal("timed out waiting for result")
	}

	if provider.EmbedCallCount() != 1 {
		t.Fatalf("expected 1 embed call, got %d", provider.EmbedCallCount())
	}
}

func TestEmbedBatcherFlushOnMaxSize(t *testing.T) {
	fakeClock := clock.Fake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	ctx := context.Background()
	logger := slog.Default()

	provider := &mockProvider{
		embedFunc: func(_ context.Context, request *modelprovider.EmbedRequest) (*modelprovider.EmbedResult, error) {
			embeddings := make([][]byte, len(request.Input))
			for i := range request.Input {
				embeddings[i] = []byte(fmt.Sprintf("vec-%d", i))
			}
			return &modelprovider.EmbedResult{
				Embeddings: embeddings,
				Model:      "test-model",
				Usage:      model.Usage{InputTokens: int64(len(request.Input) * 10)},
			}, nil
		},
	}

	// Max batch size of 3.
	batcher := NewEmbedBatcher(ctx, fakeClock, logger, 50*time.Millisecond, 3)
	defer batcher.Close()

	key := embedBatchKey{
		ProviderName:  "test-provider",
		ProviderModel: "test-model",
		Credential:    "test-cred",
	}

	// Submit 3 requests — should trigger immediate flush.
	var resultChans [3]<-chan EmbedBatchResult
	for i := range 3 {
		resultChans[i] = batcher.Submit(ctx, key, provider,
			[][]byte{[]byte(fmt.Sprintf("input-%d", i))}, 0)
	}

	// Flush happens in a goroutine. Wait for it.
	waitForEmbedCalls(t, provider, 1)

	// All three should have results.
	for i, ch := range resultChans {
		select {
		case result := <-ch:
			if result.Error != nil {
				t.Fatalf("request %d: unexpected error: %v", i, result.Error)
			}
			if len(result.Embeddings) != 1 {
				t.Fatalf("request %d: expected 1 embedding, got %d", i, len(result.Embeddings))
			}
			if result.BatchSize != 3 {
				t.Fatalf("request %d: expected batch size 3, got %d", i, result.BatchSize)
			}
		case <-t.Context().Done():
			t.Fatalf("request %d: timed out", i)
		}
	}

	// Should be exactly 1 provider call (not 3).
	if provider.EmbedCallCount() != 1 {
		t.Fatalf("expected 1 embed call, got %d", provider.EmbedCallCount())
	}
}

func TestEmbedBatcherMergesInputs(t *testing.T) {
	fakeClock := clock.Fake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	ctx := context.Background()
	logger := slog.Default()

	provider := &mockProvider{
		embedFunc: func(_ context.Context, request *modelprovider.EmbedRequest) (*modelprovider.EmbedResult, error) {
			// Return embedding = "emb-" + input text for easy verification.
			embeddings := make([][]byte, len(request.Input))
			for i, input := range request.Input {
				embeddings[i] = []byte("emb-" + string(input))
			}
			return &modelprovider.EmbedResult{
				Embeddings: embeddings,
				Model:      "test-model",
				Usage:      model.Usage{InputTokens: 500},
			}, nil
		},
	}

	batcher := NewEmbedBatcher(ctx, fakeClock, logger, 50*time.Millisecond, 64)
	defer batcher.Close()

	key := embedBatchKey{
		ProviderName:  "test-provider",
		ProviderModel: "test-model",
		Credential:    "test-cred",
	}

	// Request A: 2 inputs.
	chanA := batcher.Submit(ctx, key, provider,
		[][]byte{[]byte("a1"), []byte("a2")}, 0)

	// Request B: 3 inputs.
	chanB := batcher.Submit(ctx, key, provider,
		[][]byte{[]byte("b1"), []byte("b2"), []byte("b3")}, 0)

	// Request C: 1 input.
	chanC := batcher.Submit(ctx, key, provider,
		[][]byte{[]byte("c1")}, 0)

	// Flush via timer.
	fakeClock.WaitForTimers(1)
	fakeClock.Advance(50 * time.Millisecond)
	waitForEmbedCalls(t, provider, 1)

	// Verify provider received all 6 inputs in order.
	lastCall := provider.LastEmbedCall()
	if len(lastCall.Input) != 6 {
		t.Fatalf("expected 6 merged inputs, got %d", len(lastCall.Input))
	}
	expectedInputs := []string{"a1", "a2", "b1", "b2", "b3", "c1"}
	for i, expected := range expectedInputs {
		if string(lastCall.Input[i]) != expected {
			t.Fatalf("input[%d]: expected %q, got %q", i, expected, string(lastCall.Input[i]))
		}
	}

	// Verify A gets its 2 embeddings.
	resultA := <-chanA
	if resultA.Error != nil {
		t.Fatalf("A error: %v", resultA.Error)
	}
	if len(resultA.Embeddings) != 2 {
		t.Fatalf("A: expected 2 embeddings, got %d", len(resultA.Embeddings))
	}
	if string(resultA.Embeddings[0]) != "emb-a1" || string(resultA.Embeddings[1]) != "emb-a2" {
		t.Fatalf("A: wrong embeddings: %q, %q", resultA.Embeddings[0], resultA.Embeddings[1])
	}

	// Verify B gets its 3 embeddings.
	resultB := <-chanB
	if resultB.Error != nil {
		t.Fatalf("B error: %v", resultB.Error)
	}
	if len(resultB.Embeddings) != 3 {
		t.Fatalf("B: expected 3 embeddings, got %d", len(resultB.Embeddings))
	}
	if string(resultB.Embeddings[0]) != "emb-b1" {
		t.Fatalf("B: wrong first embedding: %q", resultB.Embeddings[0])
	}

	// Verify C gets its 1 embedding.
	resultC := <-chanC
	if resultC.Error != nil {
		t.Fatalf("C error: %v", resultC.Error)
	}
	if len(resultC.Embeddings) != 1 {
		t.Fatalf("C: expected 1 embedding, got %d", len(resultC.Embeddings))
	}
	if string(resultC.Embeddings[0]) != "emb-c1" {
		t.Fatalf("C: wrong embedding: %q", resultC.Embeddings[0])
	}

	// Verify proportional token attribution.
	// Total: 500 tokens across 6 inputs.
	// A: 2/6 * 500 = 166 (integer division)
	// B: 3/6 * 500 = 250
	// C: 1/6 * 500 = 83
	if resultA.Usage.InputTokens != 166 {
		t.Fatalf("A: expected 166 attributed tokens, got %d", resultA.Usage.InputTokens)
	}
	if resultB.Usage.InputTokens != 250 {
		t.Fatalf("B: expected 250 attributed tokens, got %d", resultB.Usage.InputTokens)
	}
	if resultC.Usage.InputTokens != 83 {
		t.Fatalf("C: expected 83 attributed tokens, got %d", resultC.Usage.InputTokens)
	}
}

func TestEmbedBatcherDifferentKeys(t *testing.T) {
	fakeClock := clock.Fake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	ctx := context.Background()
	logger := slog.Default()

	provider := &mockProvider{
		embedFunc: func(_ context.Context, request *modelprovider.EmbedRequest) (*modelprovider.EmbedResult, error) {
			embeddings := make([][]byte, len(request.Input))
			for i := range request.Input {
				embeddings[i] = []byte("vec")
			}
			return &modelprovider.EmbedResult{
				Embeddings: embeddings,
				Model:      "test-model",
				Usage:      model.Usage{InputTokens: 100},
			}, nil
		},
	}

	batcher := NewEmbedBatcher(ctx, fakeClock, logger, 50*time.Millisecond, 64)
	defer batcher.Close()

	keyA := embedBatchKey{ProviderName: "p", ProviderModel: "m", Credential: "credA"}
	keyB := embedBatchKey{ProviderName: "p", ProviderModel: "m", Credential: "credB"}

	chanA := batcher.Submit(ctx, keyA, provider, [][]byte{[]byte("a")}, 0)
	chanB := batcher.Submit(ctx, keyB, provider, [][]byte{[]byte("b")}, 0)

	// Two separate timers — one per key.
	fakeClock.WaitForTimers(2)
	fakeClock.Advance(50 * time.Millisecond)

	waitForEmbedCalls(t, provider, 2)

	resultA := <-chanA
	resultB := <-chanB

	if resultA.Error != nil {
		t.Fatalf("A error: %v", resultA.Error)
	}
	if resultB.Error != nil {
		t.Fatalf("B error: %v", resultB.Error)
	}

	// Two separate provider calls (different credentials).
	if provider.EmbedCallCount() != 2 {
		t.Fatalf("expected 2 embed calls, got %d", provider.EmbedCallCount())
	}

	// Each batch has size 1.
	if resultA.BatchSize != 1 {
		t.Fatalf("A: expected batch size 1, got %d", resultA.BatchSize)
	}
	if resultB.BatchSize != 1 {
		t.Fatalf("B: expected batch size 1, got %d", resultB.BatchSize)
	}
}

func TestEmbedBatcherContextCancellation(t *testing.T) {
	fakeClock := clock.Fake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	serverCtx := context.Background()
	logger := slog.Default()

	provider := &mockProvider{
		embedFunc: func(_ context.Context, request *modelprovider.EmbedRequest) (*modelprovider.EmbedResult, error) {
			embeddings := make([][]byte, len(request.Input))
			for i := range request.Input {
				embeddings[i] = []byte("vec")
			}
			return &modelprovider.EmbedResult{
				Embeddings: embeddings,
				Model:      "test-model",
				Usage:      model.Usage{InputTokens: int64(len(request.Input) * 10)},
			}, nil
		},
	}

	batcher := NewEmbedBatcher(serverCtx, fakeClock, logger, 50*time.Millisecond, 64)
	defer batcher.Close()

	key := embedBatchKey{ProviderName: "p", ProviderModel: "m", Credential: "c"}

	// Request A: live context.
	ctxA := context.Background()
	chanA := batcher.Submit(ctxA, key, provider, [][]byte{[]byte("a")}, 0)

	// Request B: will be cancelled before flush.
	ctxB, cancelB := context.WithCancel(context.Background())
	chanB := batcher.Submit(ctxB, key, provider, [][]byte{[]byte("b")}, 0)

	// Request C: live context.
	ctxC := context.Background()
	chanC := batcher.Submit(ctxC, key, provider, [][]byte{[]byte("c")}, 0)

	// Cancel B before the flush.
	cancelB()

	// Flush.
	fakeClock.WaitForTimers(1)
	fakeClock.Advance(50 * time.Millisecond)
	waitForEmbedCalls(t, provider, 1)

	// A and C should succeed.
	resultA := <-chanA
	if resultA.Error != nil {
		t.Fatalf("A error: %v", resultA.Error)
	}
	if resultA.BatchSize != 2 {
		t.Fatalf("A: expected batch size 2 (B was cancelled), got %d", resultA.BatchSize)
	}

	resultC := <-chanC
	if resultC.Error != nil {
		t.Fatalf("C error: %v", resultC.Error)
	}

	// B should get a context error.
	resultB := <-chanB
	if resultB.Error == nil {
		t.Fatal("B: expected error, got nil")
	}

	// Provider should have received 2 inputs (a, c), not 3.
	lastCall := provider.LastEmbedCall()
	if len(lastCall.Input) != 2 {
		t.Fatalf("expected 2 inputs to provider, got %d", len(lastCall.Input))
	}
}

func TestEmbedBatcherAllCancelled(t *testing.T) {
	fakeClock := clock.Fake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	serverCtx := context.Background()
	logger := slog.Default()

	provider := &mockProvider{
		embedFunc: func(_ context.Context, _ *modelprovider.EmbedRequest) (*modelprovider.EmbedResult, error) {
			t.Fatal("provider should not be called when all callers cancelled")
			return nil, nil
		},
	}

	batcher := NewEmbedBatcher(serverCtx, fakeClock, logger, 50*time.Millisecond, 64)
	defer batcher.Close()

	key := embedBatchKey{ProviderName: "p", ProviderModel: "m", Credential: "c"}

	ctx, cancel := context.WithCancel(context.Background())
	chanA := batcher.Submit(ctx, key, provider, [][]byte{[]byte("a")}, 0)
	chanB := batcher.Submit(ctx, key, provider, [][]byte{[]byte("b")}, 0)

	cancel()

	fakeClock.WaitForTimers(1)
	fakeClock.Advance(50 * time.Millisecond)

	// Reading from result channels blocks until the flush goroutine
	// processes the cancelled entries. The channel sends ARE the
	// synchronization — no sleep needed.
	resultA := <-chanA
	if resultA.Error == nil {
		t.Fatal("A: expected error")
	}
	resultB := <-chanB
	if resultB.Error == nil {
		t.Fatal("B: expected error")
	}

	if provider.EmbedCallCount() != 0 {
		t.Fatalf("expected 0 embed calls, got %d", provider.EmbedCallCount())
	}
}

func TestEmbedBatcherProviderError(t *testing.T) {
	fakeClock := clock.Fake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	ctx := context.Background()
	logger := slog.Default()

	providerErr := fmt.Errorf("provider exploded")
	provider := &mockProvider{
		embedFunc: func(_ context.Context, _ *modelprovider.EmbedRequest) (*modelprovider.EmbedResult, error) {
			return nil, providerErr
		},
	}

	batcher := NewEmbedBatcher(ctx, fakeClock, logger, 50*time.Millisecond, 64)
	defer batcher.Close()

	key := embedBatchKey{ProviderName: "p", ProviderModel: "m", Credential: "c"}

	chanA := batcher.Submit(ctx, key, provider, [][]byte{[]byte("a")}, 0)
	chanB := batcher.Submit(ctx, key, provider, [][]byte{[]byte("b")}, 0)

	fakeClock.WaitForTimers(1)
	fakeClock.Advance(50 * time.Millisecond)
	waitForEmbedCalls(t, provider, 1)

	resultA := <-chanA
	if resultA.Error != providerErr {
		t.Fatalf("A: expected %v, got %v", providerErr, resultA.Error)
	}
	resultB := <-chanB
	if resultB.Error != providerErr {
		t.Fatalf("B: expected %v, got %v", providerErr, resultB.Error)
	}
}

func TestEmbedBatcherClose(t *testing.T) {
	fakeClock := clock.Fake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	ctx := context.Background()
	logger := slog.Default()

	provider := &mockProvider{
		embedFunc: func(_ context.Context, request *modelprovider.EmbedRequest) (*modelprovider.EmbedResult, error) {
			embeddings := make([][]byte, len(request.Input))
			for i := range request.Input {
				embeddings[i] = []byte("vec")
			}
			return &modelprovider.EmbedResult{
				Embeddings: embeddings,
				Model:      "test-model",
				Usage:      model.Usage{InputTokens: 100},
			}, nil
		},
	}

	batcher := NewEmbedBatcher(ctx, fakeClock, logger, 50*time.Millisecond, 64)

	key := embedBatchKey{ProviderName: "p", ProviderModel: "m", Credential: "c"}
	chanA := batcher.Submit(ctx, key, provider, [][]byte{[]byte("a")}, 0)

	// Close flushes pending batches.
	batcher.Close()

	// The pending request should get a result (last-chance flush).
	select {
	case result := <-chanA:
		if result.Error != nil {
			t.Fatalf("expected successful result from close flush, got error: %v", result.Error)
		}
	case <-t.Context().Done():
		t.Fatal("timed out waiting for result after Close")
	}

	// Submitting after close returns an error immediately.
	chanB := batcher.Submit(ctx, key, provider, [][]byte{[]byte("b")}, 0)
	resultB := <-chanB
	if resultB.Error == nil {
		t.Fatal("expected error from submit after close")
	}
}
