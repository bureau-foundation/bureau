// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/messaging"
)

// ticketStatusResult mirrors the ticket service's unauthenticated
// "status" response for CBOR decoding in tests. Only includes
// liveness information — no room or ticket data.
type ticketStatusResult struct {
	UptimeSeconds float64 `cbor:"uptime_seconds" json:"uptime_seconds"`
}

// ticketCreateResult mirrors the ticket service's "create" response
// for CBOR decoding in tests.
type ticketCreateResult struct {
	ID   string `json:"id"`
	Room string `json:"room"`
}

// TestTicketServiceEndToEnd exercises the full path from socket
// connection through the ticket service to Matrix state event
// verification:
//
//   - Boot a machine (launcher + daemon)
//   - Start the ticket service binary with a registered Matrix account
//   - Verify unauthenticated connectivity (status action)
//   - Mint a service token and create a ticket (authenticated create action)
//   - Verify the ticket appears as an m.bureau.ticket state event in Matrix
func TestTicketServiceEndToEnd(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()

	ctx := t.Context()

	// --- Setup: Boot a machine ---
	//
	// The machine provides the run directory (where the ticket service
	// creates its socket) and the token signing keypair (which the daemon
	// publishes to #bureau/system for services to verify tokens).
	machine := newTestMachine(t, "machine/ticket-e2e")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
	})

	// --- Setup: Register the ticket service Matrix account ---
	ticketServiceLocalpart := "service/ticket/integ"
	ticketServiceAccount := registerPrincipal(t, ticketServiceLocalpart, "ticket-svc-password")

	// --- Setup: Write session.json for the ticket service ---
	//
	// The ticket service loads its Matrix credentials from session.json
	// in its state directory. In production, the launcher writes this
	// file; in tests, we create it directly from the registered account.
	ticketStateDir := t.TempDir()
	sessionData := service.SessionData{
		HomeserverURL: testHomeserverURL,
		UserID:        ticketServiceAccount.UserID,
		AccessToken:   ticketServiceAccount.Token,
	}
	sessionJSON, err := json.Marshal(sessionData)
	if err != nil {
		t.Fatalf("marshal session data: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ticketStateDir, "session.json"), sessionJSON, 0600); err != nil {
		t.Fatalf("write session.json: %v", err)
	}

	// --- Setup: Invite ticket service to global rooms ---
	//
	// The ticket service resolves and joins #bureau/system (to load the
	// token signing key) and #bureau/service (to register itself). These
	// are private rooms, so the admin must invite the service first.
	systemRoomID, err := admin.ResolveAlias(ctx, schema.FullRoomAlias(schema.RoomAliasSystem, testServerName))
	if err != nil {
		t.Fatalf("resolve system room: %v", err)
	}
	serviceRoomID, err := admin.ResolveAlias(ctx, schema.FullRoomAlias(schema.RoomAliasService, testServerName))
	if err != nil {
		t.Fatalf("resolve service room: %v", err)
	}

	if err := admin.InviteUser(ctx, systemRoomID, ticketServiceAccount.UserID); err != nil {
		t.Fatalf("invite ticket service to system room: %v", err)
	}
	if err := admin.InviteUser(ctx, serviceRoomID, ticketServiceAccount.UserID); err != nil {
		t.Fatalf("invite ticket service to service room: %v", err)
	}

	// --- Setup: Create a project room with ticket configuration ---
	//
	// The ticket service tracks rooms that have m.bureau.ticket_config.
	// It discovers these during initial /sync. We also publish the
	// m.bureau.room_service binding so the daemon knows which service
	// principal handles the "ticket" role in this room.
	adminUserID := "@bureau-admin:" + testServerName
	projectRoom, err := admin.CreateRoom(ctx, messaging.CreateRoomRequest{
		Name:   "ticket-e2e-project",
		Invite: []string{ticketServiceAccount.UserID},
		PowerLevelContentOverride: map[string]any{
			"users": map[string]any{
				adminUserID:                 100,
				ticketServiceAccount.UserID: 10,
			},
			"events": map[string]any{
				schema.EventTypeTicket:       10,
				schema.EventTypeTicketConfig: 100,
				schema.EventTypeRoomService:  100,
			},
		},
	})
	if err != nil {
		t.Fatalf("create project room: %v", err)
	}
	projectRoomID := projectRoom.RoomID

	_, err = admin.SendStateEvent(ctx, projectRoomID, schema.EventTypeTicketConfig, "",
		schema.TicketConfigContent{
			Version: schema.TicketConfigVersion,
			Prefix:  "tkt",
		})
	if err != nil {
		t.Fatalf("publish ticket config: %v", err)
	}

	_, err = admin.SendStateEvent(ctx, projectRoomID, schema.EventTypeRoomService, "ticket",
		schema.RoomServiceContent{
			Principal: ticketServiceAccount.UserID,
		})
	if err != nil {
		t.Fatalf("publish room service binding: %v", err)
	}

	// --- Setup: Start the ticket service binary ---
	//
	// The launcher creates socket parent directories for principals it
	// manages, but the ticket service is started directly (not through
	// the launcher). Create the parent directory for the socket path.
	socketPath := principal.RunDirSocketPath(machine.RunDir, ticketServiceLocalpart)
	if err := os.MkdirAll(filepath.Dir(socketPath), 0755); err != nil {
		t.Fatalf("create socket parent directory: %v", err)
	}

	ticketBinary := resolvedBinary(t, "TICKET_SERVICE_BINARY")
	startProcess(t, "ticket-service", ticketBinary,
		"--homeserver", testHomeserverURL,
		"--machine-name", machine.Name,
		"--principal-name", ticketServiceLocalpart,
		"--server-name", testServerName,
		"--run-dir", machine.RunDir,
		"--state-dir", ticketStateDir,
	)

	// Wait for the ticket service socket to appear. The service creates
	// the socket after completing initial sync, loading the signing key,
	// and registering in #bureau/service.
	waitForFile(t, socketPath, 15*time.Second)

	// --- Phase 1: Unauthenticated connectivity ---
	//
	// The "status" action requires no authentication and returns only
	// liveness information (uptime). This verifies the socket is
	// reachable and the CBOR protocol is working.
	t.Run("UnauthenticatedStatus", func(t *testing.T) {
		unauthClient := service.NewServiceClientFromToken(socketPath, nil)
		var status ticketStatusResult
		if err := unauthClient.Call(ctx, "status", nil, &status); err != nil {
			t.Fatalf("status call failed: %v", err)
		}
		if status.UptimeSeconds <= 0 {
			t.Errorf("uptime = %f, want > 0", status.UptimeSeconds)
		}
	})

	// --- Phase 2: Authenticated ticket creation ---
	//
	// Load the daemon's token signing private key from the machine's
	// state directory. In production, the daemon mints tokens at
	// sandbox creation time. For this test, we mint one directly.
	_, privateKey, err := servicetoken.LoadKeypair(machine.StateDir)
	if err != nil {
		t.Fatalf("load token signing keypair: %v", err)
	}

	tokenBytes, err := servicetoken.Mint(privateKey, &servicetoken.Token{
		Subject:   "test/consumer",
		Machine:   machine.Name,
		Audience:  "ticket",
		Grants:    []servicetoken.Grant{{Actions: []string{"ticket/create"}}},
		ID:        "integ-test-ticket-create",
		IssuedAt:  1735689600, // 2025-01-01T00:00:00Z
		ExpiresAt: 4070908800, // 2099-01-01T00:00:00Z
	})
	if err != nil {
		t.Fatalf("mint service token: %v", err)
	}

	authedClient := service.NewServiceClientFromToken(socketPath, tokenBytes)

	var createResult ticketCreateResult
	err = authedClient.Call(ctx, "create", map[string]any{
		"room":     projectRoomID,
		"title":    "Test ticket from integration",
		"type":     "task",
		"priority": 2,
	}, &createResult)
	if err != nil {
		t.Fatalf("create ticket: %v", err)
	}

	if createResult.ID == "" {
		t.Fatal("create returned empty ticket ID")
	}
	if createResult.Room != projectRoomID {
		t.Errorf("create room = %q, want %q", createResult.Room, projectRoomID)
	}
	t.Logf("created ticket %s in room %s", createResult.ID, createResult.Room)

	// --- Phase 3: Verify Matrix state event ---
	//
	// The ticket service wrote the ticket as an m.bureau.ticket state
	// event in the project room. The admin session reads it back via
	// the Matrix client-server API to verify the full round-trip:
	// socket request → ticket service → Matrix state event.
	raw, err := admin.GetStateEvent(ctx, projectRoomID, schema.EventTypeTicket, createResult.ID)
	if err != nil {
		t.Fatalf("get ticket state event: %v", err)
	}

	var ticketContent schema.TicketContent
	if err := json.Unmarshal(raw, &ticketContent); err != nil {
		t.Fatalf("unmarshal ticket content: %v", err)
	}

	if ticketContent.Title != "Test ticket from integration" {
		t.Errorf("title = %q, want %q", ticketContent.Title, "Test ticket from integration")
	}
	if ticketContent.Status != "open" {
		t.Errorf("status = %q, want %q", ticketContent.Status, "open")
	}
	if ticketContent.Type != "task" {
		t.Errorf("type = %q, want %q", ticketContent.Type, "task")
	}
	if ticketContent.Priority != 2 {
		t.Errorf("priority = %d, want %d", ticketContent.Priority, 2)
	}
	if ticketContent.Version != schema.TicketContentVersion {
		t.Errorf("version = %d, want %d", ticketContent.Version, schema.TicketContentVersion)
	}
	if ticketContent.CreatedBy == "" {
		t.Error("created_by is empty")
	}
	if ticketContent.CreatedAt == "" {
		t.Error("created_at is empty")
	}

	t.Logf("ticket %s verified in Matrix: title=%q status=%s type=%s priority=%d",
		createResult.ID, ticketContent.Title, ticketContent.Status,
		ticketContent.Type, ticketContent.Priority)
}
