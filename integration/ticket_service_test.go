// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/template"
	"github.com/bureau-foundation/bureau/lib/testutil"
	"github.com/bureau-foundation/bureau/messaging"
)

// TestTicketServiceAgent exercises the full production path for an agent
// creating a ticket: daemon service discovery, sandbox creation with
// service mount, mock LLM directing bureau_ticket_create via MCP,
// daemon-minted token authentication, and ticket creation verified via
// Matrix state events.
//
// The agent runs inside the sandbox and calls the ticket service through
// the bind-mounted socket using the daemon-minted token — the same path
// production agents use. The test observes outcomes exclusively via Matrix
// state events, which is the same observation path production systems use.
func TestTicketServiceAgent(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	admin := adminSession(t)
	defer admin.Close()

	fleet := createTestFleet(t, admin)

	// Boot a machine.
	machine := newTestMachine(t, fleet, "ticket-agent")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	})

	// --- Ticket service setup ---

	ticketServiceLocalpart := "service/ticket/agent-test"
	ticketServiceAccount := registerFleetPrincipal(t, fleet, ticketServiceLocalpart, "ticket-svc-password")

	ticketStateDir := t.TempDir()
	writeServiceSession(t, ticketStateDir, ticketServiceAccount)

	systemRoomID := resolveSystemRoom(t, admin)
	inviteToRooms(t, admin, ticketServiceAccount.UserID, systemRoomID, fleet.ServiceRoomID)

	// Create a project room with ticket config and service binding.
	ticketServiceEntity, err := ref.NewEntityFromAccountLocalpart(fleet.Ref, ticketServiceLocalpart)
	if err != nil {
		t.Fatalf("construct ticket service entity ref: %v", err)
	}
	projectRoomID := createTicketProjectRoom(t, admin, "ticket-agent-project",
		ticketServiceEntity, machine.UserID.String())

	// Start ticket service and wait for daemon discovery.
	serviceWatch := watchRoom(t, admin, machine.ConfigRoomID)

	ticketSocketPath := machine.PrincipalSocketPath(t, ticketServiceLocalpart)
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
		"--fleet", fleet.Prefix,
	)
	waitForFile(t, ticketSocketPath)

	// The service binary registers with a fleet-scoped localpart, so the
	// daemon's directory update message uses the fleet-scoped name.
	fleetScopedServiceName := ticketServiceEntity.Localpart()
	waitForNotification[schema.ServiceDirectoryUpdatedMessage](
		t, &serviceWatch, schema.MsgTypeServiceDirectoryUpdated, machine.UserID,
		func(message schema.ServiceDirectoryUpdatedMessage) bool {
			return slices.Contains(message.Added, fleetScopedServiceName)
		}, "service directory update adding "+fleetScopedServiceName)

	// --- Deploy agent with bureau-agent + mock LLM ---

	agent := deployAgent(t, admin, machine, agentOptions{
		Binary:           testutil.DataBinary(t, "BUREAU_AGENT_BINARY"),
		Localpart:        "agent/ticket-e2e",
		RequiredServices: []string{"ticket"},
		ExtraEnv: map[string]string{
			"BUREAU_AGENT_MODEL":      "mock-model",
			"BUREAU_AGENT_SERVICE":    "anthropic",
			"BUREAU_AGENT_MAX_TOKENS": "1024",
		},
		Payload: map[string]any{"WORKSPACE_ROOM_ID": projectRoomID},
		Authorization: &schema.AuthorizationPolicy{
			Grants: []schema.Grant{
				{Actions: []string{"command/ticket/**", "ticket/**"}},
			},
		},
	})

	// Register mock Anthropic that directs the agent to create a ticket.
	mock := newMockToolSequence(t, []mockToolStep{{
		ToolName: "bureau_ticket_create",
		ToolInput: func() map[string]any {
			return map[string]any{
				"room":     projectRoomID,
				"title":    "Agent-created ticket",
				"type":     "task",
				"priority": 2,
			}
		},
	}})

	registerProxyHTTPService(t, agent.AdminSocketPath, "anthropic", mock.URL)

	// Send prompt to trigger the agent loop.
	projectWatch := watchRoom(t, admin, projectRoomID)
	if _, err := admin.SendMessage(ctx, machine.ConfigRoomID, messaging.NewTextMessage("Create a ticket")); err != nil {
		t.Fatalf("sending prompt to agent: %v", err)
	}

	waitForMockCompletion(t, mock)

	// --- Verification via Matrix state events ---
	//
	// The ticket service wrote m.bureau.ticket to the project room.
	// Read it the same way a production observer would.
	ticketID, ticketContent := waitForTicket(t, &projectWatch, "Agent-created ticket")

	if ticketContent.Status != "open" {
		t.Errorf("status = %q, want open", ticketContent.Status)
	}
	if ticketContent.Type != "task" {
		t.Errorf("type = %q, want task", ticketContent.Type)
	}
	if ticketContent.Priority != 2 {
		t.Errorf("priority = %d, want 2", ticketContent.Priority)
	}
	if ticketContent.CreatedBy.IsZero() {
		t.Error("created_by is empty")
	}

	t.Logf("ticket %s verified: title=%q status=%s type=%s",
		ticketID, ticketContent.Title, ticketContent.Status, ticketContent.Type)
}

// TestTicketLifecycleAgent exercises ticket lifecycle operations through
// agents running inside a sandbox with mock LLMs. A single machine
// hosts one agent at a time — the test switches between PM and worker
// roles by pushing new machine configs (different principal accounts
// and authorization grants). This sequential deployment avoids two
// problems: agents sharing a config room would see each other's
// messages and trigger unwanted LLM calls, and service tokens are
// cryptographically bound to one machine's Ed25519 key (so a ticket
// service can't verify tokens from a different machine's daemon).
//
// The ticket service persists across redeployments (it's a separate
// process), so its in-memory state and Matrix-synced ticket index
// remain consistent throughout all subtests.
//
// Each operation is a separate mock→agent→service→Matrix cycle. Between
// operations, the test reads state events from Matrix to verify outcomes
// and extract dynamic values (ticket IDs) for subsequent operations.
func TestTicketLifecycleAgent(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	admin := adminSession(t)
	defer admin.Close()

	fleet := createTestFleet(t, admin)

	// Boot a machine.
	machine := newTestMachine(t, fleet, "ticket-lifecycle")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	})

	// --- Ticket service setup ---

	ticketServiceLocalpart := "service/ticket/lifecycle"
	ticketServiceAccount := registerFleetPrincipal(t, fleet, ticketServiceLocalpart, "ticket-lifecycle-pw")

	ticketStateDir := t.TempDir()
	writeServiceSession(t, ticketStateDir, ticketServiceAccount)

	systemRoomID := resolveSystemRoom(t, admin)
	inviteToRooms(t, admin, ticketServiceAccount.UserID, systemRoomID, fleet.ServiceRoomID)

	// Two project rooms for cross-room filing.
	ticketServiceEntity, err := ref.NewEntityFromAccountLocalpart(fleet.Ref, ticketServiceLocalpart)
	if err != nil {
		t.Fatalf("construct ticket service entity ref: %v", err)
	}
	roomAlphaID := createTicketProjectRoom(t, admin, "lifecycle-alpha",
		ticketServiceEntity, machine.UserID.String())
	roomBetaID := createTicketProjectRoom(t, admin, "lifecycle-beta",
		ticketServiceEntity, machine.UserID.String())

	// Start ticket service and wait for daemon discovery.
	serviceWatch := watchRoom(t, admin, machine.ConfigRoomID)

	ticketSocketPath := machine.PrincipalSocketPath(t, ticketServiceLocalpart)
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
		"--fleet", fleet.Prefix,
	)
	waitForFile(t, ticketSocketPath)

	fleetScopedServiceName := ticketServiceEntity.Localpart()
	waitForNotification[schema.ServiceDirectoryUpdatedMessage](
		t, &serviceWatch, schema.MsgTypeServiceDirectoryUpdated, machine.UserID,
		func(message schema.ServiceDirectoryUpdatedMessage) bool {
			return slices.Contains(message.Added, fleetScopedServiceName)
		}, "service directory update adding "+fleetScopedServiceName)

	// --- Agent accounts and template ---
	//
	// Both PM and worker accounts are registered and joined to the config
	// room up front. Credentials are pushed for both. The test switches
	// between them by pushing different machine configs — the daemon
	// tears down the old sandbox and starts a new one with the new
	// principal, grants, and freshly-minted service token.

	agentBinary := testutil.DataBinary(t, "BUREAU_AGENT_BINARY")
	grantTemplateAccess(t, admin, machine)

	agentTemplateRef, err := schema.ParseTemplateRef("bureau/template:ticket-lifecycle-agent")
	if err != nil {
		t.Fatalf("parse template ref: %v", err)
	}
	_, err = template.Push(ctx, admin, agentTemplateRef, agentTemplateContent(agentBinary, agentOptions{
		TemplateName:     "ticket-lifecycle-agent",
		RequiredServices: []string{"ticket"},
		ExtraEnv: map[string]string{
			"BUREAU_AGENT_MODEL":      "mock-model",
			"BUREAU_AGENT_SERVICE":    "anthropic",
			"BUREAU_AGENT_MAX_TOKENS": "1024",
		},
	}), testServer)
	if err != nil {
		t.Fatalf("push ticket-lifecycle-agent template: %v", err)
	}

	pmAccount := registerFleetPrincipal(t, fleet, "agent/lifecycle-pm", "pm-password")
	pushCredentials(t, admin, machine, pmAccount)
	joinConfigRoom(t, admin, machine.ConfigRoomID, pmAccount)

	workerAccount := registerFleetPrincipal(t, fleet, "agent/lifecycle-worker", "worker-password")
	pushCredentials(t, admin, machine, workerAccount)
	joinConfigRoom(t, admin, machine.ConfigRoomID, workerAccount)

	pmGrants := []string{"command/ticket/**", "ticket/**"}
	workerGrants := []string{
		"command/ticket/**",
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
	}

	templateRef := "bureau/template:ticket-lifecycle-agent"

	// deployAgent pushes a machine config with the given principal and
	// grants, waits for the agent to be ready, and returns its admin
	// socket path. If the same agent is already deployed (same
	// localpart), it returns the existing admin socket without
	// redeploying — the daemon deduplicates identical configs and
	// won't restart an already-running agent.
	var currentDeployedLocalpart string
	var currentAdminSocket string

	deployAgent := func(t *testing.T, account principalAccount, grants []string) string {
		t.Helper()

		if account.Localpart == currentDeployedLocalpart {
			return currentAdminSocket
		}

		readyWatch := watchRoom(t, admin, machine.ConfigRoomID)

		pushMachineConfig(t, admin, machine, deploymentConfig{
			Principals: []principalSpec{{
				Account:  account,
				Template: templateRef,
				Payload: map[string]any{
					"WORKSPACE_ROOM_ID": roomAlphaID,
				},
				Authorization: &schema.AuthorizationPolicy{
					Grants: []schema.Grant{
						{Actions: grants},
					},
				},
			}},
		})

		proxySocket := machine.PrincipalSocketPath(t, account.Localpart)
		waitForFile(t, proxySocket)
		readyWatch.WaitForMessage(t, "agent-ready", account.UserID)

		currentDeployedLocalpart = account.Localpart
		currentAdminSocket = machine.PrincipalAdminSocketPath(t, account.Localpart)
		return currentAdminSocket
	}

	// sendStep registers a mock on the admin socket, sends a prompt to
	// the config room, and waits for mock completion.
	sendStep := func(t *testing.T, adminSocket string, steps []mockToolStep, prompt string) {
		t.Helper()
		mock := newMockToolSequence(t, steps)
		registerProxyHTTPService(t, adminSocket, "anthropic", mock.URL)
		if _, err := admin.SendMessage(ctx, machine.ConfigRoomID, messaging.NewTextMessage(prompt)); err != nil {
			t.Fatalf("send prompt: %v", err)
		}
		waitForMockCompletion(t, mock)
	}

	// --- Subtests ---
	//
	// Each subtest deploys the agent(s) it needs. Between subtests the
	// previous agent may still be running; the next deployAgent call
	// replaces it.

	t.Run("CrossRoomFiling", func(t *testing.T) {
		alphaWatch := watchRoom(t, admin, roomAlphaID)
		betaWatch := watchRoom(t, admin, roomBetaID)

		adminSocket := deployAgent(t, pmAccount, pmGrants)

		sendStep(t, adminSocket, []mockToolStep{{
			ToolName: "bureau_ticket_create",
			ToolInput: func() map[string]any {
				return map[string]any{
					"room":     roomAlphaID,
					"title":    "Alpha ticket",
					"type":     "task",
					"priority": 2,
				}
			},
		}}, "Create alpha ticket")

		alphaTicketID, alphaContent := waitForTicket(t, &alphaWatch, "Alpha ticket")
		if alphaContent.Type != "task" {
			t.Errorf("alpha type = %q, want task", alphaContent.Type)
		}
		t.Logf("alpha ticket %s created", alphaTicketID)

		sendStep(t, adminSocket, []mockToolStep{{
			ToolName: "bureau_ticket_create",
			ToolInput: func() map[string]any {
				return map[string]any{
					"room":     roomBetaID,
					"title":    "Beta ticket",
					"type":     "feature",
					"priority": 1,
				}
			},
		}}, "Create beta ticket")

		betaTicketID, betaContent := waitForTicket(t, &betaWatch, "Beta ticket")
		if betaContent.Type != "feature" {
			t.Errorf("beta type = %q, want feature", betaContent.Type)
		}
		t.Logf("beta ticket %s created — cross-room filing verified", betaTicketID)
	})

	t.Run("FullLifecycle", func(t *testing.T) {
		projectWatch := watchRoom(t, admin, roomAlphaID)

		adminSocket := deployAgent(t, pmAccount, pmGrants)

		// Create.
		sendStep(t, adminSocket, []mockToolStep{{
			ToolName: "bureau_ticket_create",
			ToolInput: func() map[string]any {
				return map[string]any{
					"room":     roomAlphaID,
					"title":    "Lifecycle ticket",
					"type":     "task",
					"priority": 2,
				}
			},
		}}, "Create lifecycle ticket")

		ticketID, content := waitForTicket(t, &projectWatch, "Lifecycle ticket")
		if content.Status != "open" {
			t.Errorf("after create: status = %q, want open", content.Status)
		}
		t.Logf("created %s", ticketID)

		// Claim (open → in_progress).
		pmUserID := pmAccount.UserID
		sendStep(t, adminSocket, []mockToolStep{{
			ToolName: "bureau_ticket_update",
			ToolInput: func() map[string]any {
				return map[string]any{
					"room":     roomAlphaID,
					"ticket":   ticketID,
					"status":   "in_progress",
					"assignee": pmUserID,
				}
			},
		}}, "Claim ticket")

		content = readTicketState(t, admin, roomAlphaID, ticketID)
		if content.Status != "in_progress" {
			t.Errorf("after claim: status = %q, want in_progress", content.Status)
		}
		if content.Assignee != pmUserID {
			t.Errorf("after claim: assignee = %s, want %s", content.Assignee, pmUserID)
		}

		// Close (in_progress → closed).
		sendStep(t, adminSocket, []mockToolStep{{
			ToolName: "bureau_ticket_close",
			ToolInput: func() map[string]any {
				return map[string]any{
					"room":   roomAlphaID,
					"ticket": ticketID,
					"reason": "completed",
				}
			},
		}}, "Close ticket")

		content = readTicketState(t, admin, roomAlphaID, ticketID)
		if content.Status != "closed" {
			t.Errorf("after close: status = %q, want closed", content.Status)
		}
		if content.CloseReason != "completed" {
			t.Errorf("after close: reason = %q, want completed", content.CloseReason)
		}

		// Reopen (closed → open).
		sendStep(t, adminSocket, []mockToolStep{{
			ToolName: "bureau_ticket_reopen",
			ToolInput: func() map[string]any {
				return map[string]any{
					"room":   roomAlphaID,
					"ticket": ticketID,
				}
			},
		}}, "Reopen ticket")

		content = readTicketState(t, admin, roomAlphaID, ticketID)
		if content.Status != "open" {
			t.Errorf("after reopen: status = %q, want open", content.Status)
		}
		if content.ClosedAt != "" {
			t.Errorf("after reopen: closed_at should be cleared, got %q", content.ClosedAt)
		}

		t.Logf("full lifecycle verified for %s", ticketID)
	})

	t.Run("AsymmetricPermissions", func(t *testing.T) {
		projectWatch := watchRoom(t, admin, roomAlphaID)

		// Worker creates, claims, and tries to close a ticket.
		workerSocket := deployAgent(t, workerAccount, workerGrants)

		sendStep(t, workerSocket, []mockToolStep{{
			ToolName: "bureau_ticket_create",
			ToolInput: func() map[string]any {
				return map[string]any{
					"room":     roomAlphaID,
					"title":    "Permission test ticket",
					"type":     "task",
					"priority": 2,
				}
			},
		}}, "Worker: create ticket")

		ticketID, _ := waitForTicket(t, &projectWatch, "Permission test ticket")
		t.Logf("worker created %s", ticketID)

		workerUserID := workerAccount.UserID
		sendStep(t, workerSocket, []mockToolStep{{
			ToolName: "bureau_ticket_update",
			ToolInput: func() map[string]any {
				return map[string]any{
					"room":     roomAlphaID,
					"ticket":   ticketID,
					"status":   "in_progress",
					"assignee": workerUserID,
				}
			},
		}}, "Worker: claim ticket")

		content := readTicketState(t, admin, roomAlphaID, ticketID)
		if content.Status != "in_progress" {
			t.Errorf("after worker claim: status = %q, want in_progress", content.Status)
		}

		// Worker tries to close — service rejects (no ticket/close grant).
		sendStep(t, workerSocket, []mockToolStep{{
			ToolName: "bureau_ticket_close",
			ToolInput: func() map[string]any {
				return map[string]any{
					"room":   roomAlphaID,
					"ticket": ticketID,
					"reason": "done",
				}
			},
		}}, "Worker: close ticket")

		content = readTicketState(t, admin, roomAlphaID, ticketID)
		if content.Status != "in_progress" {
			t.Errorf("after worker close attempt: status = %q, want in_progress (close should be denied)", content.Status)
		}
		t.Log("worker close correctly denied by service")

		// Switch to PM — close and reopen succeed.
		pmSocket := deployAgent(t, pmAccount, pmGrants)

		sendStep(t, pmSocket, []mockToolStep{{
			ToolName: "bureau_ticket_close",
			ToolInput: func() map[string]any {
				return map[string]any{
					"room":   roomAlphaID,
					"ticket": ticketID,
					"reason": "PM approved",
				}
			},
		}}, "PM: close ticket")

		content = readTicketState(t, admin, roomAlphaID, ticketID)
		if content.Status != "closed" {
			t.Errorf("after PM close: status = %q, want closed", content.Status)
		}

		// Switch back to worker — reopen should fail.
		workerSocket = deployAgent(t, workerAccount, workerGrants)

		sendStep(t, workerSocket, []mockToolStep{{
			ToolName: "bureau_ticket_reopen",
			ToolInput: func() map[string]any {
				return map[string]any{
					"room":   roomAlphaID,
					"ticket": ticketID,
				}
			},
		}}, "Worker: reopen ticket")

		content = readTicketState(t, admin, roomAlphaID, ticketID)
		if content.Status != "closed" {
			t.Errorf("after worker reopen attempt: status = %q, want closed (reopen should be denied)", content.Status)
		}
		t.Log("worker reopen correctly denied by service")

		// Switch to PM — reopen succeeds.
		pmSocket = deployAgent(t, pmAccount, pmGrants)

		sendStep(t, pmSocket, []mockToolStep{{
			ToolName: "bureau_ticket_reopen",
			ToolInput: func() map[string]any {
				return map[string]any{
					"room":   roomAlphaID,
					"ticket": ticketID,
				}
			},
		}}, "PM: reopen ticket")

		content = readTicketState(t, admin, roomAlphaID, ticketID)
		if content.Status != "open" {
			t.Errorf("after PM reopen: status = %q, want open", content.Status)
		}

		t.Logf("asymmetric permissions verified for %s", ticketID)
	})

	t.Run("ContentionDetection", func(t *testing.T) {
		projectWatch := watchRoom(t, admin, roomAlphaID)

		// Worker creates and claims a ticket.
		workerSocket := deployAgent(t, workerAccount, workerGrants)

		sendStep(t, workerSocket, []mockToolStep{{
			ToolName: "bureau_ticket_create",
			ToolInput: func() map[string]any {
				return map[string]any{
					"room":     roomAlphaID,
					"title":    "Contention target",
					"type":     "task",
					"priority": 1,
				}
			},
		}}, "Worker: create contention ticket")

		ticketID, _ := waitForTicket(t, &projectWatch, "Contention target")

		workerUserID := workerAccount.UserID
		sendStep(t, workerSocket, []mockToolStep{{
			ToolName: "bureau_ticket_update",
			ToolInput: func() map[string]any {
				return map[string]any{
					"room":     roomAlphaID,
					"ticket":   ticketID,
					"status":   "in_progress",
					"assignee": workerUserID,
				}
			},
		}}, "Worker: claim ticket")

		content := readTicketState(t, admin, roomAlphaID, ticketID)
		if content.Assignee != workerUserID {
			t.Fatalf("after worker claim: assignee = %s, want %s", content.Assignee, workerUserID)
		}

		// Switch to PM — claim should fail (contention).
		pmSocket := deployAgent(t, pmAccount, pmGrants)

		pmUserID := pmAccount.UserID
		sendStep(t, pmSocket, []mockToolStep{{
			ToolName: "bureau_ticket_update",
			ToolInput: func() map[string]any {
				return map[string]any{
					"room":     roomAlphaID,
					"ticket":   ticketID,
					"status":   "in_progress",
					"assignee": pmUserID,
				}
			},
		}}, "PM: claim ticket")

		content = readTicketState(t, admin, roomAlphaID, ticketID)
		if content.Assignee != workerUserID {
			t.Errorf("after contention: assignee = %s, want %s (PM claim should be rejected)", content.Assignee, workerUserID)
		}

		t.Log("contention correctly detected")
	})
}

// --- Shared helpers for ticket tests ---

// writeServiceSession writes a session.json file for a Bureau service
// to the given state directory. The service loads its Matrix credentials
// from this file via service.LoadSession.
func writeServiceSession(t *testing.T, stateDir string, account principalAccount) {
	t.Helper()

	sessionData := service.SessionData{
		HomeserverURL: testHomeserverURL,
		UserID:        account.UserID.String(),
		AccessToken:   account.Token,
	}
	sessionJSON, err := json.Marshal(sessionData)
	if err != nil {
		t.Fatalf("marshal session data: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "session.json"), sessionJSON, 0600); err != nil {
		t.Fatalf("write session.json: %v", err)
	}
}

// resolveSystemRoom resolves the #bureau/system room ID.
func resolveSystemRoom(t *testing.T, admin *messaging.DirectSession) ref.RoomID {
	t.Helper()

	systemRoomID, err := admin.ResolveAlias(t.Context(), testNamespace.SystemRoomAlias())
	if err != nil {
		t.Fatalf("resolve system room: %v", err)
	}
	return systemRoomID
}

// inviteToRooms invites a user to one or more rooms, ignoring M_FORBIDDEN
// (already joined).
func inviteToRooms(t *testing.T, admin *messaging.DirectSession, userID ref.UserID, roomIDs ...ref.RoomID) {
	t.Helper()

	ctx := t.Context()
	for _, roomID := range roomIDs {
		if err := admin.InviteUser(ctx, roomID, userID); err != nil {
			if !messaging.IsMatrixError(err, "M_FORBIDDEN") {
				t.Fatalf("invite %s to %s: %v", userID, roomID, err)
			}
		}
	}
}

// createTicketProjectRoom creates a project room with ticket config,
// room service binding, and appropriate power levels. Invites the ticket
// service account and any additional users (machine accounts, etc.)
// so they can participate.
func createTicketProjectRoom(t *testing.T, admin *messaging.DirectSession, name string, ticketServiceEntity ref.Entity, additionalInvites ...string) ref.RoomID {
	t.Helper()

	ctx := t.Context()
	adminUserID := "@bureau-admin:" + testServerName
	ticketServiceUserID := ticketServiceEntity.UserID().String()

	invitees := append([]string{ticketServiceUserID}, additionalInvites...)
	room, err := admin.CreateRoom(ctx, messaging.CreateRoomRequest{
		Name:   name,
		Invite: invitees,
		PowerLevelContentOverride: map[string]any{
			"users": map[string]any{
				adminUserID:         100,
				ticketServiceUserID: 10,
			},
			"events": map[string]any{
				string(schema.EventTypeTicket):       10,
				string(schema.EventTypeTicketConfig): 100,
				string(schema.EventTypeRoomService):  100,
			},
		},
	})
	if err != nil {
		t.Fatalf("create room %s: %v", name, err)
	}

	if _, err := admin.SendStateEvent(ctx, room.RoomID, schema.EventTypeTicketConfig, "",
		schema.TicketConfigContent{
			Version: schema.TicketConfigVersion,
			Prefix:  "tkt",
		}); err != nil {
		t.Fatalf("publish ticket config for %s: %v", name, err)
	}

	if _, err := admin.SendStateEvent(ctx, room.RoomID, schema.EventTypeRoomService, "ticket",
		schema.RoomServiceContent{
			Principal: ticketServiceEntity,
		}); err != nil {
		t.Fatalf("publish room service binding for %s: %v", name, err)
	}

	return room.RoomID
}

// waitForMockCompletion blocks until the mock LLM has finished all steps
// or the test context expires.
func waitForMockCompletion(t *testing.T, mock *mockToolSequenceServer) {
	t.Helper()

	select {
	case <-mock.AllStepsCompleted:
	case <-t.Context().Done():
		t.Fatal("timed out waiting for mock LLM to complete")
	}
}

// waitForTicket watches a project room for a new m.bureau.ticket state
// event with the given title and returns its ticket ID (state key) and
// content. This is the production observation path: clients watch Matrix
// state events to learn about ticket changes.
func waitForTicket(t *testing.T, watch *roomWatch, expectedTitle string) (string, schema.TicketContent) {
	t.Helper()

	event := watch.WaitForEvent(t, func(event messaging.Event) bool {
		if event.Type != schema.EventTypeTicket {
			return false
		}
		title, _ := event.Content["title"].(string)
		return title == expectedTitle
	}, "ticket with title "+expectedTitle)

	ticketID := ""
	if event.StateKey != nil {
		ticketID = *event.StateKey
	}
	if ticketID == "" {
		t.Fatal("ticket state event has no state key")
	}

	contentJSON, err := json.Marshal(event.Content)
	if err != nil {
		t.Fatalf("marshal ticket content: %v", err)
	}
	var content schema.TicketContent
	if err := json.Unmarshal(contentJSON, &content); err != nil {
		t.Fatalf("unmarshal ticket content: %v", err)
	}

	return ticketID, content
}

// readTicketState reads the current m.bureau.ticket state event for a
// known ticket ID. Used after operations (update, close, reopen) where
// the ticket ID is already known from a previous waitForTicket call.
func readTicketState(t *testing.T, admin *messaging.DirectSession, roomID ref.RoomID, ticketID string) schema.TicketContent {
	t.Helper()

	raw, err := admin.GetStateEvent(t.Context(), roomID, schema.EventTypeTicket, ticketID)
	if err != nil {
		t.Fatalf("get ticket %s state: %v", ticketID, err)
	}

	var content schema.TicketContent
	if err := json.Unmarshal(raw, &content); err != nil {
		t.Fatalf("unmarshal ticket %s: %v", ticketID, err)
	}
	return content
}
