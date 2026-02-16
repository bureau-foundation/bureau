// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/service"
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

// TestTicketServiceEndToEnd exercises the full production path for the
// ticket service: daemon service discovery, consumer sandbox creation
// with service mount resolution, daemon-minted token authentication,
// and ticket creation verified via Matrix state events.
//
// The test validates the entire daemon-mediated path:
//
//   - Ticket service starts externally, self-registers in #bureau/service
//   - Daemon discovers the service via /sync of the service room
//   - Consumer agent is deployed with a template declaring
//     required_services: ["ticket"]
//   - Daemon resolves the ticket service socket via m.bureau.room_service
//     in the consumer's workspace room, mints a service token, and
//     creates the sandbox with the service socket bind-mounted
//   - The daemon-minted token authenticates to the ticket service
//   - Ticket creation writes an m.bureau.ticket state event to Matrix
func TestTicketServiceEndToEnd(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()

	ctx := t.Context()

	testAgentBinary := resolvedBinary(t, "TEST_AGENT_BINARY")

	// --- Phase 0: Boot a machine ---
	//
	// ProxyBinary is required because we deploy a consumer sandbox
	// later. The machine provides the run directory (ticket service
	// socket), token signing keypair (daemon publishes to #bureau/system
	// for services to verify tokens), and the state directory (where
	// the daemon writes minted service tokens).
	machine := newTestMachine(t, "machine/ticket-e2e")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
	})

	// --- Phase 1: Ticket service setup ---

	// Register the ticket service Matrix account.
	ticketServiceLocalpart := "service/ticket/integ"
	ticketServiceAccount := registerPrincipal(t, ticketServiceLocalpart, "ticket-svc-password")

	// Write session.json for the ticket service. The service loads its
	// Matrix credentials from this file at startup.
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

	// Invite ticket service to global rooms. The service needs
	// #bureau/system (token signing key) and #bureau/service (to
	// publish its registration).
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

	// Create a project room with ticket configuration and room service
	// binding. This simulates what "bureau ticket enable" configures in
	// each workspace room. The machine is invited so the daemon can
	// read the m.bureau.room_service binding during resolveServiceSocket.
	adminUserID := "@bureau-admin:" + testServerName
	projectRoom, err := admin.CreateRoom(ctx, messaging.CreateRoomRequest{
		Name:   "ticket-e2e-project",
		Invite: []string{ticketServiceAccount.UserID, machine.UserID},
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

	// Set up a room watch BEFORE starting the ticket service. The
	// ticket service publishes its m.bureau.service registration during
	// startup, and the daemon posts a "Service directory updated" message
	// to the config room after processing the registration.
	serviceWatch := watchRoom(t, admin, machine.ConfigRoomID)

	// Start the ticket service binary. Service principals are externally
	// managed (not started by the daemon/launcher) — this is the
	// production pattern.
	ticketSocketPath := principal.RunDirSocketPath(machine.RunDir, ticketServiceLocalpart)
	if err := os.MkdirAll(filepath.Dir(ticketSocketPath), 0755); err != nil {
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

	waitForFile(t, ticketSocketPath)

	// --- Phase 2: Daemon service discovery ---
	//
	// The ticket service published an m.bureau.service state event to
	// #bureau/service during startup. The daemon picks this up via /sync,
	// runs syncServiceDirectory → reconcileServices, configures proxy
	// routes for any running consumers, and posts to the config room.
	serviceWatch.WaitForMessage(t, "added service/ticket/integ", machine.UserID)

	// Verify unauthenticated connectivity. The "status" action requires
	// no authentication and proves the socket is reachable and CBOR is working.
	unauthClient := service.NewServiceClientFromToken(ticketSocketPath, nil)
	var status ticketStatusResult
	if err := unauthClient.Call(ctx, "status", nil, &status); err != nil {
		t.Fatalf("status call failed: %v", err)
	}
	if status.UptimeSeconds <= 0 {
		t.Errorf("uptime = %f, want > 0", status.UptimeSeconds)
	}

	// --- Phase 3: Deploy a consumer agent with required_services ---
	//
	// This exercises the daemon's resolveServiceMounts path: the template
	// declares required_services: ["ticket"], so the daemon reads
	// m.bureau.room_service in the consumer's workspace room to find the
	// ticket service principal, derives the socket path, and passes it
	// to the launcher as a ServiceMount. The launcher bind-mounts the
	// socket at /run/bureau/service/ticket.sock in the sandbox.
	//
	// The daemon also mints a service token with audience="ticket" and
	// the consumer's ticket-scoped grants, writing it to
	// <stateDir>/tokens/<consumer>/ticket.

	// Publish a template with RequiredServices: ["ticket"].
	templateRoomAlias := schema.FullRoomAlias(schema.RoomAliasTemplate, testServerName)
	templateRoomID, err := admin.ResolveAlias(ctx, templateRoomAlias)
	if err != nil {
		t.Fatalf("resolve template room: %v", err)
	}

	if err := admin.InviteUser(ctx, templateRoomID, machine.UserID); err != nil {
		if !messaging.IsMatrixError(err, "M_FORBIDDEN") {
			t.Fatalf("invite machine to template room: %v", err)
		}
	}

	_, err = admin.SendStateEvent(ctx, templateRoomID,
		schema.EventTypeTemplate, "ticket-consumer", schema.TemplateContent{
			Description:      "Consumer agent requiring ticket service",
			Command:          []string{testAgentBinary},
			RequiredServices: []string{"ticket"},
			Namespaces: &schema.TemplateNamespaces{
				PID: true,
			},
			Security: &schema.TemplateSecurity{
				NewSession:    true,
				DieWithParent: true,
				NoNewPrivs:    true,
			},
			Filesystem: []schema.TemplateMount{
				{Source: testAgentBinary, Dest: testAgentBinary, Mode: "ro"},
				{Dest: "/tmp", Type: "tmpfs"},
			},
			CreateDirs: []string{"/tmp", "/var/tmp", "/run/bureau"},
			EnvironmentVariables: map[string]string{
				"HOME":                "/workspace",
				"TERM":                "xterm-256color",
				"BUREAU_PROXY_SOCKET": "${PROXY_SOCKET}",
				"BUREAU_MACHINE_NAME": "${MACHINE_NAME}",
				"BUREAU_SERVER_NAME":  "${SERVER_NAME}",
			},
		})
	if err != nil {
		t.Fatalf("publish ticket-consumer template: %v", err)
	}

	// Register the consumer agent and push credentials.
	consumerLocalpart := "agent/ticket-consumer"
	consumer := registerPrincipal(t, consumerLocalpart, "consumer-password")
	pushCredentials(t, admin, machine, consumer)

	// Deploy the consumer with a MachineConfig that references the
	// template. WORKSPACE_ROOM_ID tells the daemon to resolve
	// m.bureau.room_service from the project room (where the "ticket"
	// binding lives). The authorization policy grants ticket/** actions
	// so the daemon includes them in the minted service token.
	templateRef := "bureau/template:ticket-consumer"
	_, err = admin.SendStateEvent(ctx, machine.ConfigRoomID,
		schema.EventTypeMachineConfig, machine.Name, schema.MachineConfig{
			Principals: []schema.PrincipalAssignment{
				{
					Localpart: consumer.Localpart,
					Template:  templateRef,
					AutoStart: true,
					Payload: map[string]any{
						"WORKSPACE_ROOM_ID": projectRoomID,
					},
					Authorization: &schema.AuthorizationPolicy{
						Grants: []schema.Grant{
							{Actions: []string{"ticket/**"}},
						},
					},
				},
			},
		})
	if err != nil {
		t.Fatalf("push machine config: %v", err)
	}

	// Wait for the consumer's proxy socket. This proves the daemon
	// successfully resolved the template, resolved the ticket service
	// mount from the project room's m.bureau.room_service binding,
	// minted a service token, and the launcher created the sandbox.
	consumerSocketPath := machine.PrincipalSocketPath(consumer.Localpart)
	waitForFile(t, consumerSocketPath)
	t.Logf("consumer sandbox created: proxy socket at %s", consumerSocketPath)

	// --- Phase 4: Authenticated ticket creation using daemon-minted token ---
	//
	// Read the service token that the daemon minted for the consumer.
	// In production, the agent reads this from /run/bureau/tokens/ticket
	// inside the sandbox (bind-mounted from the host-side token directory).
	// Here we read it from the host side for verification.
	tokenPath := filepath.Join(machine.StateDir, "tokens", consumerLocalpart, "ticket")
	tokenBytes, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("read daemon-minted service token: %v", err)
	}
	t.Logf("daemon-minted service token: %d bytes at %s", len(tokenBytes), tokenPath)

	// Create a ticket using the daemon-minted token.
	authedClient := service.NewServiceClientFromToken(ticketSocketPath, tokenBytes)

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

	// --- Phase 5: Verify Matrix state event ---
	//
	// The ticket service wrote the ticket as an m.bureau.ticket state
	// event in the project room. Read it back via the Matrix client-server
	// API to verify the full round-trip: daemon-minted token → socket
	// request → ticket service → Matrix state event.
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
