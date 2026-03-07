// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	ticketcmd "github.com/bureau-foundation/bureau/cmd/bureau/ticket"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/messaging"
)

// --- Reservation test helpers ---

// setupReservationOpsRoom resolves the machine's ops room (created during
// provisioning), invites the ticket service and fleet controller, grants
// appropriate power levels, enables ticket management for resource_request
// tickets, and publishes a relay policy allowing relay from fleet members.
//
// Returns the ops room ID after the ticket service has announced readiness.
func setupReservationOpsRoom(
	t *testing.T,
	admin *messaging.DirectSession,
	machine *testMachine,
	ticketSvc ticketServiceDeployment,
	fleetController *fleetController,
	fleet *testFleet,
) ref.RoomID {
	t.Helper()
	ctx := t.Context()

	// Resolve the ops room alias. Machine provisioning creates this room.
	opsAlias := machine.Ref.OpsRoomAlias()
	opsRoomID, err := admin.ResolveAlias(ctx, opsAlias)
	if err != nil {
		t.Fatalf("resolve ops room alias %s: %v", opsAlias, err)
	}

	// Admin joins the ops room (provisioning only invites the machine).
	if _, err := admin.JoinRoom(ctx, opsRoomID); err != nil {
		t.Fatalf("admin join ops room: %v", err)
	}

	// Invite the fleet controller (not the ticket service yet — that
	// happens via enableTicketsInRoom below, and we need the watch
	// established before the ticket service joins and announces ready).
	inviteToRooms(t, admin, fleetController.UserID, opsRoomID)

	// Publish relay policy before enabling tickets. The ticket service
	// reads relay policy on join, so it must be present first.
	relayPolicy := schema.RelayPolicy{
		Sources: []schema.RelaySource{{
			Match: schema.RelayMatchFleetMember,
			Fleet: fleet.Ref.Localpart(),
		}},
		AllowedTypes: []string{
			string(ticket.TypeResourceRequest),
			string(ticket.TypePipeline),
		},
	}
	if _, err := admin.SendStateEvent(ctx, opsRoomID, schema.EventTypeRelayPolicy, "", relayPolicy); err != nil {
		t.Fatalf("publish relay policy: %v", err)
	}

	// Watch BEFORE enabling tickets — enableTicketsInRoom invites the
	// ticket service, which triggers it to join, process ticket_config,
	// and announce service_ready. The watch must be in place before that.
	opsWatch := watchRoom(t, admin, opsRoomID)

	// Enable ticket management for resource_request tickets. This
	// invites the ticket service to the room and calls
	// configureTicketPowerLevels which sets the service user to PL 10.
	enableTicketsInRoom(t, admin, opsRoomID, ticketSvc, "rsv",
		[]ticket.TicketType{ticket.TypeResourceRequest})

	// Grant ops room power levels AFTER enableTicketsInRoom. The
	// configureTicketPowerLevels function (called by enableTicketsInRoom)
	// sets the ticket service to PL 10 for standard ticket rooms. Ops
	// rooms need PL 25 for the ticket service (to publish relay links
	// and drain_status), PL 50 for the fleet controller (to publish
	// reservations and drain), and PL 50 for the machine daemon (to
	// publish drain_status). This read-modify-write overrides the PL 10.
	if err := schema.GrantPowerLevels(ctx, admin, opsRoomID, schema.PowerLevelGrants{
		Users: map[ref.UserID]int{
			ticketSvc.Account.UserID: 25,
			fleetController.UserID:   50,
			machine.UserID:           50,
		},
	}); err != nil {
		t.Fatalf("grant ops room power levels: %v", err)
	}

	// Wait for ticket service readiness in the ops room.
	opsWatch.WaitForEvent(t, func(event messaging.Event) bool {
		return event.Type == schema.EventTypeServiceReady &&
			event.Sender == ticketSvc.Account.UserID
	}, "ticket service ready in ops room")

	return opsRoomID
}

// createReservationWorkspaceRoom creates a workspace room with a fleet-scoped
// alias, enables ticket management for resource_request tickets, and waits
// for the ticket service to announce readiness.
//
// The fleet-scoped alias is critical: the ticket service extracts fleet identity
// from the canonical alias via fleetFromRoomAlias. Without it, relay initiation
// fails with "cannot determine fleet from workspace room."
func createReservationWorkspaceRoom(
	t *testing.T,
	admin *messaging.DirectSession,
	fleet *testFleet,
	ticketSvc ticketServiceDeployment,
	name string,
) ref.RoomID {
	t.Helper()
	ctx := t.Context()

	alias := fleet.Ref.Localpart() + "/workspace/" + name
	room, err := admin.CreateRoom(ctx, messaging.CreateRoomRequest{
		Name:  "Workspace: " + name,
		Alias: alias,
	})
	if err != nil {
		t.Fatalf("create workspace room %s: %v", name, err)
	}

	// Configure ticket management using the production path.
	if err := ticketcmd.ConfigureRoom(ctx, slog.Default(), admin, room.RoomID, ticketSvc.Entity, ticketcmd.ConfigureRoomParams{
		Prefix:       "rsv",
		AllowedTypes: []ticket.TicketType{ticket.TypeResourceRequest},
	}); err != nil {
		t.Fatalf("configure tickets in workspace room %s: %v", name, err)
	}

	// Wait for ticket service readiness.
	wsWatch := watchRoom(t, admin, room.RoomID)
	wsWatch.WaitForEvent(t, func(event messaging.Event) bool {
		return event.Type == schema.EventTypeServiceReady &&
			event.Sender == ticketSvc.Account.UserID
	}, "ticket service ready in workspace "+name)

	return room.RoomID
}

// publishResourceRequest creates a resource_request ticket via the ticket
// service's CBOR socket API. The ticket has a single machine claim with
// the given mode, priority, and max duration. Returns the ticket ID.
//
// Using the ticket service API (not direct SendStateEvent) is critical:
// the relay initiation path (checkAndInitiateRelay) is only triggered
// after the service's own create handler, not when a ticket arrives via
// /sync from an external publisher.
func publishResourceRequest(
	t *testing.T,
	ticketClient *service.ServiceClient,
	roomID ref.RoomID,
	machineName string,
	mode schema.ReservationMode,
	priority int,
	maxDuration string,
) string {
	t.Helper()

	var result ticket.CreateResponse
	if err := ticketClient.Call(t.Context(), "create", map[string]any{
		"room":     roomID.String(),
		"title":    fmt.Sprintf("Resource request: machine/%s", machineName),
		"type":     string(ticket.TypeResourceRequest),
		"priority": priority,
		"reservation": map[string]any{
			"claims": []map[string]any{{
				"resource": map[string]any{
					"type":   string(schema.ResourceMachine),
					"target": machineName,
				},
				"mode":   string(mode),
				"status": string(schema.ClaimPending),
			}},
			"max_duration": maxDuration,
		},
	}, &result); err != nil {
		t.Fatalf("create resource request ticket: %v", err)
	}
	t.Logf("created resource request %s (priority %d, mode %s, duration %s)", result.ID, priority, mode, maxDuration)

	return result.ID
}

// closeWorkspaceTicket closes a ticket via the ticket service API
// using the dedicated "close" action. This exercises the production
// close path which cascades relay ticket closure to ops rooms.
func closeWorkspaceTicket(
	t *testing.T,
	ticketClient *service.ServiceClient,
	roomID ref.RoomID,
	ticketID string,
	reason string,
) {
	t.Helper()

	if err := ticketClient.Call(t.Context(), "close", map[string]any{
		"room":   roomID.String(),
		"ticket": ticketID,
		"reason": reason,
	}, nil); err != nil {
		t.Fatalf("close workspace ticket %s: %v", ticketID, err)
	}
	t.Logf("closed workspace ticket %s: %s", ticketID, reason)
}

// waitForRelayTicket waits for a relay ticket to appear in an ops room.
// Matches on Type == resource_request and Title containing the machine name.
// Returns the relay ticket ID and content.
func waitForRelayTicket(t *testing.T, watch *roomWatch, machineName string) (string, ticket.TicketContent) {
	t.Helper()

	event := watch.WaitForEvent(t, func(event messaging.Event) bool {
		if event.Type != schema.EventTypeTicket {
			return false
		}
		ticketType, _ := event.Content["type"].(string)
		title, _ := event.Content["title"].(string)
		status, _ := event.Content["status"].(string)
		return ticketType == string(ticket.TypeResourceRequest) &&
			status == string(ticket.StatusOpen) &&
			containsSubstring(title, machineName)
	}, "relay ticket for "+machineName)

	ticketID := ""
	if event.StateKey != nil {
		ticketID = *event.StateKey
	}
	if ticketID == "" {
		t.Fatal("relay ticket event has no state key")
	}

	contentJSON, err := json.Marshal(event.Content)
	if err != nil {
		t.Fatalf("marshal relay ticket content: %v", err)
	}
	var content ticket.TicketContent
	if err := json.Unmarshal(contentJSON, &content); err != nil {
		t.Fatalf("unmarshal relay ticket content: %v", err)
	}

	t.Logf("relay ticket appeared: %s", ticketID)
	return ticketID, content
}

// waitForReservationGrant waits for an m.bureau.reservation state event
// with the given holder localpart and returns the parsed grant.
func waitForReservationGrant(t *testing.T, watch *roomWatch, holderLocalpart string) schema.ReservationGrant {
	t.Helper()

	raw := watch.WaitForStateEvent(t, schema.EventTypeReservation, holderLocalpart)
	var grant schema.ReservationGrant
	if err := json.Unmarshal(raw, &grant); err != nil {
		t.Fatalf("unmarshal reservation grant: %v", err)
	}
	t.Logf("reservation granted to %s (mode=%s, expires=%s)", grant.Holder, grant.Mode, grant.ExpiresAt)
	return grant
}

// waitForReservationCleared waits for the reservation grant to be cleared
// (empty content) for the given holder.
func waitForReservationCleared(t *testing.T, watch *roomWatch, holderLocalpart string) {
	t.Helper()

	watch.WaitForEvent(t, func(event messaging.Event) bool {
		if event.Type != schema.EventTypeReservation {
			return false
		}
		if event.StateKey == nil || *event.StateKey != holderLocalpart {
			return false
		}
		// Cleared reservation has empty or near-empty content.
		// The fleet controller publishes `{}` to clear.
		return len(event.Content) == 0
	}, "reservation cleared for "+holderLocalpart)
	t.Logf("reservation cleared for %s", holderLocalpart)
}

// waitForMachineDrain waits for an m.bureau.machine_drain state event
// and returns the parsed content.
func waitForMachineDrain(t *testing.T, watch *roomWatch) schema.MachineDrainContent {
	t.Helper()

	raw := watch.WaitForStateEvent(t, schema.EventTypeMachineDrain, "")
	var drain schema.MachineDrainContent
	if err := json.Unmarshal(raw, &drain); err != nil {
		t.Fatalf("unmarshal machine drain: %v", err)
	}
	t.Logf("machine drain published (holder=%s)", drain.ReservationHolder)
	return drain
}

// waitForDrainCleared waits for the machine drain to be cleared (empty content).
func waitForDrainCleared(t *testing.T, watch *roomWatch) {
	t.Helper()

	watch.WaitForEvent(t, func(event messaging.Event) bool {
		if event.Type != schema.EventTypeMachineDrain {
			return false
		}
		if event.StateKey == nil || *event.StateKey != "" {
			return false
		}
		return len(event.Content) == 0
	}, "machine drain cleared")
	t.Logf("machine drain cleared")
}

// waitForTicketStatus waits for a ticket state event with the given ticket
// ID to reach the expected status. Returns the parsed content.
func waitForTicketStatus(t *testing.T, watch *roomWatch, ticketID string, expectedStatus ticket.TicketStatus) ticket.TicketContent {
	t.Helper()

	event := watch.WaitForEvent(t, func(event messaging.Event) bool {
		if event.Type != schema.EventTypeTicket {
			return false
		}
		if event.StateKey == nil || *event.StateKey != ticketID {
			return false
		}
		status, _ := event.Content["status"].(string)
		return status == string(expectedStatus)
	}, fmt.Sprintf("ticket %s status=%s", ticketID, expectedStatus))

	contentJSON, err := json.Marshal(event.Content)
	if err != nil {
		t.Fatalf("marshal ticket content: %v", err)
	}
	var content ticket.TicketContent
	if err := json.Unmarshal(contentJSON, &content); err != nil {
		t.Fatalf("unmarshal ticket content: %v", err)
	}
	t.Logf("ticket %s reached status %s", ticketID, expectedStatus)
	return content
}

// waitForClaimStatus waits for a workspace ticket update where the first
// claim's status matches the expected value.
func waitForClaimStatus(t *testing.T, watch *roomWatch, ticketID string, expectedStatus schema.ClaimStatus) ticket.TicketContent {
	t.Helper()

	event := watch.WaitForEvent(t, func(event messaging.Event) bool {
		if event.Type != schema.EventTypeTicket {
			return false
		}
		if event.StateKey == nil || *event.StateKey != ticketID {
			return false
		}
		// Parse reservation.claims[0].status from the raw content.
		reservation, _ := event.Content["reservation"].(map[string]any)
		if reservation == nil {
			return false
		}
		claims, _ := reservation["claims"].([]any)
		if len(claims) == 0 {
			return false
		}
		firstClaim, _ := claims[0].(map[string]any)
		if firstClaim == nil {
			return false
		}
		status, _ := firstClaim["status"].(string)
		return status == string(expectedStatus)
	}, fmt.Sprintf("ticket %s claim status=%s", ticketID, expectedStatus))

	contentJSON, err := json.Marshal(event.Content)
	if err != nil {
		t.Fatalf("marshal ticket content: %v", err)
	}
	var content ticket.TicketContent
	if err := json.Unmarshal(contentJSON, &content); err != nil {
		t.Fatalf("unmarshal ticket content: %v", err)
	}
	t.Logf("ticket %s claim[0] reached status %s", ticketID, expectedStatus)
	return content
}

// containsSubstring reports whether s contains substr.
func containsSubstring(s, substr string) bool {
	return len(s) >= len(substr) && (substr == "" || findSubstring(s, substr))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// setupOpsRoomNoFC sets up a machine's ops room for relay without the fleet
// controller watching it. Ticket management and relay policy are configured,
// and the ticket service is invited with PL 25 for publishing relay links.
// Use this for tests that need relay ticket creation without FC grant
// processing (e.g., denial cascade tests where claims must stay pending).
func setupOpsRoomNoFC(
	t *testing.T,
	admin *messaging.DirectSession,
	machine *testMachine,
	ticketSvc ticketServiceDeployment,
	fleet *testFleet,
) ref.RoomID {
	t.Helper()
	ctx := t.Context()

	opsAlias := machine.Ref.OpsRoomAlias()
	opsRoomID, err := admin.ResolveAlias(ctx, opsAlias)
	if err != nil {
		t.Fatalf("resolve ops room alias %s: %v", opsAlias, err)
	}

	if _, err := admin.JoinRoom(ctx, opsRoomID); err != nil {
		t.Fatalf("admin join ops room: %v", err)
	}

	// Relay policy before enabling tickets (ticket service reads it on join).
	relayPolicy := schema.RelayPolicy{
		Sources: []schema.RelaySource{{
			Match: schema.RelayMatchFleetMember,
			Fleet: fleet.Ref.Localpart(),
		}},
		AllowedTypes: []string{
			string(ticket.TypeResourceRequest),
			string(ticket.TypePipeline),
		},
	}
	if _, err := admin.SendStateEvent(ctx, opsRoomID, schema.EventTypeRelayPolicy, "", relayPolicy); err != nil {
		t.Fatalf("publish relay policy: %v", err)
	}

	opsWatch := watchRoom(t, admin, opsRoomID)

	enableTicketsInRoom(t, admin, opsRoomID, ticketSvc, "rsv",
		[]ticket.TicketType{ticket.TypeResourceRequest})

	// Ticket service needs PL 25 for publishing relay links and
	// drain_status. Machine daemon needs PL 50 for drain_status.
	if err := schema.GrantPowerLevels(ctx, admin, opsRoomID, schema.PowerLevelGrants{
		Users: map[ref.UserID]int{
			ticketSvc.Account.UserID: 25,
			machine.UserID:           50,
		},
	}); err != nil {
		t.Fatalf("grant ops room power levels: %v", err)
	}

	opsWatch.WaitForEvent(t, func(event messaging.Event) bool {
		return event.Type == schema.EventTypeServiceReady &&
			event.Sender == ticketSvc.Account.UserID
	}, "ticket service ready in ops room")

	return opsRoomID
}

// createPipelineWorkspaceRoom creates a workspace room that allows both
// pipeline and resource_request ticket types. Pipeline tickets with
// reservations need a room that permits the pipeline type; the relay
// mechanism creates resource_request relay tickets in ops rooms regardless
// of the workspace ticket type, but the workspace room's AllowedTypes
// must include pipeline for the create API to accept the ticket.
func createPipelineWorkspaceRoom(
	t *testing.T,
	admin *messaging.DirectSession,
	fleet *testFleet,
	ticketSvc ticketServiceDeployment,
	name string,
) ref.RoomID {
	t.Helper()
	ctx := t.Context()

	alias := fleet.Ref.Localpart() + "/workspace/" + name
	room, err := admin.CreateRoom(ctx, messaging.CreateRoomRequest{
		Name:  "Workspace: " + name,
		Alias: alias,
	})
	if err != nil {
		t.Fatalf("create workspace room %s: %v", name, err)
	}

	// Configure ticket management allowing both pipeline and resource_request.
	// Pipeline tickets get the "pip" prefix from PrefixForType; resource_request
	// tickets get "rsv". The room prefix is a fallback for types without a
	// type-specific prefix.
	if err := ticketcmd.ConfigureRoom(ctx, slog.Default(), admin, room.RoomID, ticketSvc.Entity, ticketcmd.ConfigureRoomParams{
		Prefix:       "pip",
		AllowedTypes: []ticket.TicketType{ticket.TypePipeline, ticket.TypeResourceRequest},
	}); err != nil {
		t.Fatalf("configure tickets in workspace room %s: %v", name, err)
	}

	// Wait for ticket service readiness.
	wsWatch := watchRoom(t, admin, room.RoomID)
	wsWatch.WaitForEvent(t, func(event messaging.Event) bool {
		return event.Type == schema.EventTypeServiceReady &&
			event.Sender == ticketSvc.Account.UserID
	}, "ticket service ready in workspace "+name)

	return room.RoomID
}

// publishPipelineWithReservation creates a pipeline ticket with a
// reservation via the ticket service's CBOR socket API. The ticket
// carries both PipelineExecutionContent (pipeline_ref, total_steps)
// and ReservationContent (machine claim). Returns the ticket ID.
//
// This exercises the production path where a single ticket is both the
// pipeline execution record and the resource reservation holder — no
// separate resource_request orchestration ticket.
func publishPipelineWithReservation(
	t *testing.T,
	ticketClient *service.ServiceClient,
	roomID ref.RoomID,
	pipelineRef string,
	machineName string,
	mode schema.ReservationMode,
	priority int,
	maxDuration string,
) string {
	t.Helper()

	var result ticket.CreateResponse
	if err := ticketClient.Call(t.Context(), "create", map[string]any{
		"room":     roomID.String(),
		"title":    fmt.Sprintf("Pipeline: %s (machine/%s)", pipelineRef, machineName),
		"type":     string(ticket.TypePipeline),
		"priority": priority,
		"pipeline": map[string]any{
			"pipeline_ref": pipelineRef,
			"total_steps":  3,
		},
		"reservation": map[string]any{
			"claims": []map[string]any{{
				"resource": map[string]any{
					"type":   string(schema.ResourceMachine),
					"target": machineName,
				},
				"mode":   string(mode),
				"status": string(schema.ClaimPending),
			}},
			"max_duration": maxDuration,
		},
	}, &result); err != nil {
		t.Fatalf("create pipeline ticket with reservation: %v", err)
	}
	t.Logf("created pipeline ticket %s (pipeline=%s, machine=%s, mode=%s)",
		result.ID, pipelineRef, machineName, mode)

	return result.ID
}

// publishMultiClaimResourceRequest creates a resource_request ticket
// with claims on multiple machines via the ticket service socket API.
// Returns the ticket ID. Each machine gets an exclusive claim.
func publishMultiClaimResourceRequest(
	t *testing.T,
	ticketClient *service.ServiceClient,
	roomID ref.RoomID,
	machineNames []string,
	priority int,
	maxDuration string,
) string {
	t.Helper()

	claims := make([]map[string]any, len(machineNames))
	for index, name := range machineNames {
		claims[index] = map[string]any{
			"resource": map[string]any{
				"type":   string(schema.ResourceMachine),
				"target": name,
			},
			"mode":   string(schema.ModeExclusive),
			"status": string(schema.ClaimPending),
		}
	}

	var result ticket.CreateResponse
	if err := ticketClient.Call(t.Context(), "create", map[string]any{
		"room":     roomID.String(),
		"title":    fmt.Sprintf("Multi-claim request: %v", machineNames),
		"type":     string(ticket.TypeResourceRequest),
		"priority": priority,
		"reservation": map[string]any{
			"claims":       claims,
			"max_duration": maxDuration,
		},
	}, &result); err != nil {
		t.Fatalf("create multi-claim resource request: %v", err)
	}
	t.Logf("created multi-claim request %s (machines=%v, priority=%d)", result.ID, machineNames, priority)

	return result.ID
}

// waitForTicketNote waits for a ticket update that contains at least
// one note whose body matches the given substring.
func waitForTicketNote(t *testing.T, watch *roomWatch, ticketID string, bodySubstring string) ticket.TicketContent {
	t.Helper()

	event := watch.WaitForEvent(t, func(event messaging.Event) bool {
		if event.Type != schema.EventTypeTicket {
			return false
		}
		if event.StateKey == nil || *event.StateKey != ticketID {
			return false
		}
		notes, _ := event.Content["notes"].([]any)
		for _, noteRaw := range notes {
			note, _ := noteRaw.(map[string]any)
			body, _ := note["body"].(string)
			if containsSubstring(body, bodySubstring) {
				return true
			}
		}
		return false
	}, fmt.Sprintf("ticket %s note containing %q", ticketID, bodySubstring))

	contentJSON, err := json.Marshal(event.Content)
	if err != nil {
		t.Fatalf("marshal ticket content: %v", err)
	}
	var content ticket.TicketContent
	if err := json.Unmarshal(contentJSON, &content); err != nil {
		t.Fatalf("unmarshal ticket content: %v", err)
	}
	t.Logf("ticket %s has note containing %q", ticketID, bodySubstring)
	return content
}

// advanceFleetClock advances the fleet controller's fake clock by the
// given duration via the "advance-clock" socket action. Only works
// when the fleet controller was started with BUREAU_TEST_CLOCK=1.
func advanceFleetClock(t *testing.T, fc *fleetController, duration time.Duration) {
	t.Helper()

	client := service.NewServiceClientFromToken(fc.SocketPath, nil)
	var response service.AdvanceClockResponse
	if err := client.Call(t.Context(), "advance-clock", service.AdvanceClockRequest{
		DurationMS: duration.Milliseconds(),
	}, &response); err != nil {
		t.Fatalf("advance fleet controller clock by %s: %v", duration, err)
	}
	t.Logf("advanced fleet controller clock by %s (now: %s)",
		duration, time.Unix(response.NowUnix, 0).UTC().Format(time.RFC3339))
}

// --- Integration test ---

// TestReservationLifecycle exercises the full reservation flow across
// multiple Matrix rooms: workspace rooms, ops rooms, and fleet rooms.
// The ticket service relays resource_request tickets from workspace
// rooms into ops rooms, and the fleet controller grants, preempts,
// and expires reservations.
func TestReservationLifecycle(t *testing.T) {
	t.Parallel()

	// --- Shared setup ---

	ns := setupTestNamespace(t)
	admin := ns.Admin
	fleet := createTestFleet(t, admin, ns)

	machine := newTestMachine(t, fleet, "rsv-target")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	})

	ticketSvc := deployTicketService(t, admin, fleet, machine, "rsv")
	fc := startFleetController(t, admin, machine, "service/fleet/rsv", fleet,
		map[string]string{"BUREAU_TEST_CLOCK": "1"})

	// Register a fleet-scoped agent account as the ticket requester. The
	// relay code requires CreatedBy to be a fleet entity so it can publish
	// a RelayLink with the requester's identity. The fleet controller uses
	// this as the reservation holder (state_key for reservation grants).
	requester := registerFleetPrincipal(t, fleet, "agent/rsv-requester", "test-pass")

	// Create an authenticated ticket service client using the requester's
	// identity. The relay initiation path is only triggered by the service's
	// own create handler, not by direct SendStateEvent.
	ticketToken := mintTestServiceTokenForUser(t, machine, requester.UserID, "ticket",
		[]servicetoken.Grant{{Actions: []string{ticket.ActionAll}}})
	ticketClient := service.NewServiceClientFromToken(ticketSvc.SocketPath, ticketToken)

	opsRoomID := setupReservationOpsRoom(t, admin, machine, ticketSvc, fc, fleet)
	t.Logf("ops room ready: %s", opsRoomID)

	// --- Subtests ---

	t.Run("GrantFlow", func(t *testing.T) {
		wsRoomID := createReservationWorkspaceRoom(t, admin, fleet, ticketSvc, "grant-flow")

		// Set watches before triggering actions.
		opsWatch := watchRoom(t, admin, opsRoomID)
		wsWatch := watchRoom(t, admin, wsRoomID)

		// Create a resource_request ticket (exclusive, P2, 10m duration).
		ticketID := publishResourceRequest(t, ticketClient, wsRoomID, machine.Ref.Name(), schema.ModeExclusive, 2, "10m")

		// --- Verify ops room events ---

		// Relay ticket appears.
		relayTicketID, relayContent := waitForRelayTicket(t, &opsWatch, machine.Ref.Name())
		if relayContent.Priority != 2 {
			t.Errorf("relay ticket priority = %d, want 2", relayContent.Priority)
		}
		if relayContent.Reservation == nil || len(relayContent.Reservation.Claims) == 0 {
			t.Fatal("relay ticket has no reservation claims")
		}
		if relayContent.Reservation.Claims[0].Resource.Target != machine.Ref.Name() {
			t.Errorf("relay ticket resource target = %q, want %q",
				relayContent.Reservation.Claims[0].Resource.Target, machine.Ref.Name())
		}

		// Fleet controller sets relay ticket to in_progress.
		waitForTicketStatus(t, &opsWatch, relayTicketID, ticket.StatusInProgress)

		// Machine drain published (exclusive mode).
		drain := waitForMachineDrain(t, &opsWatch)
		if drain.ReservationHolder.IsZero() {
			t.Error("drain has empty reservation holder")
		}

		// Reservation grant published.
		holderLocalpart := requester.UserID.Localpart()
		grant := waitForReservationGrant(t, &opsWatch, holderLocalpart)
		if grant.Holder.String() != requester.UserID.Localpart() {
			t.Errorf("grant holder = %q, want %q", grant.Holder, requester.UserID.Localpart())
		}
		if grant.Mode != schema.ModeExclusive {
			t.Errorf("grant mode = %q, want %q", grant.Mode, schema.ModeExclusive)
		}
		if grant.Resource.Type != schema.ResourceMachine {
			t.Errorf("grant resource type = %q, want %q", grant.Resource.Type, schema.ResourceMachine)
		}
		if grant.Resource.Target != machine.Ref.Name() {
			t.Errorf("grant resource target = %q, want %q", grant.Resource.Target, machine.Ref.Name())
		}
		if grant.ExpiresAt == "" {
			t.Error("grant has empty expires_at")
		}

		// --- Verify workspace room events ---

		// Workspace ticket claim status reaches "granted".
		wsContent := waitForClaimStatus(t, &wsWatch, ticketID, schema.ClaimGranted)
		if wsContent.Reservation == nil || len(wsContent.Reservation.Claims) == 0 {
			t.Fatal("workspace ticket lost reservation claims")
		}

		// --- Cleanup: close workspace ticket to release reservation ---
		cleanupOpsWatch := watchRoom(t, admin, opsRoomID)
		closeWorkspaceTicket(t, ticketClient, wsRoomID, ticketID, "test complete")
		waitForReservationCleared(t, &cleanupOpsWatch, holderLocalpart)
		t.Log("grant flow cleanup complete")
	})

	t.Run("Preemption", func(t *testing.T) {
		// Two workspace rooms: one for P3, one for P1.
		wsRoomP3 := createReservationWorkspaceRoom(t, admin, fleet, ticketSvc, "preempt-low")
		wsRoomP1 := createReservationWorkspaceRoom(t, admin, fleet, ticketSvc, "preempt-high")

		holderLocalpart := requester.UserID.Localpart()

		// --- Phase 1: Grant P3 request ---
		opsWatch := watchRoom(t, admin, opsRoomID)
		wsWatchP3 := watchRoom(t, admin, wsRoomP3)

		ticketIDP3 := publishResourceRequest(t, ticketClient, wsRoomP3, machine.Ref.Name(), schema.ModeExclusive, 3, "10m")

		relayTicketIDP3, _ := waitForRelayTicket(t, &opsWatch, machine.Ref.Name())
		waitForReservationGrant(t, &opsWatch, holderLocalpart)
		waitForClaimStatus(t, &wsWatchP3, ticketIDP3, schema.ClaimGranted)
		t.Log("P3 reservation granted")

		// --- Phase 2: Submit P1 request, expect preemption ---
		opsWatch2 := watchRoom(t, admin, opsRoomID)
		wsWatchP3b := watchRoom(t, admin, wsRoomP3)
		wsWatchP1 := watchRoom(t, admin, wsRoomP1)

		ticketIDP1 := publishResourceRequest(t, ticketClient, wsRoomP1, machine.Ref.Name(), schema.ModeExclusive, 1, "10m")

		// P3 relay ticket closed with preemption reason.
		p3Closed := waitForTicketStatus(t, &opsWatch2, relayTicketIDP3, ticket.StatusClosed)
		if p3Closed.CloseReason == "" {
			t.Error("P3 relay ticket closed without reason")
		}
		t.Logf("P3 relay ticket closed: %s", p3Closed.CloseReason)

		// P3 reservation cleared, P1 reservation granted.
		waitForReservationCleared(t, &opsWatch2, holderLocalpart)
		waitForReservationGrant(t, &opsWatch2, holderLocalpart)

		// P3 workspace claim reaches "preempted".
		waitForClaimStatus(t, &wsWatchP3b, ticketIDP3, schema.ClaimPreempted)

		// P1 workspace claim reaches "granted".
		waitForClaimStatus(t, &wsWatchP1, ticketIDP1, schema.ClaimGranted)
		t.Log("preemption verified")

		// --- Cleanup ---
		cleanupOpsWatch := watchRoom(t, admin, opsRoomID)
		closeWorkspaceTicket(t, ticketClient, wsRoomP1, ticketIDP1, "test complete")
		waitForReservationCleared(t, &cleanupOpsWatch, holderLocalpart)
	})

	t.Run("DurationExpiry", func(t *testing.T) {
		wsRoomID := createReservationWorkspaceRoom(t, admin, fleet, ticketSvc, "expiry-test")

		opsWatch := watchRoom(t, admin, opsRoomID)

		// Use a long MaxDuration so the reservation never expires under
		// real wall clock time. The fleet controller's test clock is
		// frozen; we advance it past the expiry point after the grant
		// is confirmed.
		holderLocalpart := requester.UserID.Localpart()
		publishResourceRequest(t, ticketClient, wsRoomID, machine.Ref.Name(), schema.ModeExclusive, 2, "1h")

		// Wait for grant.
		relayTicketID, _ := waitForRelayTicket(t, &opsWatch, machine.Ref.Name())
		waitForReservationGrant(t, &opsWatch, holderLocalpart)
		t.Log("reservation granted, advancing test clock past expiry")

		// Advance the fleet controller's fake clock past the 1-hour
		// reservation duration. This is deterministic — no wall clock
		// waiting.
		advanceFleetClock(t, fc, 2*time.Hour)

		// Send a timeline message to trigger a /sync wake-up. The fleet
		// controller calls checkReservationExpiry after each sync cycle;
		// with the clock now past the expiry, it will close the relay
		// ticket.
		opsWatch2 := watchRoom(t, admin, opsRoomID)
		if _, err := admin.SendMessage(t.Context(), opsRoomID, messaging.NewTextMessage("expiry check trigger")); err != nil {
			t.Fatalf("send wake-up message: %v", err)
		}

		// Relay ticket closed with duration exceeded.
		expiredContent := waitForTicketStatus(t, &opsWatch2, relayTicketID, ticket.StatusClosed)
		if expiredContent.CloseReason == "" {
			t.Error("expired relay ticket has empty close reason")
		}
		t.Logf("relay ticket closed: %s", expiredContent.CloseReason)

		// Reservation and drain cleared.
		waitForReservationCleared(t, &opsWatch2, holderLocalpart)
		waitForDrainCleared(t, &opsWatch2)
		t.Log("duration expiry verified")
	})

	t.Run("QueueAndRelease", func(t *testing.T) {
		wsRoomA := createReservationWorkspaceRoom(t, admin, fleet, ticketSvc, "queue-a")
		wsRoomB := createReservationWorkspaceRoom(t, admin, fleet, ticketSvc, "queue-b")

		holderLocalpart := requester.UserID.Localpart()

		// --- Phase 1: Grant ticket A ---
		opsWatch := watchRoom(t, admin, opsRoomID)
		wsWatchA := watchRoom(t, admin, wsRoomA)

		ticketIDA := publishResourceRequest(t, ticketClient, wsRoomA, machine.Ref.Name(), schema.ModeExclusive, 2, "10m")
		waitForRelayTicket(t, &opsWatch, machine.Ref.Name())
		waitForReservationGrant(t, &opsWatch, holderLocalpart)
		waitForClaimStatus(t, &wsWatchA, ticketIDA, schema.ClaimGranted)
		t.Log("ticket A granted")

		// --- Phase 2: Submit ticket B (same priority, gets queued) ---
		opsWatch2 := watchRoom(t, admin, opsRoomID)
		ticketIDB := publishResourceRequest(t, ticketClient, wsRoomB, machine.Ref.Name(), schema.ModeExclusive, 2, "10m")

		// Wait for ticket B relay ticket to appear (confirms it's enqueued
		// by the fleet controller — the relay ticket will remain open since
		// the machine is already reserved).
		waitForRelayTicket(t, &opsWatch2, machine.Ref.Name())
		t.Log("ticket B relay ticket appeared (queued)")

		// --- Phase 3: Close ticket A → B should advance ---
		opsWatch3 := watchRoom(t, admin, opsRoomID)
		wsWatchB := watchRoom(t, admin, wsRoomB)

		closeWorkspaceTicket(t, ticketClient, wsRoomA, ticketIDA, "releasing resource")

		// Ticket A reservation cleared.
		waitForReservationCleared(t, &opsWatch3, holderLocalpart)

		// Ticket B reservation granted (queue advanced).
		waitForReservationGrant(t, &opsWatch3, holderLocalpart)

		// Ticket B workspace claim reaches "granted".
		waitForClaimStatus(t, &wsWatchB, ticketIDB, schema.ClaimGranted)
		t.Log("queue advancement verified")

		// --- Cleanup ---
		cleanupOpsWatch := watchRoom(t, admin, opsRoomID)
		closeWorkspaceTicket(t, ticketClient, wsRoomB, ticketIDB, "test complete")
		waitForReservationCleared(t, &cleanupOpsWatch, holderLocalpart)
	})

	t.Run("PipelineWithReservation", func(t *testing.T) {
		// A pipeline ticket with a Reservation exercises the full relay
		// lifecycle: the ticket service relays the pipeline ticket's
		// reservation claims to the ops room, the fleet controller grants
		// the reservation, and the cross-room gate fires back on the
		// workspace ticket. Closing the pipeline ticket cascades relay
		// ticket closure and reservation release — identical to a
		// resource_request ticket but with type=pipeline.
		wsRoomID := createPipelineWorkspaceRoom(t, admin, fleet, ticketSvc, "pip-rsv")

		opsWatch := watchRoom(t, admin, opsRoomID)
		wsWatch := watchRoom(t, admin, wsRoomID)

		// Create a pipeline ticket with an exclusive machine reservation.
		ticketID := publishPipelineWithReservation(t, ticketClient, wsRoomID,
			"gpu-training", machine.Ref.Name(), schema.ModeExclusive, 2, "10m")

		// Relay ticket appears in ops room. The relay mechanism always
		// creates relay tickets as type=resource_request regardless of
		// the workspace ticket type (prepareRelayTicket hardcodes this).
		relayTicketID, relayContent := waitForRelayTicket(t, &opsWatch, machine.Ref.Name())
		if relayContent.Reservation == nil || len(relayContent.Reservation.Claims) == 0 {
			t.Fatal("relay ticket has no reservation claims")
		}
		if relayContent.Reservation.Claims[0].Resource.Target != machine.Ref.Name() {
			t.Errorf("relay claim target = %q, want %q",
				relayContent.Reservation.Claims[0].Resource.Target, machine.Ref.Name())
		}

		// Fleet controller grants the reservation.
		holderLocalpart := requester.UserID.Localpart()
		waitForTicketStatus(t, &opsWatch, relayTicketID, ticket.StatusInProgress)
		waitForReservationGrant(t, &opsWatch, holderLocalpart)

		// Workspace ticket claim reaches "granted" and reservation gate
		// is satisfied by the cross-room gate evaluation path.
		wsContent := waitForClaimStatus(t, &wsWatch, ticketID, schema.ClaimGranted)
		if wsContent.Pipeline == nil {
			t.Fatal("pipeline content lost after reservation grant")
		}
		if wsContent.Pipeline.PipelineRef != "gpu-training" {
			t.Errorf("pipeline_ref = %q, want %q", wsContent.Pipeline.PipelineRef, "gpu-training")
		}

		// Verify all gates are satisfied (the reservation gate was added
		// by relay and fired on the FC's reservation grant).
		for gateIndex, gate := range wsContent.Gates {
			if gate.Status != ticket.GateSatisfied {
				t.Errorf("gate[%d] (%s) status = %s, want satisfied",
					gateIndex, gate.Type, gate.Status)
			}
		}
		t.Log("pipeline ticket gates all satisfied after reservation grant")

		// Close the pipeline ticket (simulates executor completion).
		// Cascade: relay ticket closes → reservation released.
		cleanupOpsWatch := watchRoom(t, admin, opsRoomID)
		closeWorkspaceTicket(t, ticketClient, wsRoomID, ticketID, "pipeline complete")
		waitForReservationCleared(t, &cleanupOpsWatch, holderLocalpart)
		t.Log("pipeline reservation released on ticket close")
	})

	t.Run("PipelineCancelledOnPreemption", func(t *testing.T) {
		// When a higher-priority request preempts a pipeline ticket's
		// reservation, the pipeline ticket must be closed automatically
		// so the executor detects cancellation via its polling loop.
		// This exercises the close-on-preemption path added in
		// mirrorRelayTicketStatus.
		wsRoomPipeline := createPipelineWorkspaceRoom(t, admin, fleet, ticketSvc, "pip-preempt")
		wsRoomHighPri := createReservationWorkspaceRoom(t, admin, fleet, ticketSvc, "preempt-pip-high")

		holderLocalpart := requester.UserID.Localpart()

		// --- Phase 1: Grant pipeline reservation ---
		opsWatch := watchRoom(t, admin, opsRoomID)
		wsWatchPip := watchRoom(t, admin, wsRoomPipeline)

		pipTicketID := publishPipelineWithReservation(t, ticketClient, wsRoomPipeline,
			"preemptable-job", machine.Ref.Name(), schema.ModeExclusive, 3, "10m")

		waitForRelayTicket(t, &opsWatch, machine.Ref.Name())
		waitForReservationGrant(t, &opsWatch, holderLocalpart)
		waitForClaimStatus(t, &wsWatchPip, pipTicketID, schema.ClaimGranted)
		t.Log("pipeline reservation granted (P3)")

		// --- Phase 2: Submit P1 request → pipeline preempted ---
		opsWatch2 := watchRoom(t, admin, opsRoomID)
		wsWatchPip2 := watchRoom(t, admin, wsRoomPipeline)
		wsWatchHigh := watchRoom(t, admin, wsRoomHighPri)

		highTicketID := publishResourceRequest(t, ticketClient, wsRoomHighPri,
			machine.Ref.Name(), schema.ModeExclusive, 1, "10m")

		// Pipeline ticket claim reaches "preempted".
		preemptedContent := waitForClaimStatus(t, &wsWatchPip2, pipTicketID, schema.ClaimPreempted)
		if preemptedContent.Pipeline == nil {
			t.Fatal("pipeline content lost after preemption")
		}

		// Pipeline ticket is closed by the close-on-preemption path.
		closedContent := waitForTicketStatus(t, &wsWatchPip2, pipTicketID, ticket.StatusClosed)
		if closedContent.CloseReason == "" {
			t.Error("preempted pipeline ticket has empty close reason")
		}
		if !containsSubstring(closedContent.CloseReason, "preempted") {
			t.Errorf("close reason %q does not contain 'preempted'", closedContent.CloseReason)
		}
		t.Logf("pipeline ticket closed on preemption: %s", closedContent.CloseReason)

		// High-priority request gets the reservation.
		waitForReservationGrant(t, &opsWatch2, holderLocalpart)
		waitForClaimStatus(t, &wsWatchHigh, highTicketID, schema.ClaimGranted)
		t.Log("high-priority request granted after preemption")

		// --- Cleanup ---
		cleanupOpsWatch := watchRoom(t, admin, opsRoomID)
		closeWorkspaceTicket(t, ticketClient, wsRoomHighPri, highTicketID, "test complete")
		waitForReservationCleared(t, &cleanupOpsWatch, holderLocalpart)
	})

	t.Run("DenialCascade", func(t *testing.T) {
		// A ticket with two claims targeting different machines. When
		// one claim is denied (the resource owner closes the relay
		// ticket without granting), the denial cascade closes the other
		// relay ticket and the workspace ticket.
		//
		// Both machines use FC-free ops rooms. The fleet controller
		// must NOT watch these rooms: if it grants either claim before
		// the admin denies, handleRelayTicketClosed treats the closure
		// as preemption instead of denial, and the cascade never fires.
		client := adminClient(t)
		hsAdmin := homeserverAdmin(t)

		denyMachineA := newTestMachine(t, fleet, "rsv-deny-a")
		provisionMachine(t, client, admin, hsAdmin, denyMachineA.Ref,
			filepath.Join(denyMachineA.StateDir, "bootstrap.json"))
		opsRoomA := setupOpsRoomNoFC(t, admin, denyMachineA, ticketSvc, fleet)

		denyMachineB := newTestMachine(t, fleet, "rsv-deny-b")
		provisionMachine(t, client, admin, hsAdmin, denyMachineB.Ref,
			filepath.Join(denyMachineB.StateDir, "bootstrap.json"))
		opsRoomB := setupOpsRoomNoFC(t, admin, denyMachineB, ticketSvc, fleet)

		t.Logf("FC-free ops rooms: %s (A), %s (B)", opsRoomA, opsRoomB)

		wsRoomID := createReservationWorkspaceRoom(t, admin, fleet, ticketSvc, "denial-cascade")

		opsWatchA := watchRoom(t, admin, opsRoomA)
		opsWatchB := watchRoom(t, admin, opsRoomB)

		// Create a ticket claiming both machines.
		ticketID := publishMultiClaimResourceRequest(t, ticketClient, wsRoomID,
			[]string{denyMachineA.Ref.Name(), denyMachineB.Ref.Name()}, 2, "10m")

		// Relay tickets appear in both ops rooms.
		relayTicketIDA, _ := waitForRelayTicket(t, &opsWatchA, denyMachineA.Ref.Name())
		relayTicketIDB, _ := waitForRelayTicket(t, &opsWatchB, denyMachineB.Ref.Name())
		t.Logf("relay tickets: %s (A), %s (B)", relayTicketIDA, relayTicketIDB)

		// Deny claim B by closing its relay ticket directly. The admin
		// has PL 100 and can write ticket state events. The ticket
		// service sees this closure via /sync and triggers
		// handleRelayTicketClosed → cascadeDenial (because the claim
		// was never granted).
		denialWatchA := watchRoom(t, admin, opsRoomA)
		denialWatchWS := watchRoom(t, admin, wsRoomID)

		denyContent := ticket.TicketContent{
			Version:     1,
			Title:       "denied",
			Type:        ticket.TypeResourceRequest,
			Status:      ticket.StatusClosed,
			CloseReason: "resource unavailable",
			ClosedAt:    "2026-01-01T00:00:00Z",
			UpdatedAt:   "2026-01-01T00:00:00Z",
		}
		if _, err := admin.SendStateEvent(t.Context(), opsRoomB,
			schema.EventTypeTicket, relayTicketIDB, denyContent); err != nil {
			t.Fatalf("deny relay ticket in ops room B: %v", err)
		}
		t.Log("denied relay ticket in ops room B")

		// Denial cascade: relay ticket in ops room A is closed.
		cascadedContent := waitForTicketStatus(t, &denialWatchA, relayTicketIDA, ticket.StatusClosed)
		if !containsSubstring(cascadedContent.CloseReason, "denial cascade") {
			t.Errorf("cascaded relay close reason = %q, want containing 'denial cascade'",
				cascadedContent.CloseReason)
		}
		t.Logf("cascaded relay ticket closed: %s", cascadedContent.CloseReason)

		// Workspace ticket is closed with denial reason.
		wsClosedContent := waitForTicketStatus(t, &denialWatchWS, ticketID, ticket.StatusClosed)
		if !containsSubstring(wsClosedContent.CloseReason, "reservation denied") {
			t.Errorf("workspace close reason = %q, want containing 'reservation denied'",
				wsClosedContent.CloseReason)
		}
		t.Logf("workspace ticket closed: %s", wsClosedContent.CloseReason)
	})

	t.Run("OutboundFilter", func(t *testing.T) {
		// An ops room with an OutboundFilter restricting what crosses
		// the relay boundary. When the admin denies a relay ticket
		// with a detailed close reason, the origin ticket must receive
		// only the generic "reservation denied" — the ops room's
		// internal detail must not leak back to the requesting room.
		//
		// This tests the filtering path: handleRelayTicketClosed →
		// outboundFilterForClaim → filter.Allows("close_reason") →
		// cascadeDenial with empty reason.
		client := adminClient(t)
		hsAdmin := homeserverAdmin(t)

		filterMachine := newTestMachine(t, fleet, "rsv-filter")
		provisionMachine(t, client, admin, hsAdmin, filterMachine.Ref,
			filepath.Join(filterMachine.StateDir, "bootstrap.json"))

		// Set up the ops room without FC (claims stay pending, denial
		// path is exercised). Then overwrite the relay policy with an
		// OutboundFilter that only permits "status" to cross the
		// boundary — close_reason and status_reason are excluded.
		opsRoomID := setupOpsRoomNoFC(t, admin, filterMachine, ticketSvc, fleet)

		filteredPolicy := schema.RelayPolicy{
			Sources: []schema.RelaySource{{
				Match: schema.RelayMatchFleetMember,
				Fleet: fleet.Ref.Localpart(),
			}},
			AllowedTypes: []string{
				string(ticket.TypeResourceRequest),
				string(ticket.TypePipeline),
			},
			OutboundFilter: &schema.RelayFilter{
				Include: []string{"status"},
			},
		}
		if _, err := admin.SendStateEvent(t.Context(), opsRoomID,
			schema.EventTypeRelayPolicy, "", filteredPolicy); err != nil {
			t.Fatalf("publish filtered relay policy: %v", err)
		}
		t.Log("published relay policy with OutboundFilter restricting to status only")

		wsRoomID := createReservationWorkspaceRoom(t, admin, fleet, ticketSvc, "outbound-filter")

		opsWatch := watchRoom(t, admin, opsRoomID)
		wsWatch := watchRoom(t, admin, wsRoomID)

		ticketID := publishResourceRequest(t, ticketClient, wsRoomID,
			filterMachine.Ref.Name(), schema.ModeExclusive, 2, "10m")

		// Relay ticket appears in ops room.
		relayTicketID, relayContent := waitForRelayTicket(t, &opsWatch, filterMachine.Ref.Name())
		t.Logf("relay ticket appeared: %s", relayTicketID)

		// Verify the relay ticket body describes the resource, not
		// the origin room (origin room ID must not leak into the
		// ops room).
		expectedBody := fmt.Sprintf("Relay for %s/%s (%s)",
			schema.ResourceMachine, filterMachine.Ref.Name(), schema.ModeExclusive)
		if relayContent.Body != expectedBody {
			t.Errorf("relay ticket body = %q, want %q", relayContent.Body, expectedBody)
		}

		// Admin denies the relay ticket with a detailed internal
		// reason. Without the OutboundFilter, this reason would
		// propagate verbatim to the origin ticket.
		denyContent := ticket.TicketContent{
			Version:     1,
			Title:       "denied",
			Type:        ticket.TypeResourceRequest,
			Status:      ticket.StatusClosed,
			CloseReason: "internal maintenance window: GPU firmware update scheduled",
			ClosedAt:    "2026-01-01T00:00:00Z",
			UpdatedAt:   "2026-01-01T00:00:00Z",
		}
		if _, err := admin.SendStateEvent(t.Context(), opsRoomID,
			schema.EventTypeTicket, relayTicketID, denyContent); err != nil {
			t.Fatalf("deny relay ticket: %v", err)
		}
		t.Log("denied relay ticket with detailed internal reason")

		// Origin ticket is closed. The OutboundFilter must suppress
		// the ops room's detail — only the generic denial reason
		// should appear.
		wsContent := waitForTicketStatus(t, &wsWatch, ticketID, ticket.StatusClosed)
		if wsContent.CloseReason != "reservation denied" {
			t.Errorf("origin close reason = %q, want exactly %q "+
				"(OutboundFilter should suppress ops room detail)",
				wsContent.CloseReason, "reservation denied")
		}

		// Verify no ops room detail leaked through the filter.
		if containsSubstring(wsContent.CloseReason, "maintenance") ||
			containsSubstring(wsContent.CloseReason, "GPU") ||
			containsSubstring(wsContent.CloseReason, "firmware") {
			t.Error("ops room internal detail leaked through OutboundFilter into origin ticket")
		}

		t.Logf("origin ticket closed: %q (OutboundFilter correctly suppressed detail)", wsContent.CloseReason)
	})

	t.Run("RelayAuthRejection", func(t *testing.T) {
		// A resource_request created in a room without a fleet-scoped
		// alias cannot be relayed: the ticket service cannot determine
		// which fleet to relay into. The ticket gets a failure note
		// explaining why relay failed.
		ctx := t.Context()

		// Create a workspace room with NO fleet-scoped alias. The
		// ticket service reads the canonical alias to extract fleet
		// identity; without /fleet/ in the alias, relay fails.
		room, err := admin.CreateRoom(ctx, messaging.CreateRoomRequest{
			Name:  "Workspace: unscoped",
			Alias: "workspace-no-fleet-scope",
		})
		if err != nil {
			t.Fatalf("create non-fleet workspace room: %v", err)
		}

		// Enable tickets in this room (required for the create API).
		enableTicketsInRoom(t, admin, room.RoomID, ticketSvc, "rsv",
			[]ticket.TicketType{ticket.TypeResourceRequest})
		wsWatch := watchRoom(t, admin, room.RoomID)
		wsWatch.WaitForEvent(t, func(event messaging.Event) bool {
			return event.Type == schema.EventTypeServiceReady &&
				event.Sender == ticketSvc.Account.UserID
		}, "ticket service ready in non-fleet room")

		// Watch for ticket updates before creating.
		noteWatch := watchRoom(t, admin, room.RoomID)

		// Create a resource_request targeting the existing machine.
		// Relay initiation will fail because the workspace room has
		// no fleet-scoped alias.
		ticketID := publishResourceRequest(t, ticketClient, room.RoomID,
			machine.Ref.Name(), schema.ModeExclusive, 2, "10m")

		// The ticket gets a failure note explaining the relay error.
		noteContent := waitForTicketNote(t, &noteWatch, ticketID, "relay failed")
		foundNote := false
		for _, note := range noteContent.Notes {
			if containsSubstring(note.Body, "relay failed") {
				t.Logf("relay failure note: %s", note.Body)
				foundNote = true
				break
			}
		}
		if !foundNote {
			t.Error("expected failure note on ticket but none found")
		}
	})
}
