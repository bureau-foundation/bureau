// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/messaging"
)

// TestWebRTCTransport_DialAndServe creates two WebRTCTransports connected
// via an in-process MemorySignaler and verifies that an HTTP request
// round-trips through a WebRTC data channel.
func TestWebRTCTransport_DialAndServe(t *testing.T) {
	signaler := NewMemorySignaler()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	// Empty ICE config means host candidates only (loopback).
	config := ICEConfig{}

	// Transport A (client/dialer side).
	transportA := NewWebRTCTransport(signaler, "machine/alpha", config, logger)
	defer transportA.Close()

	// Transport B (server/listener side).
	transportB := NewWebRTCTransport(signaler, "machine/beta", config, logger)
	defer transportB.Close()

	// Transport B serves HTTP.
	received := make(chan string, 1)
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		received <- request.URL.Path
		writer.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(writer, "hello from beta")
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go transportB.Serve(ctx, handler)

	// Give the signaling poller time to start.
	time.Sleep(100 * time.Millisecond)

	// Transport A dials Transport B.
	roundTripper := HTTPTransport(transportA, "machine/beta")
	client := &http.Client{
		Transport: roundTripper,
		Timeout:   30 * time.Second,
	}

	response, err := client.Get("http://transport/http/test-service/v1/hello")
	if err != nil {
		t.Fatalf("GET error: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}

	body, _ := io.ReadAll(response.Body)
	if string(body) != "hello from beta" {
		t.Errorf("body = %q, want %q", string(body), "hello from beta")
	}

	// Verify the server received the correct path.
	select {
	case path := <-received:
		if path != "/http/test-service/v1/hello" {
			t.Errorf("server received path = %q, want %q", path, "/http/test-service/v1/hello")
		}
	case <-time.After(5 * time.Second):
		t.Error("server did not receive request")
	}
}

// TestWebRTCTransport_SequentialRequests sends multiple sequential requests
// through the same PeerConnection, verifying that data channels are reusable
// and each request gets the correct response.
func TestWebRTCTransport_SequentialRequests(t *testing.T) {
	signaler := NewMemorySignaler()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	config := ICEConfig{}

	transportA := NewWebRTCTransport(signaler, "machine/alpha", config, logger)
	defer transportA.Close()

	transportB := NewWebRTCTransport(signaler, "machine/beta", config, logger)
	defer transportB.Close()

	// Server echoes the request path back in the response body.
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(writer, request.URL.Path)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go transportB.Serve(ctx, handler)

	time.Sleep(100 * time.Millisecond)

	roundTripper := HTTPTransport(transportA, "machine/beta")
	client := &http.Client{
		Transport: roundTripper,
		Timeout:   30 * time.Second,
	}

	for index := 0; index < 3; index++ {
		path := fmt.Sprintf("/request/%d", index)
		response, err := client.Get("http://transport" + path)
		if err != nil {
			t.Fatalf("request %d: GET error: %v", index, err)
		}

		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if string(body) != path {
			t.Errorf("request %d: body = %q, want %q", index, string(body), path)
		}
	}
}

// TestWebRTCTransport_ConcurrentDials opens multiple data channels to the
// same peer concurrently. Before the getOrCreatePeer fix, concurrent callers
// would each try to establish their own PeerConnection (overwriting each
// other's offers), causing all but one to hang. After the fix, concurrent
// callers share a single PeerConnection establishment attempt.
func TestWebRTCTransport_ConcurrentDials(t *testing.T) {
	signaler := NewMemorySignaler()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	config := ICEConfig{}

	transportA := NewWebRTCTransport(signaler, "machine/alpha", config, logger)
	defer transportA.Close()

	transportB := NewWebRTCTransport(signaler, "machine/beta", config, logger)
	defer transportB.Close()

	// Server echoes the request path back in the response body.
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(writer, request.URL.Path)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go transportB.Serve(ctx, handler)

	time.Sleep(100 * time.Millisecond)

	const concurrency = 5
	var waitGroup sync.WaitGroup
	errors := make(chan error, concurrency)

	for index := 0; index < concurrency; index++ {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			// Each goroutine gets its own RoundTripper and client so that
			// Go's http.Transport doesn't serialize them through a single
			// connection pool.
			roundTripper := HTTPTransport(transportA, "machine/beta")
			client := &http.Client{
				Transport: roundTripper,
				Timeout:   30 * time.Second,
			}

			path := fmt.Sprintf("/request/%d", index)
			response, err := client.Get("http://transport" + path)
			if err != nil {
				errors <- fmt.Errorf("request %d: %w", index, err)
				return
			}
			defer response.Body.Close()

			body, _ := io.ReadAll(response.Body)
			if string(body) != path {
				errors <- fmt.Errorf("request %d: body = %q, want %q", index, string(body), path)
			}
		}(index)
	}

	waitGroup.Wait()
	close(errors)

	for err := range errors {
		t.Error(err)
	}
}

// TestWebRTCTransport_Address verifies that Address() returns the localpart.
func TestWebRTCTransport_Address(t *testing.T) {
	signaler := NewMemorySignaler()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	wt := NewWebRTCTransport(signaler, "machine/workstation", ICEConfig{}, logger)
	defer wt.Close()

	if address := wt.Address(); address != "machine/workstation" {
		t.Errorf("Address() = %q, want %q", address, "machine/workstation")
	}
}

// TestWebRTCTransport_DialAfterClose verifies that DialContext returns an
// error after the transport is closed.
func TestWebRTCTransport_DialAfterClose(t *testing.T) {
	signaler := NewMemorySignaler()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	wt := NewWebRTCTransport(signaler, "machine/alpha", ICEConfig{}, logger)
	wt.Close()

	_, err := wt.DialContext(context.Background(), "machine/beta")
	if err == nil {
		t.Fatal("expected error from DialContext after Close, got nil")
	}
}

// TestWebRTCTransport_BidirectionalHTTP verifies that both sides can serve
// and dial each other. After A connects to B, B should be able to open
// data channels back to A over the same PeerConnection.
func TestWebRTCTransport_BidirectionalHTTP(t *testing.T) {
	signaler := NewMemorySignaler()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	config := ICEConfig{}

	transportA := NewWebRTCTransport(signaler, "machine/alpha", config, logger)
	defer transportA.Close()

	transportB := NewWebRTCTransport(signaler, "machine/beta", config, logger)
	defer transportB.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Both sides serve HTTP.
	handlerA := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fmt.Fprint(writer, "from-alpha")
	})
	handlerB := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fmt.Fprint(writer, "from-beta")
	})

	go transportA.Serve(ctx, handlerA)
	go transportB.Serve(ctx, handlerB)

	time.Sleep(100 * time.Millisecond)

	// A → B
	clientAtoB := &http.Client{
		Transport: HTTPTransport(transportA, "machine/beta"),
		Timeout:   30 * time.Second,
	}
	responseAtoB, err := clientAtoB.Get("http://transport/test")
	if err != nil {
		t.Fatalf("A→B GET error: %v", err)
	}
	bodyAtoB, _ := io.ReadAll(responseAtoB.Body)
	responseAtoB.Body.Close()
	if string(bodyAtoB) != "from-beta" {
		t.Errorf("A→B body = %q, want %q", string(bodyAtoB), "from-beta")
	}

	// B → A (uses the PeerConnection that A already established, or creates
	// a new one via signaling).
	clientBtoA := &http.Client{
		Transport: HTTPTransport(transportB, "machine/alpha"),
		Timeout:   30 * time.Second,
	}
	responseBtoA, err := clientBtoA.Get("http://transport/test")
	if err != nil {
		t.Fatalf("B→A GET error: %v", err)
	}
	bodyBtoA, _ := io.ReadAll(responseBtoA.Body)
	responseBtoA.Body.Close()
	if string(bodyBtoA) != "from-alpha" {
		t.Errorf("B→A body = %q, want %q", string(bodyBtoA), "from-alpha")
	}
}

// TestWebRTCTransport_UpdateICEConfig verifies that UpdateICEConfig
// replaces the config used for future PeerConnections.
func TestWebRTCTransport_UpdateICEConfig(t *testing.T) {
	signaler := NewMemorySignaler()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	wt := NewWebRTCTransport(signaler, "machine/alpha", ICEConfig{}, logger)
	defer wt.Close()

	// Initially empty.
	wt.configMu.RLock()
	if len(wt.iceConfig.Servers) != 0 {
		t.Errorf("initial servers = %d, want 0", len(wt.iceConfig.Servers))
	}
	wt.configMu.RUnlock()

	// Update.
	wt.UpdateICEConfig(ICEConfigFromTURN(&messaging.TURNCredentialsResponse{
		Username: "user",
		Password: "pass",
		URIs:     []string{"turn:turn.local:3478"},
		TTL:      86400,
	}))

	wt.configMu.RLock()
	if len(wt.iceConfig.Servers) != 1 {
		t.Errorf("updated servers = %d, want 1", len(wt.iceConfig.Servers))
	}
	wt.configMu.RUnlock()
}

// TestMemorySignaler_PublishAndPoll verifies the in-process signaler
// correctly stores and retrieves offers and answers.
func TestMemorySignaler_PublishAndPoll(t *testing.T) {
	signaler := NewMemorySignaler()
	ctx := context.Background()

	// Publish an offer from A to B.
	if err := signaler.PublishOffer(ctx, "machine/a", "machine/b", "offer-sdp"); err != nil {
		t.Fatalf("PublishOffer failed: %v", err)
	}

	// B polls for offers.
	offers, err := signaler.PollOffers(ctx, "machine/b")
	if err != nil {
		t.Fatalf("PollOffers failed: %v", err)
	}
	if len(offers) != 1 {
		t.Fatalf("expected 1 offer, got %d", len(offers))
	}
	if offers[0].PeerLocalpart != "machine/a" {
		t.Errorf("PeerLocalpart = %q, want %q", offers[0].PeerLocalpart, "machine/a")
	}
	if offers[0].SDP != "offer-sdp" {
		t.Errorf("SDP = %q, want %q", offers[0].SDP, "offer-sdp")
	}

	// Polling again returns nothing (already seen).
	offers, err = signaler.PollOffers(ctx, "machine/b")
	if err != nil {
		t.Fatalf("second PollOffers failed: %v", err)
	}
	if len(offers) != 0 {
		t.Errorf("expected 0 offers on second poll, got %d", len(offers))
	}

	// Publish an answer from B to A.
	if err := signaler.PublishAnswer(ctx, "machine/a", "machine/b", "answer-sdp"); err != nil {
		t.Fatalf("PublishAnswer failed: %v", err)
	}

	// A polls for answers.
	answers, err := signaler.PollAnswers(ctx, "machine/a")
	if err != nil {
		t.Fatalf("PollAnswers failed: %v", err)
	}
	if len(answers) != 1 {
		t.Fatalf("expected 1 answer, got %d", len(answers))
	}
	if answers[0].PeerLocalpart != "machine/b" {
		t.Errorf("PeerLocalpart = %q, want %q", answers[0].PeerLocalpart, "machine/b")
	}
	if answers[0].SDP != "answer-sdp" {
		t.Errorf("SDP = %q, want %q", answers[0].SDP, "answer-sdp")
	}
}

func TestMemorySignaler_IndependentConsumers(t *testing.T) {
	signaler := NewMemorySignaler()
	ctx := context.Background()

	// Publish an offer from A to both B and C (different keys).
	if err := signaler.PublishOffer(ctx, "machine/a", "machine/b", "offer-for-b"); err != nil {
		t.Fatalf("PublishOffer to B failed: %v", err)
	}

	// B sees the offer.
	offers, err := signaler.PollOffers(ctx, "machine/b")
	if err != nil {
		t.Fatalf("PollOffers for B failed: %v", err)
	}
	if len(offers) != 1 {
		t.Errorf("expected 1 offer for B, got %d", len(offers))
	}

	// C should not see an offer directed at B.
	offers, err = signaler.PollOffers(ctx, "machine/c")
	if err != nil {
		t.Fatalf("PollOffers for C failed: %v", err)
	}
	if len(offers) != 0 {
		t.Errorf("expected 0 offers for C, got %d", len(offers))
	}
}
