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
	fleetRoomID := defaultFleetRoomID(t)

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
		FleetRoomID:    fleetRoomID,
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

// ticketMutationResult mirrors the ticket service's mutation response
// (update, close, reopen) for CBOR decoding in tests.
type ticketMutationResult struct {
	ID      string               `json:"id"`
	Room    string               `json:"room"`
	Content schema.TicketContent `json:"content"`
}

// ticketEntryResult mirrors entryWithRoom for list/ready/blocked results.
type ticketEntryResult struct {
	ID      string               `json:"id"`
	Room    string               `json:"room,omitempty"`
	Content schema.TicketContent `json:"content"`
}

// TestTicketLifecycle exercises the ticket service's core operational
// scenarios against a real homeserver and daemon: cross-room filing,
// full lifecycle transitions, dependency-driven readiness, asymmetric
// permissions (PM can close, worker cannot), and claim contention.
//
// This test validates that the daemon correctly propagates
// AuthorizationPolicy grants into service tokens, and that the ticket
// service enforces fine-grained authorization (ticket/close,
// ticket/reopen) separate from ticket/update.
func TestTicketLifecycle(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()

	ctx := t.Context()

	testAgentBinary := resolvedBinary(t, "TEST_AGENT_BINARY")
	fleetRoomID := defaultFleetRoomID(t)

	// --- Phase 0: Boot a machine ---

	machine := newTestMachine(t, "machine/ticket-lifecycle")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		FleetRoomID:    fleetRoomID,
	})

	// --- Phase 1: Ticket service setup ---

	ticketServiceLocalpart := "service/ticket/lifecycle"
	ticketServiceAccount := registerPrincipal(t, ticketServiceLocalpart, "ticket-lifecycle-pw")

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

	// Invite ticket service to global rooms.
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

	// --- Phase 2: Create two project rooms ---
	//
	// Both rooms bind to the same ticket service. This tests that
	// one service instance can manage tickets across multiple rooms,
	// and that cross-room ticket filing works.

	adminUserID := "@bureau-admin:" + testServerName

	createProjectRoom := func(name string) string {
		room, err := admin.CreateRoom(ctx, messaging.CreateRoomRequest{
			Name:   name,
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
			t.Fatalf("create room %s: %v", name, err)
		}

		_, err = admin.SendStateEvent(ctx, room.RoomID, schema.EventTypeTicketConfig, "",
			schema.TicketConfigContent{
				Version: schema.TicketConfigVersion,
				Prefix:  "tkt",
			})
		if err != nil {
			t.Fatalf("publish ticket config for %s: %v", name, err)
		}

		_, err = admin.SendStateEvent(ctx, room.RoomID, schema.EventTypeRoomService, "ticket",
			schema.RoomServiceContent{
				Principal: ticketServiceAccount.UserID,
			})
		if err != nil {
			t.Fatalf("publish room service binding for %s: %v", name, err)
		}

		return room.RoomID
	}

	roomAlphaID := createProjectRoom("lifecycle-alpha")
	roomBetaID := createProjectRoom("lifecycle-beta")
	t.Logf("project rooms: alpha=%s beta=%s", roomAlphaID, roomBetaID)

	// --- Phase 3: Start ticket service ---

	serviceWatch := watchRoom(t, admin, machine.ConfigRoomID)

	ticketSocketPath := principal.RunDirSocketPath(machine.RunDir, ticketServiceLocalpart)
	if err := os.MkdirAll(filepath.Dir(ticketSocketPath), 0755); err != nil {
		t.Fatalf("create socket parent directory: %v", err)
	}

	ticketBinary := resolvedBinary(t, "TICKET_SERVICE_BINARY")
	startProcess(t, "ticket-lifecycle", ticketBinary,
		"--homeserver", testHomeserverURL,
		"--machine-name", machine.Name,
		"--principal-name", ticketServiceLocalpart,
		"--server-name", testServerName,
		"--run-dir", machine.RunDir,
		"--state-dir", ticketStateDir,
	)

	waitForFile(t, ticketSocketPath)
	serviceWatch.WaitForMessage(t, "added service/ticket/lifecycle", machine.UserID)

	// --- Phase 4: Deploy PM and worker consumers ---
	//
	// The PM gets ticket/** (all ticket operations including close/reopen).
	// The worker gets specific grants excluding ticket/close and ticket/reopen.

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
		schema.EventTypeTemplate, "ticket-lifecycle-agent", schema.TemplateContent{
			Description:      "Agent for ticket lifecycle testing",
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
		t.Fatalf("publish ticket-lifecycle-agent template: %v", err)
	}

	// Register PM and worker accounts.
	pmLocalpart := "agent/lifecycle-pm"
	pmAccount := registerPrincipal(t, pmLocalpart, "pm-password")
	pushCredentials(t, admin, machine, pmAccount)

	workerLocalpart := "agent/lifecycle-worker"
	workerAccount := registerPrincipal(t, workerLocalpart, "worker-password")
	pushCredentials(t, admin, machine, workerAccount)

	templateRef := "bureau/template:ticket-lifecycle-agent"
	_, err = admin.SendStateEvent(ctx, machine.ConfigRoomID,
		schema.EventTypeMachineConfig, machine.Name, schema.MachineConfig{
			Principals: []schema.PrincipalAssignment{
				{
					Localpart: pmAccount.Localpart,
					Template:  templateRef,
					AutoStart: true,
					Payload: map[string]any{
						"WORKSPACE_ROOM_ID": roomAlphaID,
					},
					Authorization: &schema.AuthorizationPolicy{
						Grants: []schema.Grant{
							{Actions: []string{"ticket/**"}},
						},
					},
				},
				{
					Localpart: workerAccount.Localpart,
					Template:  templateRef,
					AutoStart: true,
					Payload: map[string]any{
						"WORKSPACE_ROOM_ID": roomAlphaID,
					},
					Authorization: &schema.AuthorizationPolicy{
						Grants: []schema.Grant{
							{Actions: []string{
								"ticket/create",
								"ticket/update",
								"ticket/list",
								"ticket/ready",
								"ticket/show",
								"ticket/blocked",
								"ticket/stats",
								"ticket/grep",
								"ticket/deps",
								"ticket/ranked",
								"ticket/children",
								"ticket/epic-health",
								"ticket/info",
							}},
						},
					},
				},
			},
		})
	if err != nil {
		t.Fatalf("push machine config: %v", err)
	}

	// Wait for both consumer proxy sockets.
	pmSocketPath := machine.PrincipalSocketPath(pmAccount.Localpart)
	workerSocketPath := machine.PrincipalSocketPath(workerAccount.Localpart)
	waitForFile(t, pmSocketPath)
	waitForFile(t, workerSocketPath)
	t.Log("both consumers deployed")

	// Read daemon-minted tokens.
	pmTokenPath := filepath.Join(machine.StateDir, "tokens", pmLocalpart, "ticket")
	pmTokenBytes, err := os.ReadFile(pmTokenPath)
	if err != nil {
		t.Fatalf("read PM service token: %v", err)
	}

	workerTokenPath := filepath.Join(machine.StateDir, "tokens", workerLocalpart, "ticket")
	workerTokenBytes, err := os.ReadFile(workerTokenPath)
	if err != nil {
		t.Fatalf("read worker service token: %v", err)
	}

	pmClient := service.NewServiceClientFromToken(ticketSocketPath, pmTokenBytes)
	workerClient := service.NewServiceClientFromToken(ticketSocketPath, workerTokenBytes)

	// --- Phase 5: Cross-room filing ---

	t.Run("CrossRoomFiling", func(t *testing.T) {
		var alphaResult ticketCreateResult
		err := pmClient.Call(ctx, "create", map[string]any{
			"room":     roomAlphaID,
			"title":    "Alpha ticket",
			"type":     "task",
			"priority": 2,
		}, &alphaResult)
		if err != nil {
			t.Fatalf("create in alpha: %v", err)
		}
		if alphaResult.Room != roomAlphaID {
			t.Errorf("alpha room = %q, want %q", alphaResult.Room, roomAlphaID)
		}

		var betaResult ticketCreateResult
		err = pmClient.Call(ctx, "create", map[string]any{
			"room":     roomBetaID,
			"title":    "Beta ticket",
			"type":     "feature",
			"priority": 1,
		}, &betaResult)
		if err != nil {
			t.Fatalf("create in beta: %v", err)
		}
		if betaResult.Room != roomBetaID {
			t.Errorf("beta room = %q, want %q", betaResult.Room, roomBetaID)
		}

		// Verify both tickets exist in Matrix via state events.
		raw, err := admin.GetStateEvent(ctx, roomAlphaID, schema.EventTypeTicket, alphaResult.ID)
		if err != nil {
			t.Fatalf("get alpha ticket state: %v", err)
		}
		var alphaContent schema.TicketContent
		if err := json.Unmarshal(raw, &alphaContent); err != nil {
			t.Fatalf("unmarshal alpha ticket: %v", err)
		}
		if alphaContent.Title != "Alpha ticket" {
			t.Errorf("alpha title = %q, want %q", alphaContent.Title, "Alpha ticket")
		}

		raw, err = admin.GetStateEvent(ctx, roomBetaID, schema.EventTypeTicket, betaResult.ID)
		if err != nil {
			t.Fatalf("get beta ticket state: %v", err)
		}
		var betaContent schema.TicketContent
		if err := json.Unmarshal(raw, &betaContent); err != nil {
			t.Fatalf("unmarshal beta ticket: %v", err)
		}
		if betaContent.Title != "Beta ticket" {
			t.Errorf("beta title = %q, want %q", betaContent.Title, "Beta ticket")
		}

		t.Logf("cross-room filing verified: alpha=%s beta=%s", alphaResult.ID, betaResult.ID)
	})

	// --- Phase 6: Full lifecycle ---

	t.Run("FullLifecycle", func(t *testing.T) {
		// Create.
		var createResult ticketCreateResult
		err := pmClient.Call(ctx, "create", map[string]any{
			"room":     roomAlphaID,
			"title":    "Lifecycle ticket",
			"type":     "task",
			"priority": 2,
		}, &createResult)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		ticketID := createResult.ID
		t.Logf("created %s", ticketID)

		// Claim (open → in_progress).
		pmUserID := pmAccount.UserID
		var claimResult ticketMutationResult
		err = pmClient.Call(ctx, "update", map[string]any{
			"room":     roomAlphaID,
			"ticket":   ticketID,
			"status":   "in_progress",
			"assignee": pmUserID,
		}, &claimResult)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if claimResult.Content.Status != "in_progress" {
			t.Errorf("after claim: status = %q, want in_progress", claimResult.Content.Status)
		}
		if claimResult.Content.Assignee != pmUserID {
			t.Errorf("after claim: assignee = %q, want %q", claimResult.Content.Assignee, pmUserID)
		}

		// Close (in_progress → closed).
		var closeResult ticketMutationResult
		err = pmClient.Call(ctx, "close", map[string]any{
			"room":   roomAlphaID,
			"ticket": ticketID,
			"reason": "completed",
		}, &closeResult)
		if err != nil {
			t.Fatalf("close: %v", err)
		}
		if closeResult.Content.Status != "closed" {
			t.Errorf("after close: status = %q, want closed", closeResult.Content.Status)
		}
		if closeResult.Content.ClosedAt == "" {
			t.Error("after close: closed_at is empty")
		}
		if closeResult.Content.CloseReason != "completed" {
			t.Errorf("after close: reason = %q, want completed", closeResult.Content.CloseReason)
		}
		if closeResult.Content.Assignee != "" {
			t.Errorf("after close: assignee should be cleared, got %q", closeResult.Content.Assignee)
		}

		// Verify closed state in Matrix.
		raw, err := admin.GetStateEvent(ctx, roomAlphaID, schema.EventTypeTicket, ticketID)
		if err != nil {
			t.Fatalf("get closed ticket state: %v", err)
		}
		var closedContent schema.TicketContent
		if err := json.Unmarshal(raw, &closedContent); err != nil {
			t.Fatalf("unmarshal closed ticket: %v", err)
		}
		if closedContent.Status != "closed" {
			t.Errorf("Matrix state: status = %q, want closed", closedContent.Status)
		}

		// Reopen (closed → open).
		var reopenResult ticketMutationResult
		err = pmClient.Call(ctx, "reopen", map[string]any{
			"room":   roomAlphaID,
			"ticket": ticketID,
		}, &reopenResult)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		if reopenResult.Content.Status != "open" {
			t.Errorf("after reopen: status = %q, want open", reopenResult.Content.Status)
		}
		if reopenResult.Content.ClosedAt != "" {
			t.Errorf("after reopen: closed_at should be cleared, got %q", reopenResult.Content.ClosedAt)
		}
		if reopenResult.Content.CloseReason != "" {
			t.Errorf("after reopen: close_reason should be cleared, got %q", reopenResult.Content.CloseReason)
		}

		t.Logf("full lifecycle verified for %s", ticketID)
	})

	// --- Phase 7: Dependencies and readiness ---

	t.Run("DependenciesAndReadiness", func(t *testing.T) {
		// Create ticket A (no blockers).
		var resultA ticketCreateResult
		err := pmClient.Call(ctx, "create", map[string]any{
			"room":     roomAlphaID,
			"title":    "Dependency parent",
			"type":     "task",
			"priority": 1,
		}, &resultA)
		if err != nil {
			t.Fatalf("create A: %v", err)
		}
		ticketA := resultA.ID

		// Create ticket B (blocked by A).
		var resultB ticketCreateResult
		err = pmClient.Call(ctx, "create", map[string]any{
			"room":       roomAlphaID,
			"title":      "Blocked child",
			"type":       "task",
			"priority":   2,
			"blocked_by": []string{ticketA},
		}, &resultB)
		if err != nil {
			t.Fatalf("create B: %v", err)
		}
		ticketB := resultB.ID

		t.Logf("dependency pair: A=%s B=%s (B blocked by A)", ticketA, ticketB)

		// Query blocked — B should be blocked.
		var blockedEntries []ticketEntryResult
		err = pmClient.Call(ctx, "blocked", map[string]any{
			"room": roomAlphaID,
		}, &blockedEntries)
		if err != nil {
			t.Fatalf("blocked query: %v", err)
		}

		foundBlocked := false
		for _, entry := range blockedEntries {
			if entry.ID == ticketB {
				foundBlocked = true
				break
			}
		}
		if !foundBlocked {
			t.Errorf("ticket B (%s) not in blocked set", ticketB)
		}

		// Query ready — A should be ready, B should not.
		var readyEntries []ticketEntryResult
		err = pmClient.Call(ctx, "ready", map[string]any{
			"room": roomAlphaID,
		}, &readyEntries)
		if err != nil {
			t.Fatalf("ready query (before close): %v", err)
		}

		foundAReady := false
		foundBReady := false
		for _, entry := range readyEntries {
			if entry.ID == ticketA {
				foundAReady = true
			}
			if entry.ID == ticketB {
				foundBReady = true
			}
		}
		if !foundAReady {
			t.Errorf("ticket A (%s) should be ready (no blockers)", ticketA)
		}
		if foundBReady {
			t.Errorf("ticket B (%s) should NOT be ready (blocked by A)", ticketB)
		}

		// Close ticket A.
		err = pmClient.Call(ctx, "close", map[string]any{
			"room":   roomAlphaID,
			"ticket": ticketA,
			"reason": "done",
		}, nil)
		if err != nil {
			t.Fatalf("close A: %v", err)
		}

		// Query ready again — B should now be ready.
		var readyAfterClose []ticketEntryResult
		err = pmClient.Call(ctx, "ready", map[string]any{
			"room": roomAlphaID,
		}, &readyAfterClose)
		if err != nil {
			t.Fatalf("ready query (after close): %v", err)
		}

		foundBReadyAfter := false
		for _, entry := range readyAfterClose {
			if entry.ID == ticketB {
				foundBReadyAfter = true
				break
			}
		}
		if !foundBReadyAfter {
			t.Errorf("ticket B (%s) should be ready after A was closed", ticketB)
		}

		// Query blocked again — B should not be blocked anymore.
		var blockedAfterClose []ticketEntryResult
		err = pmClient.Call(ctx, "blocked", map[string]any{
			"room": roomAlphaID,
		}, &blockedAfterClose)
		if err != nil {
			t.Fatalf("blocked query (after close): %v", err)
		}

		for _, entry := range blockedAfterClose {
			if entry.ID == ticketB {
				t.Errorf("ticket B (%s) should not be blocked after A was closed", ticketB)
			}
		}

		t.Logf("dependency readiness verified: A=%s closed, B=%s now ready", ticketA, ticketB)
	})

	// --- Phase 8: Asymmetric permissions ---

	t.Run("AsymmetricPermissions", func(t *testing.T) {
		// Worker creates a ticket — should succeed.
		var createResult ticketCreateResult
		err := workerClient.Call(ctx, "create", map[string]any{
			"room":     roomAlphaID,
			"title":    "Worker-created ticket",
			"type":     "task",
			"priority": 2,
		}, &createResult)
		if err != nil {
			t.Fatalf("worker create: %v", err)
		}
		ticketID := createResult.ID
		t.Logf("worker created %s", ticketID)

		// Worker claims it — should succeed.
		workerUserID := workerAccount.UserID
		var claimResult ticketMutationResult
		err = workerClient.Call(ctx, "update", map[string]any{
			"room":     roomAlphaID,
			"ticket":   ticketID,
			"status":   "in_progress",
			"assignee": workerUserID,
		}, &claimResult)
		if err != nil {
			t.Fatalf("worker claim: %v", err)
		}
		if claimResult.Content.Status != "in_progress" {
			t.Errorf("worker claim: status = %q, want in_progress", claimResult.Content.Status)
		}

		// Worker tries to close — should FAIL (no ticket/close grant).
		err = workerClient.Call(ctx, "close", map[string]any{
			"room":   roomAlphaID,
			"ticket": ticketID,
			"reason": "done",
		}, nil)
		if err == nil {
			t.Fatal("worker close should have failed (no ticket/close grant)")
		}
		t.Logf("worker close correctly denied: %v", err)

		// Worker tries to close via update action — should also FAIL.
		err = workerClient.Call(ctx, "update", map[string]any{
			"room":   roomAlphaID,
			"ticket": ticketID,
			"status": "closed",
		}, nil)
		if err == nil {
			t.Fatal("worker update-to-closed should have failed (no ticket/close grant)")
		}
		t.Logf("worker update-to-closed correctly denied: %v", err)

		// PM closes the ticket — should succeed.
		var closeResult ticketMutationResult
		err = pmClient.Call(ctx, "close", map[string]any{
			"room":   roomAlphaID,
			"ticket": ticketID,
			"reason": "PM approved",
		}, &closeResult)
		if err != nil {
			t.Fatalf("PM close: %v", err)
		}
		if closeResult.Content.Status != "closed" {
			t.Errorf("PM close: status = %q, want closed", closeResult.Content.Status)
		}

		// Worker tries to reopen — should FAIL (no ticket/reopen grant).
		err = workerClient.Call(ctx, "reopen", map[string]any{
			"room":   roomAlphaID,
			"ticket": ticketID,
		}, nil)
		if err == nil {
			t.Fatal("worker reopen should have failed (no ticket/reopen grant)")
		}
		t.Logf("worker reopen correctly denied: %v", err)

		// PM reopens — should succeed.
		var reopenResult ticketMutationResult
		err = pmClient.Call(ctx, "reopen", map[string]any{
			"room":   roomAlphaID,
			"ticket": ticketID,
		}, &reopenResult)
		if err != nil {
			t.Fatalf("PM reopen: %v", err)
		}
		if reopenResult.Content.Status != "open" {
			t.Errorf("PM reopen: status = %q, want open", reopenResult.Content.Status)
		}

		t.Logf("asymmetric permissions verified for %s", ticketID)
	})

	// --- Phase 9: Contention detection ---

	t.Run("ContentionDetection", func(t *testing.T) {
		// PM creates a ticket.
		var createResult ticketCreateResult
		err := pmClient.Call(ctx, "create", map[string]any{
			"room":     roomAlphaID,
			"title":    "Contention target",
			"type":     "task",
			"priority": 1,
		}, &createResult)
		if err != nil {
			t.Fatalf("create contention ticket: %v", err)
		}
		ticketID := createResult.ID
		t.Logf("contention target: %s", ticketID)

		// Worker claims it first.
		workerUserID := workerAccount.UserID
		err = workerClient.Call(ctx, "update", map[string]any{
			"room":     roomAlphaID,
			"ticket":   ticketID,
			"status":   "in_progress",
			"assignee": workerUserID,
		}, nil)
		if err != nil {
			t.Fatalf("worker claim: %v", err)
		}

		// PM tries to claim the same ticket — should fail with contention.
		pmUserID := pmAccount.UserID
		err = pmClient.Call(ctx, "update", map[string]any{
			"room":     roomAlphaID,
			"ticket":   ticketID,
			"status":   "in_progress",
			"assignee": pmUserID,
		}, nil)
		if err == nil {
			t.Fatal("PM claim should have failed (ticket already in_progress)")
		}
		t.Logf("contention correctly detected: %v", err)
	})
}
