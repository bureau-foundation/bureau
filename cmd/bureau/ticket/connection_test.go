// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
)

func TestAddResolvedRoomEmptyNoOp(t *testing.T) {
	t.Parallel()

	fields := map[string]any{
		"ticket": "tkt-a3f9",
	}

	err := addResolvedRoom(context.Background(), fields, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The fields map should be unchanged — no "room" key added.
	if _, exists := fields["room"]; exists {
		t.Error("empty room string should not add 'room' key to fields")
	}
	if fields["ticket"] != "tkt-a3f9" {
		t.Error("existing field was modified")
	}
}

func TestAddResolvedRoomWithRoomID(t *testing.T) {
	t.Parallel()

	fields := map[string]any{
		"ticket": "tkt-b2c1",
	}

	// Room IDs (! prefix) are resolved without network calls.
	err := addResolvedRoom(context.Background(), fields, "!xyz789:bureau.local")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	room, exists := fields["room"]
	if !exists {
		t.Fatal("expected 'room' key in fields")
	}
	if room != "!xyz789:bureau.local" {
		t.Errorf("room = %q, want %q", room, "!xyz789:bureau.local")
	}
}

func TestAddResolvedRoomMalformedRoomID(t *testing.T) {
	t.Parallel()

	fields := map[string]any{}

	err := addResolvedRoom(context.Background(), fields, "!no-server-part")
	if err == nil {
		t.Fatal("expected error for malformed room ID")
	}

	// Should not have added anything to fields on error.
	if _, exists := fields["room"]; exists {
		t.Error("should not add 'room' key on error")
	}
}

func TestAddResolvedRoomBareLocalpart(t *testing.T) {
	// Cannot use t.Parallel() — t.Setenv modifies process environment.

	// Provide a session file so LoadSession succeeds, but use an
	// unreachable homeserver so resolution fails. The error message
	// confirms the alias was constructed correctly.
	sessionDirectory := t.TempDir()
	sessionPath := filepath.Join(sessionDirectory, "session.json")
	session := cli.OperatorSession{
		UserID:      "@op:dev.bureau",
		AccessToken: "test-token",
		Homeserver:  "http://127.0.0.1:1",
	}
	data, err := json.Marshal(session)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(sessionPath, data, 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("BUREAU_SESSION_FILE", sessionPath)

	fields := map[string]any{}
	err = addResolvedRoom(context.Background(), fields, "prod/tickets")
	if err == nil {
		t.Fatal("expected error (no homeserver)")
	}

	// The error should contain the constructed alias, confirming
	// the bare localpart → full alias construction worked.
	if !strings.Contains(err.Error(), "#prod/tickets:dev.bureau") {
		t.Errorf("error = %q, should contain constructed alias", err)
	}
}
