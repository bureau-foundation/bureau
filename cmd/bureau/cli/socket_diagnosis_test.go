// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"
)

func TestDiagnoseSocketError_NotPermissionDenied(t *testing.T) {
	// Non-EACCES errors should return nil — caller handles them.
	err := fmt.Errorf("connection refused: %w", syscall.ECONNREFUSED)
	if result := DiagnoseSocketError(err, "/run/bureau/observe.sock"); result != nil {
		t.Fatalf("expected nil for ECONNREFUSED, got: %v", result)
	}
}

func TestDiagnoseSocketError_WrappedEACCES(t *testing.T) {
	// Simulates the error chain from observe.Connect → net.Dial → EACCES.
	// The EACCES is wrapped several layers deep, but errors.Is traverses
	// the chain.
	inner := &net.OpError{
		Op:  "dial",
		Net: "unix",
		Addr: &net.UnixAddr{
			Name: "/run/bureau/observe.sock",
			Net:  "unix",
		},
		Err: syscall.EACCES,
	}
	wrapped := fmt.Errorf("dial daemon socket: %w", inner)

	result := DiagnoseSocketError(wrapped, "/run/bureau/observe.sock")
	if result == nil {
		t.Fatal("expected non-nil diagnosis for EACCES")
	}

	// The error should be categorized as Forbidden.
	if result.Category != CategoryForbidden {
		t.Fatalf("expected category %q, got %q", CategoryForbidden, result.Category)
	}

	// The hint should be non-empty and actionable.
	if result.Hint == "" {
		t.Fatal("expected non-empty hint")
	}
}

func TestDiagnoseSocketError_EPERM(t *testing.T) {
	// EPERM should also be diagnosed (some environments return EPERM
	// instead of EACCES for socket operations).
	err := fmt.Errorf("operation failed: %w", syscall.EPERM)
	result := DiagnoseSocketError(err, "/some/socket.sock")
	if result == nil {
		t.Fatal("expected non-nil diagnosis for EPERM")
	}
	if result.Category != CategoryForbidden {
		t.Fatalf("expected category %q, got %q", CategoryForbidden, result.Category)
	}
}

func TestDiagnoseSocketError_NilError(t *testing.T) {
	// nil error should be handled gracefully.
	if result := DiagnoseSocketError(nil, "/run/bureau/observe.sock"); result != nil {
		t.Fatalf("expected nil for nil error, got: %v", result)
	}
}

func TestDiagnoseSocketError_PlainError(t *testing.T) {
	// A plain error without any syscall wrapping.
	err := errors.New("something went wrong")
	if result := DiagnoseSocketError(err, "/run/bureau/observe.sock"); result != nil {
		t.Fatalf("expected nil for plain error, got: %v", result)
	}
}
