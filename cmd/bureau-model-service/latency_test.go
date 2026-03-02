// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/modelprovider"
	"github.com/bureau-foundation/bureau/lib/schema/model"
)

// newTestRouter creates a LatencyRouter with a fake clock and short
// flush delay for testing. Returns the router and the fake clock for
// advancing timers.
func newTestRouter(t *testing.T) (*LatencyRouter, *clock.FakeClock) {
	t.Helper()
	fakeClock := clock.Fake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	router := NewLatencyRouter(
		context.Background(),
		fakeClock,
		slog.Default(),
		LatencyConfig{
			FlushDelay:          50 * time.Millisecond,
			DefaultMaxBatchSize: 64,
		},
	)
	t.Cleanup(router.Close)
	return router, fakeClock
}

// echoEmbedProvider returns a mockProvider that echoes "emb-" +
// input text as embeddings and reports 10 tokens per input.
func echoEmbedProvider() *mockProvider {
	return &mockProvider{
		embedFunc: func(_ context.Context, request *modelprovider.EmbedRequest) (*modelprovider.EmbedResult, error) {
			embeddings := make([][]byte, len(request.Input))
			for i, input := range request.Input {
				embeddings[i] = []byte("emb-" + string(input))
			}
			return &modelprovider.EmbedResult{
				Embeddings: embeddings,
				Model:      "test-model",
				Usage:      model.Usage{InputTokens: int64(len(request.Input) * 10)},
			}, nil
		},
	}
}

func TestLatencyRouterImmediateEmbed(t *testing.T) {
	router, _ := newTestRouter(t)
	provider := echoEmbedProvider()

	result, err := router.SubmitEmbed(
		context.Background(),
		model.LatencyImmediate,
		provider,
		"test-provider", "test-model", "test-cred",
		[][]byte{[]byte("hello"), []byte("world")},
		0, true,
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Embeddings) != 2 {
		t.Fatalf("expected 2 embeddings, got %d", len(result.Embeddings))
	}
	if string(result.Embeddings[0]) != "emb-hello" {
		t.Fatalf("wrong embedding[0]: %q", result.Embeddings[0])
	}
	if result.BatchSize != 1 {
		t.Fatalf("immediate should have batch size 1, got %d", result.BatchSize)
	}
	if result.Usage.InputTokens != 20 {
		t.Fatalf("expected 20 input tokens, got %d", result.Usage.InputTokens)
	}

	// Direct call — exactly 1 provider invocation.
	if provider.EmbedCallCount() != 1 {
		t.Fatalf("expected 1 embed call, got %d", provider.EmbedCallCount())
	}
}

func TestLatencyRouterBatchEmbed(t *testing.T) {
	router, fakeClock := newTestRouter(t)
	provider := echoEmbedProvider()

	batchKey := embedBatchKey{ProviderName: "p", ProviderModel: "m", Credential: "cred"}

	// Submit two batch embed requests concurrently. They should be
	// merged into a single provider call by the EmbedBatcher.
	type embedResult struct {
		result EmbedBatchResult
		err    error
	}
	resultChanA := make(chan embedResult, 1)
	resultChanB := make(chan embedResult, 1)

	go func() {
		result, err := router.SubmitEmbed(
			context.Background(), model.LatencyBatch,
			provider, "p", "m", "cred",
			[][]byte{[]byte("a1"), []byte("a2")},
			0, true,
		)
		resultChanA <- embedResult{result, err}
	}()
	go func() {
		result, err := router.SubmitEmbed(
			context.Background(), model.LatencyBatch,
			provider, "p", "m", "cred",
			[][]byte{[]byte("b1")},
			0, true,
		)
		resultChanB <- embedResult{result, err}
	}()

	// Wait for both goroutines to have submitted to the batcher.
	// This is deterministic: WaitForBatchSize blocks on a condition
	// variable that fires when any entry is added.
	router.embedBatcher.WaitForBatchSize(batchKey, 2)

	// Advance past the flush delay.
	fakeClock.Advance(50 * time.Millisecond)
	waitForEmbedCalls(t, provider, 1)

	resultA := <-resultChanA
	resultB := <-resultChanB

	if resultA.err != nil {
		t.Fatalf("A error: %v", resultA.err)
	}
	if resultB.err != nil {
		t.Fatalf("B error: %v", resultB.err)
	}

	// Both should report batch size 2 (two callers in the batch).
	if resultA.result.BatchSize != 2 {
		t.Fatalf("A: expected batch size 2, got %d", resultA.result.BatchSize)
	}
	if resultB.result.BatchSize != 2 {
		t.Fatalf("B: expected batch size 2, got %d", resultB.result.BatchSize)
	}

	// A gets 2 embeddings, B gets 1.
	if len(resultA.result.Embeddings) != 2 {
		t.Fatalf("A: expected 2 embeddings, got %d", len(resultA.result.Embeddings))
	}
	if len(resultB.result.Embeddings) != 1 {
		t.Fatalf("B: expected 1 embedding, got %d", len(resultB.result.Embeddings))
	}

	// Single merged provider call.
	if provider.EmbedCallCount() != 1 {
		t.Fatalf("expected 1 provider call, got %d", provider.EmbedCallCount())
	}
}

func TestLatencyRouterBatchDegradesToImmediateWithoutBatchSupport(t *testing.T) {
	router, _ := newTestRouter(t)
	provider := echoEmbedProvider()

	// batchSupport=false — batch policy degrades to immediate.
	result, err := router.SubmitEmbed(
		context.Background(),
		model.LatencyBatch,
		provider,
		"p", "m", "cred",
		[][]byte{[]byte("hello")},
		0, false, // no batch support
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.BatchSize != 1 {
		t.Fatalf("expected batch size 1 (degraded to immediate), got %d", result.BatchSize)
	}

	// Direct call, not batched.
	if provider.EmbedCallCount() != 1 {
		t.Fatalf("expected 1 direct call, got %d", provider.EmbedCallCount())
	}
}

func TestLatencyRouterBackgroundDegradesToImmediateWithoutBatchSupport(t *testing.T) {
	router, _ := newTestRouter(t)
	provider := echoEmbedProvider()

	// batchSupport=false — background policy degrades to immediate.
	result, err := router.SubmitEmbed(
		context.Background(),
		model.LatencyBackground,
		provider,
		"p", "m", "cred",
		[][]byte{[]byte("hello")},
		0, false,
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.BatchSize != 1 {
		t.Fatalf("expected batch size 1, got %d", result.BatchSize)
	}
	if provider.EmbedCallCount() != 1 {
		t.Fatalf("expected 1 direct call, got %d", provider.EmbedCallCount())
	}
}

func TestLatencyRouterBackgroundEmbedWaitsForIdle(t *testing.T) {
	router, fakeClock := newTestRouter(t)
	provider := echoEmbedProvider()

	// Simulate an active immediate request.
	router.RecordActiveStart("p")

	// Background embed should block until idle.
	type embedResult struct {
		result EmbedBatchResult
		err    error
	}
	resultChan := make(chan embedResult, 1)
	go func() {
		result, err := router.SubmitEmbed(
			context.Background(), model.LatencyBackground,
			provider, "p", "m", "cred",
			[][]byte{[]byte("bg")},
			0, true,
		)
		resultChan <- embedResult{result, err}
	}()

	// Wait for the goroutine to enter WaitForIdle.
	router.backgroundScheduler.WaitForWaiters("p", 1)

	// Verify still blocked.
	select {
	case <-resultChan:
		t.Fatal("background embed should be blocked while provider is active")
	default:
	}

	// End the active request — background should proceed to batcher.
	router.RecordActiveEnd("p")

	// The batcher timer fires. Wait for it and advance.
	fakeClock.WaitForTimers(1)
	fakeClock.Advance(50 * time.Millisecond)
	waitForEmbedCalls(t, provider, 1)

	select {
	case result := <-resultChan:
		if result.err != nil {
			t.Fatalf("unexpected error: %v", result.err)
		}
		if string(result.result.Embeddings[0]) != "emb-bg" {
			t.Fatalf("wrong embedding: %q", result.result.Embeddings[0])
		}
	case <-t.Context().Done():
		t.Fatal("timed out waiting for background embed result")
	}
}

func TestLatencyRouterGateCompleteImmediate(t *testing.T) {
	router, _ := newTestRouter(t)

	// Immediate gating returns nil without blocking.
	err := router.GateComplete(
		context.Background(),
		model.LatencyImmediate,
		"p", "m", 0,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLatencyRouterGateCompleteBatch(t *testing.T) {
	router, fakeClock := newTestRouter(t)

	// Launch two batch completion requests. They should both block
	// until the gate opens.
	errChanA := make(chan error, 1)
	errChanB := make(chan error, 1)

	go func() {
		errChanA <- router.GateComplete(
			context.Background(), model.LatencyBatch,
			"p", "m", 0,
		)
	}()
	go func() {
		errChanB <- router.GateComplete(
			context.Background(), model.LatencyBatch,
			"p", "m", 0,
		)
	}()

	// Wait for both to register in the gate, then advance the timer.
	router.completionGate.WaitForGateSize(
		completionGateKey{ProviderName: "p", ProviderModel: "m"}, 2,
	)
	fakeClock.Advance(50 * time.Millisecond)

	// Both should complete.
	select {
	case err := <-errChanA:
		if err != nil {
			t.Fatalf("A: unexpected error: %v", err)
		}
	case <-t.Context().Done():
		t.Fatal("A: timed out")
	}
	select {
	case err := <-errChanB:
		if err != nil {
			t.Fatalf("B: unexpected error: %v", err)
		}
	case <-t.Context().Done():
		t.Fatal("B: timed out")
	}
}

func TestLatencyRouterGateCompleteBackground(t *testing.T) {
	router, _ := newTestRouter(t)

	// Active immediate request blocks background completions.
	router.RecordActiveStart("p")

	proceedChan := make(chan struct{})
	go func() {
		_ = router.GateComplete(
			context.Background(), model.LatencyBackground,
			"p", "m", 0,
		)
		close(proceedChan)
	}()

	// Wait for the goroutine to enter WaitForIdle.
	router.backgroundScheduler.WaitForWaiters("p", 1)

	// Verify still blocked.
	select {
	case <-proceedChan:
		t.Fatal("background gate should be blocked while provider is active")
	default:
	}

	// End the active request — background proceeds without gating.
	router.RecordActiveEnd("p")

	select {
	case <-proceedChan:
		// OK
	case <-t.Context().Done():
		t.Fatal("timed out waiting for background completion to proceed")
	}
}

func TestLatencyRouterGateCompleteContextCancel(t *testing.T) {
	router, _ := newTestRouter(t)

	// Active request blocks background.
	router.RecordActiveStart("p")

	ctx, cancel := context.WithCancel(context.Background())
	errChan := make(chan error, 1)
	go func() {
		errChan <- router.GateComplete(
			ctx, model.LatencyBackground, "p", "m", 0,
		)
	}()

	// Wait for the goroutine to enter WaitForIdle.
	router.backgroundScheduler.WaitForWaiters("p", 1)

	cancel()

	select {
	case err := <-errChan:
		if err != context.Canceled {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-t.Context().Done():
		t.Fatal("timed out waiting for cancelled gate")
	}

	router.RecordActiveEnd("p")
}

func TestLatencyRouterUnknownPolicy(t *testing.T) {
	router, _ := newTestRouter(t)
	provider := echoEmbedProvider()

	// Unknown policy for SubmitEmbed.
	_, err := router.SubmitEmbed(
		context.Background(),
		model.LatencyPolicy("turbo"),
		provider,
		"p", "m", "cred",
		[][]byte{[]byte("hello")},
		0, true,
	)
	if err == nil {
		t.Fatal("expected error for unknown policy")
	}

	// Unknown policy for GateComplete.
	err = router.GateComplete(
		context.Background(),
		model.LatencyPolicy("turbo"),
		"p", "m", 0,
	)
	if err == nil {
		t.Fatal("expected error for unknown policy")
	}
}

func TestLatencyRouterEmbedProviderError(t *testing.T) {
	router, _ := newTestRouter(t)

	providerErr := fmt.Errorf("inference engine crashed")
	provider := &mockProvider{
		embedFunc: func(_ context.Context, _ *modelprovider.EmbedRequest) (*modelprovider.EmbedResult, error) {
			return nil, providerErr
		},
	}

	// Immediate path: provider error propagates.
	_, err := router.SubmitEmbed(
		context.Background(),
		model.LatencyImmediate,
		provider,
		"p", "m", "cred",
		[][]byte{[]byte("hello")},
		0, true,
	)
	if err != providerErr {
		t.Fatalf("expected %v, got %v", providerErr, err)
	}
}

func TestLatencyRouterActiveRequestTracking(t *testing.T) {
	router, _ := newTestRouter(t)

	// RecordActiveStart/End should pass through to BackgroundScheduler.
	// Verify by checking that background waits for idle.
	router.RecordActiveStart("p")
	router.RecordActiveStart("p")

	proceedChan := make(chan struct{})
	go func() {
		_ = router.GateComplete(
			context.Background(), model.LatencyBackground,
			"p", "m", 0,
		)
		close(proceedChan)
	}()

	// Wait for the goroutine to enter WaitForIdle.
	router.backgroundScheduler.WaitForWaiters("p", 1)

	// End one — still blocked (1 remaining).
	router.RecordActiveEnd("p")
	// Give the goroutine a chance to wake and re-check.
	runtime.Gosched()

	select {
	case <-proceedChan:
		t.Fatal("should still be blocked (1 active request)")
	default:
	}

	// End the second — now idle.
	router.RecordActiveEnd("p")

	select {
	case <-proceedChan:
		// OK
	case <-t.Context().Done():
		t.Fatal("timed out waiting for background to proceed")
	}
}
