// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	ticketcmd "github.com/bureau-foundation/bureau/cmd/bureau/ticket"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/stewardship"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/lib/templatedef"
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

	ticketSvc := deployTicketService(t, admin, fleet, machine, "agent-test")
	ticketServiceEntity := ticketSvc.Entity

	// Create a project room with ticket config and service binding.
	projectRoomID := createTicketProjectRoom(t, admin, "ticket-agent-project",
		ticketServiceEntity, machine.UserID.String())

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
				{Actions: []string{schema.ActionCommandTicketAll, ticket.ActionAll}},
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
	if _, err := admin.SendMessage(ctx, machine.ConfigRoomID, messaging.NewTargetedTextMessage("Create a ticket", agent.Account.UserID)); err != nil {
		t.Fatalf("sending prompt to agent: %v", err)
	}

	waitForMockCompletion(t, mock)

	// --- Verification via Matrix state events ---
	//
	// The ticket service wrote m.bureau.ticket to the project room.
	// Read it the same way a production observer would.
	ticketID, ticketContent := waitForTicket(t, &projectWatch, "Agent-created ticket")

	if ticketContent.Status != ticket.StatusOpen {
		t.Errorf("status = %q, want %s", ticketContent.Status, ticket.StatusOpen)
	}
	if ticketContent.Type != ticket.TypeTask {
		t.Errorf("type = %q, want %s", ticketContent.Type, ticket.TypeTask)
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

	ticketSvc := deployTicketService(t, admin, fleet, machine, "lifecycle")
	ticketServiceEntity := ticketSvc.Entity

	// Two project rooms for cross-room filing.
	roomAlphaID := createTicketProjectRoom(t, admin, "lifecycle-alpha",
		ticketServiceEntity, machine.UserID.String())
	roomBetaID := createTicketProjectRoom(t, admin, "lifecycle-beta",
		ticketServiceEntity, machine.UserID.String())

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
	_, err = templatedef.Push(ctx, admin, agentTemplateRef, agentTemplateContent(agentBinary, agentOptions{
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

	floorWorkerAccount := registerFleetPrincipal(t, fleet, "agent/lifecycle-floor", "floor-password")
	pushCredentials(t, admin, machine, floorWorkerAccount)
	joinConfigRoom(t, admin, machine.ConfigRoomID, floorWorkerAccount)

	pmGrants := []string{schema.ActionCommandTicketAll, ticket.ActionAll}
	workerGrants := []string{
		schema.ActionCommandTicketAll,
		ticket.ActionCreate,
		ticket.ActionUpdate,
		ticket.ActionList,
		ticket.ActionReady,
		ticket.ActionShow,
		ticket.ActionBlocked,
		ticket.ActionStats,
		ticket.ActionGrep,
		ticket.ActionDeps,
		ticket.ActionRanked,
		ticket.ActionChildren,
		ticket.ActionEpicHealth,
		ticket.ActionInfo,
	}

	// The floor worker has MCP tool access but zero ticket mutation
	// grants. All mutation authority comes from the assignee floor:
	// if the floor worker is the current assignee, it can update
	// body/status/labels, add notes, and close — but cannot modify
	// structural fields (title, priority, type, etc.) or reopen.
	floorWorkerGrants := []string{
		schema.ActionCommandTicketAll,
		ticket.ActionList,
		ticket.ActionShow,
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
				Localpart: account.Localpart,
				Template:  templateRef,
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

		proxySocket := machine.PrincipalProxySocketPath(t, account.Localpart)
		waitForFile(t, proxySocket)
		readyWatch.WaitForMessage(t, "agent-ready", account.UserID)

		currentDeployedLocalpart = account.Localpart
		currentAdminSocket = machine.PrincipalProxyAdminSocketPath(t, account.Localpart)
		return currentAdminSocket
	}

	// sendStep registers a mock on the admin socket, sends a targeted prompt
	// to the config room, and waits for mock completion. The target scopes the
	// message to a specific agent so other agents in the room ignore it.
	sendStep := func(t *testing.T, adminSocket string, target ref.UserID, steps []mockToolStep, prompt string) {
		t.Helper()
		mock := newMockToolSequence(t, steps)
		registerProxyHTTPService(t, adminSocket, "anthropic", mock.URL)
		if _, err := admin.SendMessage(ctx, machine.ConfigRoomID, messaging.NewTargetedTextMessage(prompt, target)); err != nil {
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

		sendStep(t, adminSocket, pmAccount.UserID, []mockToolStep{{
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
		if alphaContent.Type != ticket.TypeTask {
			t.Errorf("alpha type = %q, want %s", alphaContent.Type, ticket.TypeTask)
		}
		t.Logf("alpha ticket %s created", alphaTicketID)

		sendStep(t, adminSocket, pmAccount.UserID, []mockToolStep{{
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
		if betaContent.Type != ticket.TypeFeature {
			t.Errorf("beta type = %q, want %s", betaContent.Type, ticket.TypeFeature)
		}
		t.Logf("beta ticket %s created — cross-room filing verified", betaTicketID)
	})

	t.Run("FullLifecycle", func(t *testing.T) {
		projectWatch := watchRoom(t, admin, roomAlphaID)

		adminSocket := deployAgent(t, pmAccount, pmGrants)

		// Create.
		sendStep(t, adminSocket, pmAccount.UserID, []mockToolStep{{
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
		if content.Status != ticket.StatusOpen {
			t.Errorf("after create: status = %q, want %s", content.Status, ticket.StatusOpen)
		}
		t.Logf("created %s", ticketID)

		// Claim (open → in_progress).
		pmUserID := pmAccount.UserID
		sendStep(t, adminSocket, pmAccount.UserID, []mockToolStep{{
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
		if content.Status != ticket.StatusInProgress {
			t.Errorf("after claim: status = %q, want %s", content.Status, ticket.StatusInProgress)
		}
		if content.Assignee != pmUserID {
			t.Errorf("after claim: assignee = %s, want %s", content.Assignee, pmUserID)
		}

		// Close (in_progress → closed).
		sendStep(t, adminSocket, pmAccount.UserID, []mockToolStep{{
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
		if content.Status != ticket.StatusClosed {
			t.Errorf("after close: status = %q, want %s", content.Status, ticket.StatusClosed)
		}
		if content.CloseReason != "completed" {
			t.Errorf("after close: reason = %q, want completed", content.CloseReason)
		}

		// Reopen (closed → open).
		sendStep(t, adminSocket, pmAccount.UserID, []mockToolStep{{
			ToolName: "bureau_ticket_reopen",
			ToolInput: func() map[string]any {
				return map[string]any{
					"room":   roomAlphaID,
					"ticket": ticketID,
				}
			},
		}}, "Reopen ticket")

		content = readTicketState(t, admin, roomAlphaID, ticketID)
		if content.Status != ticket.StatusOpen {
			t.Errorf("after reopen: status = %q, want %s", content.Status, ticket.StatusOpen)
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

		sendStep(t, workerSocket, workerAccount.UserID, []mockToolStep{{
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
		sendStep(t, workerSocket, workerAccount.UserID, []mockToolStep{{
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
		if content.Status != ticket.StatusInProgress {
			t.Errorf("after worker claim: status = %q, want %s", content.Status, ticket.StatusInProgress)
		}

		// Worker closes — assignee floor grants implicit close
		// permission even without a ticket/close grant. Assignment
		// is delegation of work authority; closing is how the
		// assignee signals completion.
		sendStep(t, workerSocket, workerAccount.UserID, []mockToolStep{{
			ToolName: "bureau_ticket_close",
			ToolInput: func() map[string]any {
				return map[string]any{
					"room":   roomAlphaID,
					"ticket": ticketID,
					"reason": "done",
				}
			},
		}}, "Worker: close ticket (assignee floor)")

		content = readTicketState(t, admin, roomAlphaID, ticketID)
		if content.Status != ticket.StatusClosed {
			t.Errorf("after worker close: status = %q, want %s (assignee floor should allow close)", content.Status, ticket.StatusClosed)
		}
		t.Log("worker close succeeded via assignee floor")

		// Worker tries to reopen — reopen is not in the assignee
		// floor and the worker has no ticket/reopen grant.
		workerSocket = deployAgent(t, workerAccount, workerGrants)

		sendStep(t, workerSocket, workerAccount.UserID, []mockToolStep{{
			ToolName: "bureau_ticket_reopen",
			ToolInput: func() map[string]any {
				return map[string]any{
					"room":   roomAlphaID,
					"ticket": ticketID,
				}
			},
		}}, "Worker: reopen ticket")

		content = readTicketState(t, admin, roomAlphaID, ticketID)
		if content.Status != ticket.StatusClosed {
			t.Errorf("after worker reopen attempt: status = %q, want %s (reopen should be denied)", content.Status, ticket.StatusClosed)
		}
		t.Log("worker reopen correctly denied by service")

		// Switch to PM — reopen succeeds.
		pmSocket := deployAgent(t, pmAccount, pmGrants)

		sendStep(t, pmSocket, pmAccount.UserID, []mockToolStep{{
			ToolName: "bureau_ticket_reopen",
			ToolInput: func() map[string]any {
				return map[string]any{
					"room":   roomAlphaID,
					"ticket": ticketID,
				}
			},
		}}, "PM: reopen ticket")

		content = readTicketState(t, admin, roomAlphaID, ticketID)
		if content.Status != ticket.StatusOpen {
			t.Errorf("after PM reopen: status = %q, want %s", content.Status, ticket.StatusOpen)
		}

		t.Logf("asymmetric permissions verified for %s", ticketID)
	})

	t.Run("ContentionDetection", func(t *testing.T) {
		projectWatch := watchRoom(t, admin, roomAlphaID)

		// Worker creates and claims a ticket.
		workerSocket := deployAgent(t, workerAccount, workerGrants)

		sendStep(t, workerSocket, workerAccount.UserID, []mockToolStep{{
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
		sendStep(t, workerSocket, workerAccount.UserID, []mockToolStep{{
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
		sendStep(t, pmSocket, pmAccount.UserID, []mockToolStep{{
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

	t.Run("AssigneeFloor", func(t *testing.T) {
		projectWatch := watchRoom(t, admin, roomAlphaID)

		// PM creates a ticket and assigns it to the floor worker.
		// The floor worker has zero mutation grants — all write
		// authority comes from being the ticket's assignee.
		pmSocket := deployAgent(t, pmAccount, pmGrants)

		floorWorkerUserID := floorWorkerAccount.UserID
		sendStep(t, pmSocket, pmAccount.UserID, []mockToolStep{{
			ToolName: "bureau_ticket_create",
			ToolInput: func() map[string]any {
				return map[string]any{
					"room":     roomAlphaID,
					"title":    "Assignee floor test",
					"type":     "task",
					"priority": 2,
				}
			},
		}}, "PM: create ticket")

		ticketID, _ := waitForTicket(t, &projectWatch, "Assignee floor test")
		t.Logf("PM created %s", ticketID)

		sendStep(t, pmSocket, pmAccount.UserID, []mockToolStep{{
			ToolName: "bureau_ticket_update",
			ToolInput: func() map[string]any {
				return map[string]any{
					"room":     roomAlphaID,
					"ticket":   ticketID,
					"status":   "in_progress",
					"assignee": floorWorkerUserID,
				}
			},
		}}, "PM: assign to floor worker")

		content := readTicketState(t, admin, roomAlphaID, ticketID)
		if content.Assignee != floorWorkerUserID {
			t.Fatalf("setup: assignee = %s, want %s", content.Assignee, floorWorkerUserID)
		}
		t.Logf("PM assigned %s to floor worker", ticketID)

		// Deploy floor worker (zero mutation grants). The daemon
		// mints a service token with only list/show — no ticket/update,
		// ticket/close, or ticket/reopen. The assignee floor in the
		// ticket service is the only thing allowing mutations.
		floorSocket := deployAgent(t, floorWorkerAccount, floorWorkerGrants)

		// Floor worker updates body — allowed by assignee floor.
		sendStep(t, floorSocket, floorWorkerUserID, []mockToolStep{{
			ToolName: "bureau_ticket_update",
			ToolInput: func() map[string]any {
				return map[string]any{
					"room":   roomAlphaID,
					"ticket": ticketID,
					"body":   "Floor worker added context",
				}
			},
		}}, "Floor worker: update body")

		content = readTicketState(t, admin, roomAlphaID, ticketID)
		if content.Body != "Floor worker added context" {
			t.Errorf("after body update: body = %q, want %q", content.Body, "Floor worker added context")
		}
		t.Log("floor worker body update succeeded via assignee floor")

		// Floor worker closes — allowed by assignee floor.
		sendStep(t, floorSocket, floorWorkerUserID, []mockToolStep{{
			ToolName: "bureau_ticket_close",
			ToolInput: func() map[string]any {
				return map[string]any{
					"room":   roomAlphaID,
					"ticket": ticketID,
					"reason": "completed via floor",
				}
			},
		}}, "Floor worker: close ticket")

		content = readTicketState(t, admin, roomAlphaID, ticketID)
		if content.Status != ticket.StatusClosed {
			t.Errorf("after close: status = %q, want %s", content.Status, ticket.StatusClosed)
		}
		t.Log("floor worker close succeeded via assignee floor")

		// Floor worker tries to update title — denied by assignee
		// floor field restriction (title is structural, not floor).
		// The tool call returns an error; ticket state is unchanged.
		floorSocket = deployAgent(t, floorWorkerAccount, floorWorkerGrants)

		sendStep(t, floorSocket, floorWorkerUserID, []mockToolStep{{
			ToolName: "bureau_ticket_update",
			ToolInput: func() map[string]any {
				return map[string]any{
					"room":   roomAlphaID,
					"ticket": ticketID,
					"title":  "Hijacked title",
				}
			},
		}}, "Floor worker: update title (should fail)")

		content = readTicketState(t, admin, roomAlphaID, ticketID)
		if content.Title != "Assignee floor test" {
			t.Errorf("after title update attempt: title = %q, want %q (should be denied)", content.Title, "Assignee floor test")
		}
		t.Log("floor worker title update correctly denied")

		// Floor worker tries to reopen — denied (reopen is not in
		// the assignee floor).
		sendStep(t, floorSocket, floorWorkerUserID, []mockToolStep{{
			ToolName: "bureau_ticket_reopen",
			ToolInput: func() map[string]any {
				return map[string]any{
					"room":   roomAlphaID,
					"ticket": ticketID,
				}
			},
		}}, "Floor worker: reopen (should fail)")

		content = readTicketState(t, admin, roomAlphaID, ticketID)
		if content.Status != ticket.StatusClosed {
			t.Errorf("after reopen attempt: status = %q, want %s (reopen should be denied)", content.Status, ticket.StatusClosed)
		}
		t.Log("floor worker reopen correctly denied")

		// PM reopens — full grants allow it.
		pmSocket = deployAgent(t, pmAccount, pmGrants)

		sendStep(t, pmSocket, pmAccount.UserID, []mockToolStep{{
			ToolName: "bureau_ticket_reopen",
			ToolInput: func() map[string]any {
				return map[string]any{
					"room":   roomAlphaID,
					"ticket": ticketID,
				}
			},
		}}, "PM: reopen ticket")

		content = readTicketState(t, admin, roomAlphaID, ticketID)
		if content.Status != ticket.StatusOpen {
			t.Errorf("after PM reopen: status = %q, want %s", content.Status, ticket.StatusOpen)
		}

		t.Logf("assignee floor permissions verified for %s", ticketID)
	})
}

// TestStewardship exercises the stewardship governance flow end-to-end:
// publishing stewardship declarations, creating tickets with affected
// resources, verifying auto-configured review gates and reviewers,
// approving reviews via set-disposition, and confirming gate
// satisfaction. Uses direct service socket calls (minted test tokens)
// for ticket operations and admin Matrix state events for stewardship
// declarations.
func TestStewardship(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	admin := adminSession(t)
	defer admin.Close()

	fleet := createTestFleet(t, admin)

	// Boot a machine.
	machine := newTestMachine(t, fleet, "stewardship")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	})

	// Deploy ticket service with socket access.
	ticketSvc := deployTicketService(t, admin, fleet, machine, "stewardship")

	// Mint a service token for direct socket calls. The subject is a
	// synthetic entity — the service validates the token signature and
	// grants, not the subject's existence.
	operatorEntity, err := ref.NewEntityFromAccountLocalpart(fleet.Ref, "agent/stewardship-test")
	if err != nil {
		t.Fatalf("construct operator entity: %v", err)
	}
	token := mintTestServiceToken(t, machine, operatorEntity, "ticket",
		[]servicetoken.Grant{{Actions: []string{ticket.ActionAll}}})
	client := service.NewServiceClientFromToken(ticketSvc.SocketPath, token)

	adminUserID := admin.UserID()

	// publishStewardshipRoom creates a project room with a pre-loaded
	// stewardship declaration. The declaration is published BEFORE enabling
	// tickets (which invites the service), so it's in the room state when
	// the service joins. The service indexes stewardship events during
	// processRoomState, then announces readiness — the WaitForEvent on
	// m.bureau.service_ready is an event-driven proof that the stewardship
	// index is populated.
	publishStewardshipRoom := func(
		t *testing.T,
		name string,
		stateKey string,
		declaration stewardship.StewardshipContent,
		ticketPrefix string,
	) ref.RoomID {
		t.Helper()

		room, err := admin.CreateRoom(ctx, messaging.CreateRoomRequest{
			Name:   name,
			Invite: []string{machine.UserID.String()},
		})
		if err != nil {
			t.Fatalf("create room %s: %v", name, err)
		}

		// Publish stewardship declaration before inviting the service.
		if _, err := admin.SendStateEvent(ctx, room.RoomID,
			schema.EventTypeStewardship, stateKey, declaration); err != nil {
			t.Fatalf("publish stewardship in %s: %v", name, err)
		}

		// Enable tickets — invites the service, which joins, processes
		// room state (including stewardship), and announces readiness.
		roomWatch := watchRoom(t, admin, room.RoomID)
		enableTicketsInRoom(t, admin, room.RoomID, ticketSvc, ticketPrefix, nil)

		roomWatch.WaitForEvent(t, func(event messaging.Event) bool {
			return event.Type == schema.EventTypeServiceReady &&
				event.Sender == ticketSvc.Account.UserID
		}, "ticket service ready in "+name)

		return room.RoomID
	}

	// --- Shared project room for core stewardship tests ---
	//
	// Single-tier declaration: gate_types=["task"], pattern fleet/gpu/**,
	// principals matching admin-*:test.bureau.local (the per-test admin
	// created by adminSession), threshold 1.
	tierThreshold := 1
	projectRoomID := publishStewardshipRoom(t,
		"stewardship-project", "fleet/gpu",
		stewardship.StewardshipContent{
			Version:          1,
			ResourcePatterns: []string{"fleet/gpu/**"},
			GateTypes:        []ticket.TicketType{ticket.TypeTask},
			Tiers: []stewardship.StewardshipTier{{
				Principals: []string{"admin-*:" + testServerName},
				Threshold:  &tierThreshold,
			}},
		},
		"stw",
	)

	t.Run("Review", func(t *testing.T) {
		// Create a ticket with matching affects and type.
		var createResult struct {
			ID   string `json:"id"`
			Room string `json:"room"`
		}
		if err := client.Call(ctx, "create", map[string]any{
			"room":     projectRoomID.String(),
			"title":    "GPU quota increase",
			"type":     "task",
			"priority": 2,
			"affects":  []string{"fleet/gpu/a100"},
		}, &createResult); err != nil {
			t.Fatalf("create ticket: %v", err)
		}

		// Read ticket and verify stewardship-derived review gate.
		content := readTicketState(t, admin, projectRoomID, createResult.ID)

		var stewardshipGate *ticket.TicketGate
		for i := range content.Gates {
			if strings.HasPrefix(content.Gates[i].ID, "stewardship:") {
				stewardshipGate = &content.Gates[i]
				break
			}
		}
		if stewardshipGate == nil {
			t.Fatalf("no stewardship review gate; gates = %v", content.Gates)
		}
		if stewardshipGate.Type != ticket.GateReview {
			t.Errorf("gate type = %q, want review", stewardshipGate.Type)
		}

		// Verify reviewers include the admin (matched by admin-*:test.bureau.local).
		if content.Review == nil {
			t.Fatal("no review section")
		}
		if len(content.Review.Reviewers) == 0 {
			t.Fatal("no reviewers")
		}

		var adminReviewer *ticket.ReviewerEntry
		for i := range content.Review.Reviewers {
			if content.Review.Reviewers[i].UserID == adminUserID {
				adminReviewer = &content.Review.Reviewers[i]
				break
			}
		}
		if adminReviewer == nil {
			t.Fatalf("admin %s not in reviewers: %v", adminUserID, content.Review.Reviewers)
		}
		if adminReviewer.Disposition != ticket.DispositionPending {
			t.Errorf("admin disposition = %q, want pending", adminReviewer.Disposition)
		}
		if adminReviewer.Tier != 0 {
			t.Errorf("admin tier = %d, want 0", adminReviewer.Tier)
		}

		// Verify tier thresholds.
		if len(content.Review.TierThresholds) == 0 {
			t.Fatal("no tier thresholds")
		}
		if content.Review.TierThresholds[0].Threshold == nil || *content.Review.TierThresholds[0].Threshold != 1 {
			t.Errorf("tier 0 threshold = %v, want 1", content.Review.TierThresholds[0].Threshold)
		}

		// Transition to review status (required before set-disposition).
		if err := client.Call(ctx, "update", map[string]any{
			"room":   projectRoomID.String(),
			"ticket": createResult.ID,
			"status": string(ticket.StatusReview),
		}, nil); err != nil {
			t.Fatalf("transition to review: %v", err)
		}

		// Approve the review as the admin (who is the stewardship-
		// resolved reviewer). set-disposition uses token.Subject to
		// identify the caller, so we mint a token with the admin's
		// UserID as subject.
		adminToken := mintTestServiceTokenForUser(t, machine, adminUserID, "ticket",
			[]servicetoken.Grant{{Actions: []string{ticket.ActionAll}}})
		adminClient := service.NewServiceClientFromToken(ticketSvc.SocketPath, adminToken)

		// Watch for the gate satisfaction write. The set-disposition
		// handler writes the disposition change; the sync loop then
		// picks up the echo, evaluates gates, and writes a second
		// state event with the gate satisfied. We must wait for that
		// second write rather than reading immediately after the
		// disposition call returns.
		gateWatch := watchRoom(t, admin, projectRoomID)

		if err := adminClient.Call(ctx, "set-disposition", map[string]any{
			"room":        projectRoomID.String(),
			"ticket":      createResult.ID,
			"gate_id":     stewardshipGate.ID,
			"reviewer_id": adminUserID.String(),
			"disposition": "approved",
		}, nil); err != nil {
			t.Fatalf("set disposition: %v", err)
		}

		// Wait for the gate satisfaction write. The set-disposition
		// handler writes E1 (disposition change); the sync loop
		// then evaluates gates and writes E2 (gate satisfied). We
		// match on the gate's status field inside the event content.
		gateWatch.WaitForEvent(t, func(event messaging.Event) bool {
			if event.Type != schema.EventTypeTicket {
				return false
			}
			if event.StateKey == nil || *event.StateKey != createResult.ID {
				return false
			}
			gates, _ := event.Content["gates"].([]any)
			for _, g := range gates {
				gateMap, _ := g.(map[string]any)
				if gateMap["id"] == stewardshipGate.ID && gateMap["status"] == "satisfied" {
					return true
				}
			}
			return false
		}, "stewardship gate satisfied")

		content = readTicketState(t, admin, projectRoomID, createResult.ID)

		for _, reviewer := range content.Review.Reviewers {
			if reviewer.UserID == adminUserID {
				if reviewer.Disposition != ticket.DispositionApproved {
					t.Errorf("after approval: disposition = %q, want approved", reviewer.Disposition)
				}
				break
			}
		}
		for _, gate := range content.Gates {
			if gate.ID == stewardshipGate.ID {
				if gate.Status != ticket.GateSatisfied {
					t.Errorf("stewardship gate status = %q, want satisfied", gate.Status)
				}
				break
			}
		}

		t.Logf("stewardship review verified: gate=%s, reviewers=%d",
			stewardshipGate.ID, len(content.Review.Reviewers))
	})

	t.Run("NoMatchingType", func(t *testing.T) {
		// Stewardship gates "task" tickets. A "bug" ticket with matching
		// affects should NOT get stewardship gates.
		var createResult struct {
			ID   string `json:"id"`
			Room string `json:"room"`
		}
		if err := client.Call(ctx, "create", map[string]any{
			"room":     projectRoomID.String(),
			"title":    "GPU driver crash",
			"type":     "bug",
			"priority": 2,
			"affects":  []string{"fleet/gpu/a100"},
		}, &createResult); err != nil {
			t.Fatalf("create ticket: %v", err)
		}

		content := readTicketState(t, admin, projectRoomID, createResult.ID)
		for _, gate := range content.Gates {
			if strings.HasPrefix(gate.ID, "stewardship:") {
				t.Errorf("unexpected stewardship gate %q for non-matching ticket type", gate.ID)
			}
		}
		t.Log("no stewardship gate for non-matching type confirmed")
	})

	t.Run("NoAffects", func(t *testing.T) {
		// A "task" ticket without affects should NOT get stewardship gates,
		// even though the room has a matching stewardship declaration.
		var createResult struct {
			ID   string `json:"id"`
			Room string `json:"room"`
		}
		if err := client.Call(ctx, "create", map[string]any{
			"room":     projectRoomID.String(),
			"title":    "Unrelated task",
			"type":     "task",
			"priority": 2,
		}, &createResult); err != nil {
			t.Fatalf("create ticket: %v", err)
		}

		content := readTicketState(t, admin, projectRoomID, createResult.ID)
		for _, gate := range content.Gates {
			if strings.HasPrefix(gate.ID, "stewardship:") {
				t.Errorf("unexpected stewardship gate %q for ticket without affects", gate.ID)
			}
		}
		t.Log("no stewardship gate without affects confirmed")
	})

	t.Run("NoMatchingResource", func(t *testing.T) {
		// Affects that don't match any stewardship declaration pattern
		// should produce no stewardship gates.
		var createResult struct {
			ID   string `json:"id"`
			Room string `json:"room"`
		}
		if err := client.Call(ctx, "create", map[string]any{
			"room":     projectRoomID.String(),
			"title":    "Network task",
			"type":     "task",
			"priority": 2,
			"affects":  []string{"network/switch/core-01"},
		}, &createResult); err != nil {
			t.Fatalf("create ticket: %v", err)
		}

		content := readTicketState(t, admin, projectRoomID, createResult.ID)
		for _, gate := range content.Gates {
			if strings.HasPrefix(gate.ID, "stewardship:") {
				t.Errorf("unexpected stewardship gate %q for non-matching resource", gate.ID)
			}
		}
		t.Log("no stewardship gate for non-matching resource confirmed")
	})

	t.Run("StewardshipResolve", func(t *testing.T) {
		// The stewardship-resolve action is a dry-run preview. Verify it
		// returns the expected matches without creating a ticket.
		var resolveResult struct {
			Gates     []ticket.TicketGate    `json:"gates"`
			Reviewers []ticket.ReviewerEntry `json:"reviewers"`
			Matches   []struct {
				MatchedPattern  string `json:"matched_pattern"`
				MatchedResource string `json:"matched_resource"`
			} `json:"matches"`
		}
		if err := client.Call(ctx, "stewardship-resolve", map[string]any{
			"affects":     []string{"fleet/gpu/a100"},
			"ticket_type": "task",
		}, &resolveResult); err != nil {
			t.Fatalf("stewardship-resolve: %v", err)
		}

		if len(resolveResult.Matches) == 0 {
			t.Fatal("no matches from stewardship-resolve")
		}
		if resolveResult.Matches[0].MatchedPattern != "fleet/gpu/**" {
			t.Errorf("matched pattern = %q, want fleet/gpu/**", resolveResult.Matches[0].MatchedPattern)
		}
		if len(resolveResult.Gates) == 0 {
			t.Fatal("no gates from stewardship-resolve")
		}
		if len(resolveResult.Reviewers) == 0 {
			t.Fatal("no reviewers from stewardship-resolve")
		}

		// Verify admin is in the resolved reviewers.
		var foundAdmin bool
		for _, reviewer := range resolveResult.Reviewers {
			if reviewer.UserID == adminUserID {
				foundAdmin = true
				break
			}
		}
		if !foundAdmin {
			t.Errorf("admin %s not in resolved reviewers: %v", adminUserID, resolveResult.Reviewers)
		}

		t.Logf("stewardship-resolve: %d matches, %d gates, %d reviewers",
			len(resolveResult.Matches), len(resolveResult.Gates), len(resolveResult.Reviewers))
	})

	t.Run("StewardshipList", func(t *testing.T) {
		// The stewardship-list action returns all known declarations.
		var listResult []struct {
			RoomID           ref.RoomID `json:"room_id"`
			StateKey         string     `json:"state_key"`
			ResourcePatterns []string   `json:"resource_patterns"`
			GateTypes        []string   `json:"gate_types"`
		}
		if err := client.Call(ctx, "stewardship-list", nil, &listResult); err != nil {
			t.Fatalf("stewardship-list: %v", err)
		}

		// Find the declaration we published.
		var found bool
		for _, entry := range listResult {
			if entry.RoomID == projectRoomID && entry.StateKey == "fleet/gpu" {
				found = true
				if len(entry.ResourcePatterns) != 1 || entry.ResourcePatterns[0] != "fleet/gpu/**" {
					t.Errorf("patterns = %v, want [fleet/gpu/**]", entry.ResourcePatterns)
				}
				if len(entry.GateTypes) != 1 || entry.GateTypes[0] != "task" {
					t.Errorf("gate_types = %v, want [task]", entry.GateTypes)
				}
				break
			}
		}
		if !found {
			t.Errorf("declaration fleet/gpu in room %s not found in list (got %d entries)", projectRoomID, len(listResult))
		}

		t.Logf("stewardship-list: %d declarations", len(listResult))
	})
}

// --- Shared helpers for ticket service deployment ---

// ticketServiceDeployment holds the result of deploying a ticket service
// on a test machine. Tests use Entity to configure rooms with service
// bindings, the socket path for direct service calls, and the account
// for room membership and principal pattern matching.
type ticketServiceDeployment struct {
	Entity     ref.Entity       // fleet-scoped entity ref for service bindings
	Account    principalAccount // service account (UserID, Localpart, Token)
	SocketPath string           // path to the service CBOR socket
}

// deployTicketService deploys a ticket service using the production
// principal.Create() path. The suffix distinguishes multiple ticket
// services within the same fleet (each test needs a unique localpart
// to avoid registration collisions across parallel tests sharing the
// same homeserver).
func deployTicketService(t *testing.T, admin *messaging.DirectSession, fleet *testFleet, machine *testMachine, suffix string) ticketServiceDeployment {
	t.Helper()

	svc := deployService(t, admin, fleet, machine, serviceDeployOptions{
		Binary:    resolvedBinary(t, "TICKET_SERVICE_BINARY"),
		Name:      "ticket-" + suffix,
		Localpart: "service/ticket/" + suffix,
		MatrixPolicy: &schema.MatrixPolicy{
			AllowJoin: true,
		},
	})
	return ticketServiceDeployment{
		Entity:     svc.Entity,
		Account:    svc.Account,
		SocketPath: svc.SocketPath,
	}
}

// enableTicketsInRoom configures an existing room for ticket management
// using the production ticket.ConfigureRoom path. Publishes ticket config,
// service binding, invites the service, and configures power levels.
func enableTicketsInRoom(t *testing.T, admin *messaging.DirectSession, roomID ref.RoomID, ticketService ticketServiceDeployment, prefix string, allowedTypes []ticket.TicketType) {
	t.Helper()

	if err := ticketcmd.ConfigureRoom(t.Context(), slog.Default(), admin, roomID, ticketService.Entity, ticketcmd.ConfigureRoomParams{
		Prefix:       prefix,
		AllowedTypes: allowedTypes,
	}); err != nil {
		t.Fatalf("configure tickets in room %s: %v", roomID, err)
	}
}

// createTicketProjectRoom creates a project room with ticket config,
// service binding, and appropriate power levels. Invites the ticket
// service account and any additional users (machine accounts, etc.)
// so they can participate. Uses ticket.ConfigureRoom for the full
// production setup path.
func createTicketProjectRoom(t *testing.T, admin *messaging.DirectSession, name string, ticketServiceEntity ref.Entity, additionalInvites ...string) ref.RoomID {
	t.Helper()

	ctx := t.Context()

	room, err := admin.CreateRoom(ctx, messaging.CreateRoomRequest{
		Name:   name,
		Invite: additionalInvites,
	})
	if err != nil {
		t.Fatalf("create room %s: %v", name, err)
	}

	if err := ticketcmd.ConfigureRoom(ctx, slog.Default(), admin, room.RoomID, ticketServiceEntity, ticketcmd.ConfigureRoomParams{
		Prefix: "tkt",
	}); err != nil {
		t.Fatalf("configure tickets in room %s: %v", name, err)
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
func waitForTicket(t *testing.T, watch *roomWatch, expectedTitle string) (string, ticket.TicketContent) {
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
	var content ticket.TicketContent
	if err := json.Unmarshal(contentJSON, &content); err != nil {
		t.Fatalf("unmarshal ticket content: %v", err)
	}

	return ticketID, content
}

// readTicketState reads the current m.bureau.ticket state event for a
// known ticket ID. Used after operations (update, close, reopen) where
// the ticket ID is already known from a previous waitForTicket call.
func readTicketState(t *testing.T, admin *messaging.DirectSession, roomID ref.RoomID, ticketID string) ticket.TicketContent {
	t.Helper()

	raw, err := admin.GetStateEvent(t.Context(), roomID, schema.EventTypeTicket, ticketID)
	if err != nil {
		t.Fatalf("get ticket %s state: %v", ticketID, err)
	}

	var content ticket.TicketContent
	if err := json.Unmarshal(raw, &content); err != nil {
		t.Fatalf("unmarshal ticket %s: %v", ticketID, err)
	}
	return content
}
