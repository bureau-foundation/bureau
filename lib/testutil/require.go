// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	"fmt"
	"time"
)

// RequireReceive reads one value from ch within timeout, or fails the
// test. This encapsulates the timeout safety valve pattern so that
// individual tests do not need direct time.After calls.
//
//	result := testutil.RequireReceive(t, ch, 5*time.Second, "waiting for result")
func RequireReceive[T any](t interface {
	Helper()
	Fatalf(format string, args ...any)
}, ch <-chan T, timeout time.Duration, msgAndArgs ...any) T {
	t.Helper()
	select {
	case v, ok := <-ch:
		if !ok {
			t.Fatalf("channel closed without sending a value: %s", formatMessage(msgAndArgs))
		}
		return v
	case <-time.After(timeout): //nolint:realclock test hang prevention
		t.Fatalf("timed out after %v: %s", timeout, formatMessage(msgAndArgs))
	}
	panic("unreachable")
}

// RequireSend sends v on ch within timeout, or fails the test.
//
//	testutil.RequireSend(t, ch, value, 5*time.Second, "sending value")
func RequireSend[T any](t interface {
	Helper()
	Fatalf(format string, args ...any)
}, ch chan<- T, v T, timeout time.Duration, msgAndArgs ...any) {
	t.Helper()
	select {
	case ch <- v:
	case <-time.After(timeout): //nolint:realclock test hang prevention
		t.Fatalf("timed out after %v: %s", timeout, formatMessage(msgAndArgs))
	}
}

// RequireClosed waits for ch to be closed (or receive a value) within
// timeout, or fails the test. Use this for readiness channels that
// signal by closing.
//
//	testutil.RequireClosed(t, server.Ready(), 5*time.Second, "server ready")
func RequireClosed(t interface {
	Helper()
	Fatalf(format string, args ...any)
}, ch <-chan struct{}, timeout time.Duration, msgAndArgs ...any) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(timeout): //nolint:realclock test hang prevention
		t.Fatalf("timed out after %v waiting for channel close: %s", timeout, formatMessage(msgAndArgs))
	}
}

// formatMessage formats optional message arguments into a string.
// Accepts either a single string or a format string followed by args.
func formatMessage(msgAndArgs []any) string {
	if len(msgAndArgs) == 0 {
		return "(no message)"
	}
	if len(msgAndArgs) == 1 {
		if s, ok := msgAndArgs[0].(string); ok {
			return s
		}
		return fmt.Sprintf("%v", msgAndArgs[0])
	}
	if format, ok := msgAndArgs[0].(string); ok {
		return fmt.Sprintf(format, msgAndArgs[1:]...)
	}
	return fmt.Sprintf("%v", msgAndArgs)
}
