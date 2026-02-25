// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/stewardship"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/lib/testutil"
	"github.com/bureau-foundation/bureau/messaging"
)

// TestStewardshipReview exercises the full stewardship review lifecycle
// through the production stack: stewardship declaration publishing,
// review gate auto-configuration, multi-reviewer disposition setting,
// threshold-based gate satisfaction, tiered escalation, notifications,
// cross-room stewardship resolution, and agent-mediated review via MCP.
//
// All reviewer operations use direct service socket calls (minted
// service tokens) for speed. One subtest deploys a real agent with a
// mock LLM to prove the MCP boundary works for stewardship-gated
// ticket creation.
//
// Setup: one machine, one ticket service, three steward accounts joined
// to shared rooms. Multiple stewardship declarations (different state
// keys, different resource patterns) in the same room allow each
// subtest to target specific declarations via `affects` without
// creating per-subtest rooms.
func TestStewardshipReview(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	admin := adminSession(t)
	defer admin.Close()

	fleet := createTestFleet(t, admin)

	// Boot a machine.
	machine := newTestMachine(t, fleet, "stw-review")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	})

	// Deploy ticket service.
	ticketSvc := deployTicketService(t, admin, fleet, machine, "stw-review")

	// --- Steward accounts ---
	//
	// Three steward accounts with bare localparts. Each gets a
	// temporary session for room joins and a minted service token
	// for set-disposition calls. The localpart prefix includes a
	// random suffix so parallel tests don't collide on the shared
	// homeserver.
	var randomBytes [4]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		t.Fatalf("generate random prefix: %v", err)
	}
	testPrefix := hex.EncodeToString(randomBytes[:])

	alphaLocalpart := "steward-alpha-" + testPrefix
	betaLocalpart := "steward-beta-" + testPrefix
	gammaLocalpart := "steward-gamma-" + testPrefix

	alphaAccount := registerPrincipal(t, alphaLocalpart, "alpha-pw")
	betaAccount := registerPrincipal(t, betaLocalpart, "beta-pw")
	gammaAccount := registerPrincipal(t, gammaLocalpart, "gamma-pw")

	// Sessions for room joins (closed after setup).
	alphaSession := principalSession(t, alphaAccount)
	betaSession := principalSession(t, betaAccount)
	gammaSession := principalSession(t, gammaAccount)

	// Stewardship principal patterns.
	alphaPattern := alphaLocalpart + ":" + testServerName
	allStewardsPattern := "steward-*-" + testPrefix + ":" + testServerName

	// Service tokens for set-disposition (keyed by UserID → subject).
	alphaToken := mintTestServiceTokenForUser(t, machine, alphaAccount.UserID, "ticket",
		[]servicetoken.Grant{{Actions: []string{ticket.ActionAll}}})
	betaToken := mintTestServiceTokenForUser(t, machine, betaAccount.UserID, "ticket",
		[]servicetoken.Grant{{Actions: []string{ticket.ActionAll}}})

	alphaClient := service.NewServiceClientFromToken(ticketSvc.SocketPath, alphaToken)
	betaClient := service.NewServiceClientFromToken(ticketSvc.SocketPath, betaToken)

	// Operator token for ticket creation (non-reviewer entity).
	operatorEntity, err := ref.NewEntityFromAccountLocalpart(fleet.Ref, "agent/stw-operator")
	if err != nil {
		t.Fatalf("construct operator entity: %v", err)
	}
	operatorToken := mintTestServiceToken(t, machine, operatorEntity, "ticket",
		[]servicetoken.Grant{{Actions: []string{ticket.ActionAll}}})
	operatorClient := service.NewServiceClientFromToken(ticketSvc.SocketPath, operatorToken)

	adminUserID := admin.UserID()

	// --- Room setup ---
	//
	// All rooms are created and steward accounts joined before any
	// subtests start, so steward sessions can be closed early.

	// joinStewards invites and joins the given steward accounts to a room.
	type stewardJoin struct {
		account principalAccount
		session *messaging.DirectSession
	}
	joinStewards := func(t *testing.T, roomID ref.RoomID, stewards []stewardJoin) {
		t.Helper()
		for _, steward := range stewards {
			if err := admin.InviteUser(ctx, roomID, steward.account.UserID); err != nil {
				if !messaging.IsMatrixError(err, messaging.ErrCodeForbidden) {
					t.Fatalf("invite %s: %v", steward.account.Localpart, err)
				}
			}
			if _, err := steward.session.JoinRoom(ctx, roomID); err != nil {
				t.Fatalf("%s join room: %v", steward.account.Localpart, err)
			}
		}
	}

	allStewards := []stewardJoin{
		{alphaAccount, alphaSession},
		{betaAccount, betaSession},
		{gammaAccount, gammaSession},
	}

	// --- Main review room ---
	//
	// Multiple stewardship declarations (different state keys and
	// resource patterns) in one room. Each subtest creates tickets
	// with `affects` targeting the appropriate declaration.

	tierThreshold1 := 1
	tierThreshold2 := 2

	mainRoomID, err := admin.CreateRoom(ctx, messaging.CreateRoomRequest{
		Name:   "stw-review-main",
		Invite: []string{machine.UserID.String()},
	})
	if err != nil {
		t.Fatalf("create main room: %v", err)
	}

	joinStewards(t, mainRoomID.RoomID, allStewards)

	// Single-reviewer declaration: alpha only, threshold 1.
	if _, err := admin.SendStateEvent(ctx, mainRoomID.RoomID,
		schema.EventTypeStewardship, "single",
		stewardship.StewardshipContent{
			Version:          1,
			ResourcePatterns: []string{"infra/single/**"},
			GateTypes:        []ticket.TicketType{ticket.TypeTask},
			Tiers: []stewardship.StewardshipTier{{
				Principals: []string{alphaPattern},
				Threshold:  &tierThreshold1,
			}},
		}); err != nil {
		t.Fatalf("publish single-reviewer stewardship: %v", err)
	}

	// Multi-reviewer declaration: all three stewards, threshold 2.
	if _, err := admin.SendStateEvent(ctx, mainRoomID.RoomID,
		schema.EventTypeStewardship, "multi",
		stewardship.StewardshipContent{
			Version:          1,
			ResourcePatterns: []string{"infra/multi/**"},
			GateTypes:        []ticket.TicketType{ticket.TypeTask},
			Tiers: []stewardship.StewardshipTier{{
				Principals: []string{allStewardsPattern},
				Threshold:  &tierThreshold2,
			}},
		}); err != nil {
		t.Fatalf("publish multi-reviewer stewardship: %v", err)
	}

	// Tiered declaration: tier 0 (alpha, threshold 1), tier 1 (beta, threshold 1).
	betaPattern := betaLocalpart + ":" + testServerName
	if _, err := admin.SendStateEvent(ctx, mainRoomID.RoomID,
		schema.EventTypeStewardship, "tiered",
		stewardship.StewardshipContent{
			Version:          1,
			ResourcePatterns: []string{"infra/tiered/**"},
			GateTypes:        []ticket.TicketType{ticket.TypeTask},
			Tiers: []stewardship.StewardshipTier{
				{
					Principals: []string{alphaPattern},
					Threshold:  &tierThreshold1,
				},
				{
					Principals: []string{betaPattern},
					Threshold:  &tierThreshold1,
				},
			},
		}); err != nil {
		t.Fatalf("publish tiered stewardship: %v", err)
	}

	// Notification-only declaration: NotifyTypes (no GateTypes).
	if _, err := admin.SendStateEvent(ctx, mainRoomID.RoomID,
		schema.EventTypeStewardship, "notify",
		stewardship.StewardshipContent{
			Version:          1,
			ResourcePatterns: []string{"infra/notify/**"},
			NotifyTypes:      []ticket.TicketType{ticket.TypeBug},
			Tiers: []stewardship.StewardshipTier{{
				Principals: []string{allStewardsPattern},
				Threshold:  &tierThreshold1,
			}},
		}); err != nil {
		t.Fatalf("publish notify stewardship: %v", err)
	}

	// Enable tickets — service joins, processes all stewardship
	// declarations and room membership, announces readiness.
	mainRoomWatch := watchRoom(t, admin, mainRoomID.RoomID)
	enableTicketsInRoom(t, admin, mainRoomID.RoomID, ticketSvc, "stw", nil)
	mainRoomWatch.WaitForEvent(t, func(event messaging.Event) bool {
		return event.Type == schema.EventTypeServiceReady &&
			event.Sender == ticketSvc.Account.UserID
	}, "ticket service ready in main room")

	// --- Cross-room setup ---
	//
	// Declaration room has stewardship + stewards. Ticket room has
	// tickets enabled but no stewardship. Tickets in the ticket room
	// with matching affects should resolve stewardship from the
	// declaration room's index.

	crossDeclRoomID, err := admin.CreateRoom(ctx, messaging.CreateRoomRequest{
		Name:   "stw-cross-decl",
		Invite: []string{machine.UserID.String()},
	})
	if err != nil {
		t.Fatalf("create cross-decl room: %v", err)
	}

	joinStewards(t, crossDeclRoomID.RoomID, allStewards)

	if _, err := admin.SendStateEvent(ctx, crossDeclRoomID.RoomID,
		schema.EventTypeStewardship, "cross",
		stewardship.StewardshipContent{
			Version:          1,
			ResourcePatterns: []string{"platform/**"},
			GateTypes:        []ticket.TicketType{ticket.TypeTask},
			Tiers: []stewardship.StewardshipTier{{
				Principals: []string{allStewardsPattern},
				Threshold:  &tierThreshold1,
			}},
		}); err != nil {
		t.Fatalf("publish cross-room stewardship: %v", err)
	}

	// Enable tickets in the declaration room so the service joins
	// and indexes the stewardship declaration.
	crossDeclWatch := watchRoom(t, admin, crossDeclRoomID.RoomID)
	enableTicketsInRoom(t, admin, crossDeclRoomID.RoomID, ticketSvc, "xd", nil)
	crossDeclWatch.WaitForEvent(t, func(event messaging.Event) bool {
		return event.Type == schema.EventTypeServiceReady &&
			event.Sender == ticketSvc.Account.UserID
	}, "ticket service ready in cross-decl room")

	// Ticket room: tickets enabled, no stewardship declaration.
	crossTicketRoomID := createTicketProjectRoom(t, admin, "stw-cross-ticket",
		ticketSvc.Entity, machine.UserID.String())

	// --- Agent room ---
	//
	// Stewardship declaration matching admin (who creates the room).
	// The agent creates a ticket with matching affects via MCP tool;
	// the test verifies stewardship gates are auto-configured.
	agentRoomID, err := admin.CreateRoom(ctx, messaging.CreateRoomRequest{
		Name:   "stw-agent-room",
		Invite: []string{machine.UserID.String()},
	})
	if err != nil {
		t.Fatalf("create agent room: %v", err)
	}

	adminPattern := "admin-*:" + testServerName
	if _, err := admin.SendStateEvent(ctx, agentRoomID.RoomID,
		schema.EventTypeStewardship, "agent-review",
		stewardship.StewardshipContent{
			Version:          1,
			ResourcePatterns: []string{"deploy/**"},
			GateTypes:        []ticket.TicketType{ticket.TypeTask},
			Tiers: []stewardship.StewardshipTier{{
				Principals: []string{adminPattern},
				Threshold:  &tierThreshold1,
			}},
		}); err != nil {
		t.Fatalf("publish agent-room stewardship: %v", err)
	}

	agentRoomWatch := watchRoom(t, admin, agentRoomID.RoomID)
	enableTicketsInRoom(t, admin, agentRoomID.RoomID, ticketSvc, "ag", nil)
	agentRoomWatch.WaitForEvent(t, func(event messaging.Event) bool {
		return event.Type == schema.EventTypeServiceReady &&
			event.Sender == ticketSvc.Account.UserID
	}, "ticket service ready in agent room")

	// Close steward sessions now that all rooms are joined.
	alphaSession.Close()
	betaSession.Close()
	gammaSession.Close()

	// --- Helpers ---

	// findStewardshipGate returns the first gate with a "stewardship:" ID prefix.
	findStewardshipGate := func(t *testing.T, content ticket.TicketContent) *ticket.TicketGate {
		t.Helper()
		for i := range content.Gates {
			if strings.HasPrefix(content.Gates[i].ID, "stewardship:") {
				return &content.Gates[i]
			}
		}
		return nil
	}

	// waitForGateSatisfied waits for a ticket state event where the
	// named gate has status "satisfied".
	waitForGateSatisfied := func(t *testing.T, watch *roomWatch, ticketID, gateID string) {
		t.Helper()
		watch.WaitForEvent(t, func(event messaging.Event) bool {
			if event.Type != schema.EventTypeTicket {
				return false
			}
			if event.StateKey == nil || *event.StateKey != ticketID {
				return false
			}
			gates, _ := event.Content["gates"].([]any)
			for _, gate := range gates {
				gateMap, _ := gate.(map[string]any)
				if gateMap["id"] == gateID && gateMap["status"] == "satisfied" {
					return true
				}
			}
			return false
		}, "gate "+gateID+" satisfied on "+ticketID)
	}

	// createTicket creates a ticket via the operator client and returns
	// its ID.
	createTicket := func(t *testing.T, roomID ref.RoomID, title, ticketType string, priority int, affects []string) string {
		t.Helper()
		params := map[string]any{
			"room":     roomID.String(),
			"title":    title,
			"type":     ticketType,
			"priority": priority,
		}
		if len(affects) > 0 {
			params["affects"] = affects
		}
		var result struct {
			ID string `json:"id"`
		}
		if err := operatorClient.Call(ctx, "create", params, &result); err != nil {
			t.Fatalf("create ticket %q: %v", title, err)
		}
		return result.ID
	}

	// transitionToReview moves a ticket to review status.
	transitionToReview := func(t *testing.T, roomID ref.RoomID, ticketID string) {
		t.Helper()
		if err := operatorClient.Call(ctx, "update", map[string]any{
			"room":   roomID.String(),
			"ticket": ticketID,
			"status": string(ticket.StatusReview),
		}, nil); err != nil {
			t.Fatalf("transition %s to review: %v", ticketID, err)
		}
	}

	// --- Subtests ---

	t.Run("SingleReviewerApproval", func(t *testing.T) {
		ticketID := createTicket(t, mainRoomID.RoomID,
			"Single reviewer test", "task", 2, []string{"infra/single/node-1"})

		content := readTicketState(t, admin, mainRoomID.RoomID, ticketID)

		gate := findStewardshipGate(t, content)
		if gate == nil {
			t.Fatalf("no stewardship gate; gates = %v", content.Gates)
		}
		if gate.Type != ticket.GateReview {
			t.Errorf("gate type = %q, want review", gate.Type)
		}
		if gate.Status != ticket.GatePending {
			t.Errorf("gate status = %q, want pending", gate.Status)
		}
		if content.Review == nil {
			t.Fatal("no review section")
		}

		// Verify alpha is the only stewardship reviewer.
		var foundAlpha bool
		for _, reviewer := range content.Review.Reviewers {
			if reviewer.UserID == alphaAccount.UserID {
				foundAlpha = true
				if reviewer.Disposition != ticket.DispositionPending {
					t.Errorf("alpha disposition = %q, want pending", reviewer.Disposition)
				}
			}
		}
		if !foundAlpha {
			t.Fatalf("alpha not in reviewers: %v", content.Review.Reviewers)
		}

		// Transition to review and approve.
		transitionToReview(t, mainRoomID.RoomID, ticketID)

		gateWatch := watchRoom(t, admin, mainRoomID.RoomID)
		if err := alphaClient.Call(ctx, "set-disposition", map[string]any{
			"room":        mainRoomID.RoomID.String(),
			"ticket":      ticketID,
			"disposition": "approved",
		}, nil); err != nil {
			t.Fatalf("alpha set-disposition: %v", err)
		}

		waitForGateSatisfied(t, &gateWatch, ticketID, gate.ID)

		content = readTicketState(t, admin, mainRoomID.RoomID, ticketID)
		for _, reviewer := range content.Review.Reviewers {
			if reviewer.UserID == alphaAccount.UserID {
				if reviewer.Disposition != ticket.DispositionApproved {
					t.Errorf("after approval: disposition = %q, want approved", reviewer.Disposition)
				}
			}
		}

		for _, contentGate := range content.Gates {
			if contentGate.ID == gate.ID {
				if contentGate.Status != ticket.GateSatisfied {
					t.Errorf("gate status = %q, want satisfied", contentGate.Status)
				}
			}
		}
		t.Logf("single reviewer approval verified: gate=%s", gate.ID)
	})

	t.Run("MultiReviewerThreshold", func(t *testing.T) {
		ticketID := createTicket(t, mainRoomID.RoomID,
			"Multi reviewer test", "task", 2, []string{"infra/multi/cluster-1"})

		content := readTicketState(t, admin, mainRoomID.RoomID, ticketID)
		gate := findStewardshipGate(t, content)
		if gate == nil {
			t.Fatalf("no stewardship gate; gates = %v", content.Gates)
		}

		// Verify all three stewards are reviewers.
		stewardUserIDs := map[ref.UserID]bool{
			alphaAccount.UserID: false,
			betaAccount.UserID:  false,
			gammaAccount.UserID: false,
		}
		for _, reviewer := range content.Review.Reviewers {
			if _, expected := stewardUserIDs[reviewer.UserID]; expected {
				stewardUserIDs[reviewer.UserID] = true
			}
		}
		for userID, found := range stewardUserIDs {
			if !found {
				t.Errorf("steward %s not in reviewers", userID)
			}
		}

		// Verify threshold is 2.
		if len(content.Review.TierThresholds) == 0 {
			t.Fatal("no tier thresholds")
		}
		if content.Review.TierThresholds[0].Threshold == nil || *content.Review.TierThresholds[0].Threshold != 2 {
			t.Errorf("threshold = %v, want 2", content.Review.TierThresholds[0].Threshold)
		}

		transitionToReview(t, mainRoomID.RoomID, ticketID)

		// Alpha approves — gate should stay pending (1 of 2).
		if err := alphaClient.Call(ctx, "set-disposition", map[string]any{
			"room":        mainRoomID.RoomID.String(),
			"ticket":      ticketID,
			"disposition": "approved",
		}, nil); err != nil {
			t.Fatalf("alpha set-disposition: %v", err)
		}

		content = readTicketState(t, admin, mainRoomID.RoomID, ticketID)
		for _, contentGate := range content.Gates {
			if contentGate.ID == gate.ID {
				if contentGate.Status != ticket.GatePending {
					t.Errorf("after 1 approval: gate status = %q, want pending", contentGate.Status)
				}
			}
		}

		// Beta approves — gate should be satisfied (2 of 2).
		gateWatch := watchRoom(t, admin, mainRoomID.RoomID)
		if err := betaClient.Call(ctx, "set-disposition", map[string]any{
			"room":        mainRoomID.RoomID.String(),
			"ticket":      ticketID,
			"disposition": "approved",
		}, nil); err != nil {
			t.Fatalf("beta set-disposition: %v", err)
		}

		waitForGateSatisfied(t, &gateWatch, ticketID, gate.ID)

		content = readTicketState(t, admin, mainRoomID.RoomID, ticketID)

		// Verify: alpha approved, beta approved, gamma still pending.
		for _, reviewer := range content.Review.Reviewers {
			switch reviewer.UserID {
			case alphaAccount.UserID:
				if reviewer.Disposition != ticket.DispositionApproved {
					t.Errorf("alpha disposition = %q, want approved", reviewer.Disposition)
				}
			case betaAccount.UserID:
				if reviewer.Disposition != ticket.DispositionApproved {
					t.Errorf("beta disposition = %q, want approved", reviewer.Disposition)
				}
			case gammaAccount.UserID:
				if reviewer.Disposition != ticket.DispositionPending {
					t.Errorf("gamma disposition = %q, want pending", reviewer.Disposition)
				}
			}
		}

		t.Logf("multi-reviewer threshold verified: 2-of-3, gate=%s", gate.ID)
	})

	t.Run("RejectionAndReApproval", func(t *testing.T) {
		ticketID := createTicket(t, mainRoomID.RoomID,
			"Rejection test", "task", 2, []string{"infra/single/node-2"})

		content := readTicketState(t, admin, mainRoomID.RoomID, ticketID)
		gate := findStewardshipGate(t, content)
		if gate == nil {
			t.Fatalf("no stewardship gate; gates = %v", content.Gates)
		}

		transitionToReview(t, mainRoomID.RoomID, ticketID)

		// Alpha requests changes — gate stays pending.
		if err := alphaClient.Call(ctx, "set-disposition", map[string]any{
			"room":        mainRoomID.RoomID.String(),
			"ticket":      ticketID,
			"disposition": "changes_requested",
		}, nil); err != nil {
			t.Fatalf("alpha set-disposition changes_requested: %v", err)
		}

		content = readTicketState(t, admin, mainRoomID.RoomID, ticketID)
		for _, reviewer := range content.Review.Reviewers {
			if reviewer.UserID == alphaAccount.UserID {
				if reviewer.Disposition != ticket.DispositionChangesRequested {
					t.Errorf("after rejection: disposition = %q, want changes_requested", reviewer.Disposition)
				}
			}
		}
		for _, contentGate := range content.Gates {
			if contentGate.ID == gate.ID {
				if contentGate.Status != ticket.GatePending {
					t.Errorf("after rejection: gate status = %q, want pending", contentGate.Status)
				}
			}
		}

		// Alpha re-approves — gate satisfied.
		gateWatch := watchRoom(t, admin, mainRoomID.RoomID)
		if err := alphaClient.Call(ctx, "set-disposition", map[string]any{
			"room":        mainRoomID.RoomID.String(),
			"ticket":      ticketID,
			"disposition": "approved",
		}, nil); err != nil {
			t.Fatalf("alpha set-disposition approved: %v", err)
		}

		waitForGateSatisfied(t, &gateWatch, ticketID, gate.ID)

		content = readTicketState(t, admin, mainRoomID.RoomID, ticketID)
		for _, reviewer := range content.Review.Reviewers {
			if reviewer.UserID == alphaAccount.UserID {
				if reviewer.Disposition != ticket.DispositionApproved {
					t.Errorf("after re-approval: disposition = %q, want approved", reviewer.Disposition)
				}
				if reviewer.UpdatedAt == "" {
					t.Error("after re-approval: UpdatedAt is empty")
				}
			}
		}

		t.Logf("rejection + re-approval verified: gate=%s", gate.ID)
	})

	t.Run("NonReviewerDenied", func(t *testing.T) {
		ticketID := createTicket(t, mainRoomID.RoomID,
			"Non-reviewer test", "task", 2, []string{"infra/single/node-3"})

		transitionToReview(t, mainRoomID.RoomID, ticketID)

		// Beta is not a reviewer for the "single" declaration (only alpha is).
		err := betaClient.Call(ctx, "set-disposition", map[string]any{
			"room":        mainRoomID.RoomID.String(),
			"ticket":      ticketID,
			"disposition": "approved",
		}, nil)
		if err == nil {
			t.Fatal("expected error from non-reviewer set-disposition, got nil")
		}
		if !strings.Contains(err.Error(), "not in the reviewer list") {
			t.Errorf("unexpected error: %v (expected 'not in the reviewer list')", err)
		}

		// Verify ticket state unchanged.
		content := readTicketState(t, admin, mainRoomID.RoomID, ticketID)
		for _, reviewer := range content.Review.Reviewers {
			if reviewer.UserID == alphaAccount.UserID {
				if reviewer.Disposition != ticket.DispositionPending {
					t.Errorf("disposition changed unexpectedly: %q", reviewer.Disposition)
				}
			}
		}

		t.Log("non-reviewer set-disposition correctly rejected")
	})

	t.Run("ReviewStatusRequired", func(t *testing.T) {
		ticketID := createTicket(t, mainRoomID.RoomID,
			"Status required test", "task", 2, []string{"infra/single/node-4"})

		// Do NOT transition to review — ticket is in "open" status.
		err := alphaClient.Call(ctx, "set-disposition", map[string]any{
			"room":        mainRoomID.RoomID.String(),
			"ticket":      ticketID,
			"disposition": "approved",
		}, nil)
		if err == nil {
			t.Fatal("expected error from set-disposition on non-review ticket, got nil")
		}
		if !strings.Contains(err.Error(), "not in review status") {
			t.Errorf("unexpected error: %v (expected 'not in review status')", err)
		}

		t.Log("set-disposition on non-review ticket correctly rejected")
	})

	t.Run("TieredReview", func(t *testing.T) {
		ticketID := createTicket(t, mainRoomID.RoomID,
			"Tiered review test", "task", 2, []string{"infra/tiered/storage-1"})

		content := readTicketState(t, admin, mainRoomID.RoomID, ticketID)
		gate := findStewardshipGate(t, content)
		if gate == nil {
			t.Fatalf("no stewardship gate; gates = %v", content.Gates)
		}

		// Verify reviewers in different tiers.
		var alphaTier, betaTier int
		alphaTier = -1
		betaTier = -1
		for _, reviewer := range content.Review.Reviewers {
			if reviewer.UserID == alphaAccount.UserID {
				alphaTier = reviewer.Tier
			}
			if reviewer.UserID == betaAccount.UserID {
				betaTier = reviewer.Tier
			}
		}
		if alphaTier < 0 {
			t.Fatal("alpha not in reviewers")
		}
		if betaTier < 0 {
			t.Fatal("beta not in reviewers")
		}
		if alphaTier >= betaTier {
			t.Errorf("alpha tier (%d) should be lower than beta tier (%d)", alphaTier, betaTier)
		}

		transitionToReview(t, mainRoomID.RoomID, ticketID)

		// Alpha approves (tier 0) — gate stays pending (tier 1 unsatisfied).
		if err := alphaClient.Call(ctx, "set-disposition", map[string]any{
			"room":        mainRoomID.RoomID.String(),
			"ticket":      ticketID,
			"disposition": "approved",
		}, nil); err != nil {
			t.Fatalf("alpha set-disposition: %v", err)
		}

		content = readTicketState(t, admin, mainRoomID.RoomID, ticketID)
		for _, contentGate := range content.Gates {
			if contentGate.ID == gate.ID {
				if contentGate.Status != ticket.GatePending {
					t.Errorf("after tier 0 approval: gate status = %q, want pending", contentGate.Status)
				}
			}
		}

		// Beta approves (tier 1) — gate satisfied (both tiers done).
		gateWatch := watchRoom(t, admin, mainRoomID.RoomID)
		if err := betaClient.Call(ctx, "set-disposition", map[string]any{
			"room":        mainRoomID.RoomID.String(),
			"ticket":      ticketID,
			"disposition": "approved",
		}, nil); err != nil {
			t.Fatalf("beta set-disposition: %v", err)
		}

		waitForGateSatisfied(t, &gateWatch, ticketID, gate.ID)

		t.Logf("tiered review verified: tier 0=%d, tier 1=%d, gate=%s", alphaTier, betaTier, gate.ID)
	})

	t.Run("NotificationOnCreate", func(t *testing.T) {
		// Watch the main room for the notification message before
		// creating the ticket that triggers it.
		notifyWatch := watchRoom(t, admin, mainRoomID.RoomID)

		ticketID := createTicket(t, mainRoomID.RoomID,
			"Notify trigger", "bug", 3, []string{"infra/notify/monitor-1"})

		// Notification should arrive as m.room.message from the
		// ticket service, containing "Stewardship notification"
		// and the ticket ID.
		body := notifyWatch.WaitForMessage(t,
			"Stewardship notification", ticketSvc.Account.UserID)

		if !strings.Contains(body, ticketID) {
			t.Errorf("notification body does not contain ticket ID %q: %s", ticketID, body)
		}
		if !strings.Contains(body, "Notify trigger") {
			t.Errorf("notification body does not contain title: %s", body)
		}
		if !strings.Contains(body, "bug") {
			t.Errorf("notification body does not contain ticket type: %s", body)
		}

		// Verify no stewardship gate on the ticket (NotifyTypes, not GateTypes).
		content := readTicketState(t, admin, mainRoomID.RoomID, ticketID)
		gate := findStewardshipGate(t, content)
		if gate != nil {
			t.Errorf("unexpected stewardship gate on notify-only ticket: %v", gate)
		}

		t.Logf("notification verified for %s", ticketID)
	})

	t.Run("NotificationNotGate", func(t *testing.T) {
		ticketID := createTicket(t, mainRoomID.RoomID,
			"Notify no gate", "bug", 2, []string{"infra/notify/monitor-2"})

		content := readTicketState(t, admin, mainRoomID.RoomID, ticketID)

		// No stewardship gate for NotifyTypes-only declarations.
		gate := findStewardshipGate(t, content)
		if gate != nil {
			t.Errorf("unexpected stewardship gate on notify-only ticket: %v", gate)
		}

		// Review section should be nil (no review gate).
		if content.Review != nil && len(content.Review.Reviewers) > 0 {
			t.Errorf("unexpected reviewers on notify-only ticket: %v", content.Review.Reviewers)
		}

		t.Log("notify-only correctly produces no gate or reviewers")
	})

	t.Run("CrossRoomStewardship", func(t *testing.T) {
		// Create a ticket in the ticket room (no stewardship) with
		// affects matching the declaration in the declaration room.
		ticketID := createTicket(t, crossTicketRoomID,
			"Cross-room test", "task", 2, []string{"platform/auth"})

		content := readTicketState(t, admin, crossTicketRoomID, ticketID)
		gate := findStewardshipGate(t, content)
		if gate == nil {
			t.Fatalf("no stewardship gate from cross-room declaration; gates = %v", content.Gates)
		}
		if gate.Type != ticket.GateReview {
			t.Errorf("gate type = %q, want review", gate.Type)
		}

		// Verify reviewers are resolved from the declaration room's
		// membership (where stewards are joined), not the ticket room.
		if content.Review == nil || len(content.Review.Reviewers) == 0 {
			t.Fatal("no reviewers resolved from cross-room declaration")
		}

		var foundSteward bool
		for _, reviewer := range content.Review.Reviewers {
			if reviewer.UserID == alphaAccount.UserID ||
				reviewer.UserID == betaAccount.UserID ||
				reviewer.UserID == gammaAccount.UserID {
				foundSteward = true
				break
			}
		}
		if !foundSteward {
			t.Errorf("no steward account in reviewers (expected from declaration room membership): %v", content.Review.Reviewers)
		}

		t.Logf("cross-room stewardship verified: gate=%s, reviewers=%d",
			gate.ID, len(content.Review.Reviewers))
	})

	t.Run("AgentReview", func(t *testing.T) {
		// Deploy an agent that creates a stewardship-gated ticket
		// via the bureau_ticket_create MCP tool. This proves the
		// full path: agent → MCP → proxy → daemon token → service
		// socket → stewardship resolution → Matrix state event.
		//
		// The review approval is done via socket by the admin (who
		// is the stewardship-resolved reviewer in the agent room).
		// This verifies gate satisfaction through the /sync loop.

		agent := deployAgent(t, admin, machine, agentOptions{
			Binary:           testutil.DataBinary(t, "BUREAU_AGENT_BINARY"),
			Localpart:        "agent/stw-review-e2e",
			RequiredServices: []string{"ticket"},
			ExtraEnv: map[string]string{
				"BUREAU_AGENT_MODEL":      "mock-model",
				"BUREAU_AGENT_SERVICE":    "anthropic",
				"BUREAU_AGENT_MAX_TOKENS": "1024",
			},
			Payload: map[string]any{"WORKSPACE_ROOM_ID": agentRoomID.RoomID},
			Authorization: &schema.AuthorizationPolicy{
				Grants: []schema.Grant{
					{Actions: []string{schema.ActionCommandTicketAll, ticket.ActionAll}},
				},
			},
		})

		// Mock LLM: agent creates a ticket with affects targeting the
		// agent room's stewardship declaration.
		agentProjectWatch := watchRoom(t, admin, agentRoomID.RoomID)

		mock := newMockToolSequence(t, []mockToolStep{{
			ToolName: "bureau_ticket_create",
			ToolInput: func() map[string]any {
				return map[string]any{
					"room":     agentRoomID.RoomID,
					"title":    "Agent-created stewardship ticket",
					"type":     "task",
					"priority": 2,
					"affects":  []string{"deploy/production/api"},
				}
			},
		}})

		registerProxyHTTPService(t, agent.AdminSocketPath, "anthropic", mock.URL)

		if _, err := admin.SendMessage(ctx, machine.ConfigRoomID,
			messaging.NewTargetedTextMessage("Create a deployment ticket", agent.Account.UserID)); err != nil {
			t.Fatalf("send prompt to agent: %v", err)
		}

		waitForMockCompletion(t, mock)

		// Verify the ticket has a stewardship gate.
		ticketID, ticketContent := waitForTicket(t, &agentProjectWatch, "Agent-created stewardship ticket")
		gate := findStewardshipGate(t, ticketContent)
		if gate == nil {
			t.Fatalf("agent-created ticket has no stewardship gate; gates = %v", ticketContent.Gates)
		}
		if gate.Type != ticket.GateReview {
			t.Errorf("gate type = %q, want review", gate.Type)
		}

		// Verify admin is a reviewer (matched by admin-*:test.bureau.local).
		if ticketContent.Review == nil || len(ticketContent.Review.Reviewers) == 0 {
			t.Fatal("no reviewers on agent-created ticket")
		}
		var adminIsReviewer bool
		for _, reviewer := range ticketContent.Review.Reviewers {
			if reviewer.UserID == adminUserID {
				adminIsReviewer = true
			}
		}
		if !adminIsReviewer {
			t.Errorf("admin not in reviewers: %v", ticketContent.Review.Reviewers)
		}

		// Approve the review as the admin to verify gate satisfaction
		// through the full /sync loop.
		if err := operatorClient.Call(ctx, "update", map[string]any{
			"room":   agentRoomID.RoomID.String(),
			"ticket": ticketID,
			"status": string(ticket.StatusReview),
		}, nil); err != nil {
			t.Fatalf("transition to review: %v", err)
		}

		adminToken := mintTestServiceTokenForUser(t, machine, adminUserID, "ticket",
			[]servicetoken.Grant{{Actions: []string{ticket.ActionAll}}})
		adminReviewClient := service.NewServiceClientFromToken(ticketSvc.SocketPath, adminToken)

		gateWatch := watchRoom(t, admin, agentRoomID.RoomID)
		if err := adminReviewClient.Call(ctx, "set-disposition", map[string]any{
			"room":        agentRoomID.RoomID.String(),
			"ticket":      ticketID,
			"disposition": "approved",
		}, nil); err != nil {
			t.Fatalf("admin set-disposition: %v", err)
		}

		waitForGateSatisfied(t, &gateWatch, ticketID, gate.ID)

		t.Logf("agent-created stewardship ticket verified: %s gate=%s", ticketID, gate.ID)
	})
}
