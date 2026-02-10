// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package matrix

import (
	"context"
	"testing"
)

func TestSendCommand_MissingArgs(t *testing.T) {
	command := SendCommand()
	// No positional args after flags — should fail.
	err := command.Execute([]string{"--homeserver", "http://localhost:6167", "--token", "tok", "--user-id", "@a:b"})
	if err == nil {
		t.Fatal("expected error for missing room and message args")
	}
}

func TestSendCommand_OneArg(t *testing.T) {
	command := SendCommand()
	// Only room, no message.
	err := command.Execute([]string{"--homeserver", "http://localhost:6167", "--token", "tok", "--user-id", "@a:b", "!room:local"})
	if err == nil {
		t.Fatal("expected error for missing message arg")
	}
}

func TestSendCommand_TooManyArgs(t *testing.T) {
	command := SendCommand()
	err := command.Execute([]string{
		"--homeserver", "http://localhost:6167", "--token", "tok", "--user-id", "@a:b",
		"!room:local", "hello", "extra",
	})
	if err == nil {
		t.Fatal("expected error for too many args")
	}
}

func TestResolveRoom_Alias(t *testing.T) {
	// resolveRoom with an alias requires a real session; we just test the
	// validation branch for non-alias, non-room-ID input.
	_, err := resolveRoom(context.Background(), nil, "invalid")
	if err == nil {
		t.Fatal("expected error for invalid room target")
	}
}

func TestResolveRoom_RoomID(t *testing.T) {
	roomID, err := resolveRoom(context.Background(), nil, "!abc:local")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if roomID != "!abc:local" {
		t.Errorf("expected !abc:local, got %s", roomID)
	}
}
