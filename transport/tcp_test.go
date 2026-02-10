// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestTCPListener_Address(t *testing.T) {
	listener, err := NewTCPListener("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewTCPListener() error: %v", err)
	}
	defer listener.Close()

	address := listener.Address()
	if address == "" {
		t.Error("Address() returned empty string")
	}
	if !strings.Contains(address, ":") {
		t.Errorf("Address() = %q, expected host:port format", address)
	}
}

func TestTCPRoundTrip(t *testing.T) {
	// Start a TCP listener with a test handler that echoes the request.
	listener, err := NewTCPListener("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewTCPListener() error: %v", err)
	}
	defer listener.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "%s %s", r.Method, r.URL.Path)
	})

	go listener.Serve(ctx, handler)

	// Use the dialer to create an HTTP client targeting the listener.
	dialer := &TCPDialer{}
	client := &http.Client{
		Transport: HTTPTransport(dialer, listener.Address()),
		Timeout:   5 * time.Second,
	}

	// The hostname in the URL is irrelevant — the dialer routes to the
	// listener's address regardless. This mirrors how the relay works:
	// the URL host doesn't determine the connection target.
	response, err := client.Get("http://ignored-host/http/test-service/v1/hello")
	if err != nil {
		t.Fatalf("GET error: %v", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("ReadAll() error: %v", err)
	}

	want := "GET /http/test-service/v1/hello"
	if string(body) != want {
		t.Errorf("response body = %q, want %q", string(body), want)
	}
}

func TestTCPRoundTrip_POST(t *testing.T) {
	listener, err := NewTCPListener("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewTCPListener() error: %v", err)
	}
	defer listener.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "body=%s", string(body))
	})

	go listener.Serve(ctx, handler)

	dialer := &TCPDialer{}
	client := &http.Client{
		Transport: HTTPTransport(dialer, listener.Address()),
		Timeout:   5 * time.Second,
	}

	response, err := client.Post(
		"http://localhost/http/stt/v1/transcribe",
		"application/json",
		strings.NewReader(`{"audio":"base64data"}`),
	)
	if err != nil {
		t.Fatalf("POST error: %v", err)
	}
	defer response.Body.Close()

	body, _ := io.ReadAll(response.Body)
	want := `body={"audio":"base64data"}`
	if string(body) != want {
		t.Errorf("response body = %q, want %q", string(body), want)
	}
}

func TestTCPDialer_ConnectionRefused(t *testing.T) {
	dialer := &TCPDialer{Timeout: time.Second}

	// Port 1 is almost certainly not listening.
	_, err := dialer.DialContext(context.Background(), "127.0.0.1:1")
	if err == nil {
		t.Error("expected error connecting to non-listening port")
	}
}

func TestTCPDialer_ContextCancellation(t *testing.T) {
	dialer := &TCPDialer{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	_, err := dialer.DialContext(ctx, "127.0.0.1:1")
	if err == nil {
		t.Error("expected error with cancelled context")
	}
}

func TestTCPListener_ContextCancellation(t *testing.T) {
	listener, err := NewTCPListener("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewTCPListener() error: %v", err)
	}
	defer listener.Close()

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- listener.Serve(ctx, http.NotFoundHandler())
	}()

	// Cancel the context — Serve should return cleanly.
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve() returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Serve() did not return after context cancellation")
	}
}

func TestHTTPTransport_Interface(t *testing.T) {
	listener, err := NewTCPListener("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewTCPListener() error: %v", err)
	}
	defer listener.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go listener.Serve(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// HTTPTransport returns an http.RoundTripper — verify it works.
	dialer := &TCPDialer{}
	roundTripper := HTTPTransport(dialer, listener.Address())

	request, _ := http.NewRequest("GET", "http://any-host/test", nil)
	response, err := roundTripper.RoundTrip(request)
	if err != nil {
		t.Fatalf("RoundTrip() error: %v", err)
	}
	response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", response.StatusCode, http.StatusOK)
	}
}
