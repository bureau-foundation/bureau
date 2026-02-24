// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveRoomEmpty(t *testing.T) {
	t.Parallel()

	_, err := ResolveRoom(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty room string")
	}
	if !strings.Contains(err.Error(), "--room is required") {
		t.Errorf("error = %q, want mention of --room", err)
	}
}

func TestResolveRoomValidRoomID(t *testing.T) {
	t.Parallel()

	roomID, err := ResolveRoom(context.Background(), "!abc123:bureau.local")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := roomID.String(); got != "!abc123:bureau.local" {
		t.Errorf("RoomID = %q, want %q", got, "!abc123:bureau.local")
	}
}

func TestResolveRoomMalformedRoomID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
	}{
		{"no server", "!abc123"},
		{"empty local part", "!:bureau.local"},
		{"empty server", "!abc:"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := ResolveRoom(context.Background(), test.input)
			if err == nil {
				t.Errorf("expected error for %q", test.input)
			}
		})
	}
}

func TestResolveRoomBareLocalpartConstructsAlias(t *testing.T) {
	// Cannot use t.Parallel() — t.Setenv modifies process environment.

	// Write a session file so LoadSession succeeds.
	sessionDirectory := t.TempDir()
	sessionPath := filepath.Join(sessionDirectory, "session.json")
	session := OperatorSession{
		UserID:      "@operator:test.bureau",
		AccessToken: "fake-token-for-test",
		Homeserver:  "http://127.0.0.1:1", // Unreachable — we expect a connection error.
	}
	data, err := json.Marshal(session)
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	if err := os.WriteFile(sessionPath, data, 0600); err != nil {
		t.Fatalf("write session: %v", err)
	}
	t.Setenv("BUREAU_SESSION_FILE", sessionPath)

	// Bare localpart "prod/tickets" should construct
	// "#prod/tickets:test.bureau" and then fail at alias resolution
	// (no homeserver running). The error message reveals the
	// constructed alias, confirming the construction logic.
	_, err = ResolveRoom(context.Background(), "prod/tickets")
	if err == nil {
		t.Fatal("expected error (no homeserver), got nil")
	}

	// The error should contain the constructed alias.
	errMessage := err.Error()
	if !strings.Contains(errMessage, "#prod/tickets:test.bureau") {
		t.Errorf("error = %q, should contain constructed alias #prod/tickets:test.bureau", errMessage)
	}
}

func TestResolveRoomFullAliasConstructsCorrectly(t *testing.T) {
	// Cannot use t.Parallel() — t.Setenv modifies process environment.

	// Write a session file (needed for the Matrix client creation
	// in the alias resolution path).
	sessionDirectory := t.TempDir()
	sessionPath := filepath.Join(sessionDirectory, "session.json")
	session := OperatorSession{
		UserID:      "@operator:test.bureau",
		AccessToken: "fake-token-for-test",
		Homeserver:  "http://127.0.0.1:1",
	}
	data, err := json.Marshal(session)
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	if err := os.WriteFile(sessionPath, data, 0600); err != nil {
		t.Fatalf("write session: %v", err)
	}
	t.Setenv("BUREAU_SESSION_FILE", sessionPath)

	// Full alias "#prod/tickets:other.server" should be parsed
	// as-is (no server derivation from session). It will fail at
	// resolution, but the error should contain the alias.
	_, err = ResolveRoom(context.Background(), "#prod/tickets:other.server")
	if err == nil {
		t.Fatal("expected error (no homeserver), got nil")
	}

	errMessage := err.Error()
	if !strings.Contains(errMessage, "#prod/tickets:other.server") {
		t.Errorf("error = %q, should contain alias #prod/tickets:other.server", errMessage)
	}
}

func TestResolveRoomInvalidAlias(t *testing.T) {
	// Cannot use t.Parallel() — t.Setenv modifies process environment.

	sessionDirectory := t.TempDir()
	sessionPath := filepath.Join(sessionDirectory, "session.json")
	session := OperatorSession{
		UserID:      "@operator:test.bureau",
		AccessToken: "fake-token-for-test",
		Homeserver:  "http://127.0.0.1:1",
	}
	data, err := json.Marshal(session)
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	if err := os.WriteFile(sessionPath, data, 0600); err != nil {
		t.Fatalf("write session: %v", err)
	}
	t.Setenv("BUREAU_SESSION_FILE", sessionPath)

	// "#" alone with no localpart or server should fail at alias parsing.
	_, err = ResolveRoom(context.Background(), "#")
	if err == nil {
		t.Fatal("expected error for bare '#'")
	}
}

func TestResolveRoomNoSessionForBareLocalpart(t *testing.T) {
	// Cannot use t.Parallel() — t.Setenv modifies process environment.

	// Point to a nonexistent session file.
	t.Setenv("BUREAU_SESSION_FILE", filepath.Join(t.TempDir(), "nonexistent.json"))

	_, err := ResolveRoom(context.Background(), "prod/tickets")
	if err == nil {
		t.Fatal("expected error when session file is missing")
	}

	// Should mention "bureau login" since the session is missing.
	if !strings.Contains(err.Error(), "bureau login") {
		t.Errorf("error = %q, should mention 'bureau login'", err)
	}
}
