// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package matrix

import (
	"encoding/json"
	"testing"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
)

func TestStateGetCommand_MissingRoom(t *testing.T) {
	command := StateCommand()
	err := command.Execute([]string{
		"get", "--homeserver", "http://localhost:6167", "--token", "tok", "--user-id", "@a:b",
	})
	if err == nil {
		t.Fatal("expected error for missing room arg")
	}
}

func TestStateGetCommand_TooManyArgs(t *testing.T) {
	command := StateCommand()
	err := command.Execute([]string{
		"get", "--homeserver", "http://localhost:6167", "--token", "tok", "--user-id", "@a:b",
		"!room:local", "m.room.topic", "extra",
	})
	if err == nil {
		t.Fatal("expected error for too many args")
	}
}

func TestStateSetCommand_MissingArgs(t *testing.T) {
	command := StateCommand()
	// Only room, no event type or body.
	err := command.Execute([]string{
		"set", "--homeserver", "http://localhost:6167", "--token", "tok", "--user-id", "@a:b",
		"!room:local",
	})
	if err == nil {
		t.Fatal("expected error for missing event type and JSON body")
	}
}

func TestStateSetCommand_MissingBody(t *testing.T) {
	command := StateCommand()
	// Room and event type but no body.
	err := command.Execute([]string{
		"set", "--homeserver", "http://localhost:6167", "--token", "tok", "--user-id", "@a:b",
		"!room:local", "m.room.topic",
	})
	if err == nil {
		t.Fatal("expected error for missing JSON body")
	}
}

func TestStateSetCommand_InvalidJSON(t *testing.T) {
	command := StateCommand()
	err := command.Execute([]string{
		"set", "--homeserver", "http://localhost:6167", "--token", "tok", "--user-id", "@a:b",
		"!room:local", "m.room.topic", "not valid json",
	})
	if err == nil {
		t.Fatal("expected error for invalid JSON body")
	}
}

func TestStateSetCommand_TooManyArgs(t *testing.T) {
	command := StateCommand()
	err := command.Execute([]string{
		"set", "--homeserver", "http://localhost:6167", "--token", "tok", "--user-id", "@a:b",
		"!room:local", "m.room.topic", `{"topic":"test"}`, "extra",
	})
	if err == nil {
		t.Fatal("expected error for too many args")
	}
}

func TestStateSetCommand_StdinConflictsWithPositionalBody(t *testing.T) {
	command := StateCommand()
	err := command.Execute([]string{
		"set", "--homeserver", "http://localhost:6167", "--token", "tok", "--user-id", "@a:b",
		"--stdin", "!room:local", "m.room.topic", `{"topic":"test"}`,
	})
	if err == nil {
		t.Fatal("expected error when --stdin is used with positional JSON body")
	}
}

func TestWriteJSON_ValidStruct(t *testing.T) {
	value := map[string]string{"key": "value"}
	err := cli.WriteJSON(value)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWriteJSON_RawMessage(t *testing.T) {
	raw := json.RawMessage(`{"nested": true}`)
	err := cli.WriteJSON(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
