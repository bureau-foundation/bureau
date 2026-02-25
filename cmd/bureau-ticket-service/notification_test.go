// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/stewardship"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/stewardshipindex"
	"github.com/bureau-foundation/bureau/messaging"
)

// --- resolveStewardshipNotifications tests ---

func TestResolveStewardshipNotifications(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!steward-room:bureau.local")

	// Declaration with NotifyTypes including "task" and GateTypes
	// including "bug". A task ticket should match NotifyTypes, not
	// GateTypes.
	ts.stewardshipIndex.Put(roomID, "fleet/gpu", stewardship.StewardshipContent{
		Version:          1,
		ResourcePatterns: []string{"fleet/gpu/**"},
		GateTypes:        []ticket.TicketType{ticket.TypeBug},
		NotifyTypes:      []ticket.TicketType{ticket.TypeTask},
		Tiers: []stewardship.StewardshipTier{
			{Principals: []string{"dev/*:bureau.local"}},
		},
	})

	// Task type should match NotifyTypes.
	declarations := ts.resolveStewardshipNotifications(
		[]string{"fleet/gpu/a100"}, ticket.TypeTask)
	if len(declarations) != 1 {
		t.Fatalf("expected 1 declaration, got %d", len(declarations))
	}
	if declarations[0].StateKey != "fleet/gpu" {
		t.Errorf("state_key: got %q, want fleet/gpu", declarations[0].StateKey)
	}

	// Bug type should NOT match NotifyTypes (it's in GateTypes).
	declarations = ts.resolveStewardshipNotifications(
		[]string{"fleet/gpu/a100"}, ticket.TypeBug)
	if len(declarations) != 0 {
		t.Errorf("expected 0 declarations for bug type, got %d", len(declarations))
	}

	// Feature type matches neither.
	declarations = ts.resolveStewardshipNotifications(
		[]string{"fleet/gpu/a100"}, ticket.TypeFeature)
	if len(declarations) != 0 {
		t.Errorf("expected 0 declarations for feature type, got %d", len(declarations))
	}
}

func TestResolveStewardshipNotificationsDedup(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!steward-room:bureau.local")

	// Declaration with multiple resource patterns that both match.
	ts.stewardshipIndex.Put(roomID, "fleet/gpu", stewardship.StewardshipContent{
		Version:          1,
		ResourcePatterns: []string{"fleet/gpu/**", "fleet/**"},
		NotifyTypes:      []ticket.TicketType{ticket.TypeTask},
		Tiers: []stewardship.StewardshipTier{
			{Principals: []string{"dev/*:bureau.local"}},
		},
	})

	// Both patterns match fleet/gpu/a100, but declaration should appear once.
	declarations := ts.resolveStewardshipNotifications(
		[]string{"fleet/gpu/a100"}, ticket.TypeTask)
	if len(declarations) != 1 {
		t.Fatalf("expected 1 declaration (deduplicated), got %d", len(declarations))
	}
}

func TestResolveStewardshipNotificationsNoMatch(t *testing.T) {
	ts := newTestService()

	// No declarations configured.
	declarations := ts.resolveStewardshipNotifications(
		[]string{"fleet/gpu/a100"}, ticket.TypeTask)
	if len(declarations) != 0 {
		t.Errorf("expected 0 declarations, got %d", len(declarations))
	}

	// Empty affects.
	declarations = ts.resolveStewardshipNotifications(nil, ticket.TypeTask)
	if len(declarations) != 0 {
		t.Errorf("expected 0 declarations for nil affects, got %d", len(declarations))
	}
}

// --- resolveNotificationPrincipals tests ---

func TestResolveNotificationPrincipals(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!steward-room:bureau.local")

	ts.membersByRoom[roomID] = map[ref.UserID]roomMember{
		ref.MustParseUserID("@dev/lead:bureau.local"):     {DisplayName: "Lead"},
		ref.MustParseUserID("@dev/worker:bureau.local"):   {DisplayName: "Worker"},
		ref.MustParseUserID("@bureau/admin:bureau.local"): {DisplayName: "Admin"},
	}

	declaration := stewardshipindex.Declaration{
		RoomID:   roomID,
		StateKey: "fleet/gpu",
		Content: stewardship.StewardshipContent{
			Tiers: []stewardship.StewardshipTier{
				{Principals: []string{"dev/*:bureau.local"}},
				{Principals: []string{"bureau/admin:bureau.local"}},
			},
		},
	}

	principals := ts.resolveNotificationPrincipals(declaration)
	if len(principals) != 3 {
		t.Fatalf("expected 3 principals, got %d", len(principals))
	}

	// Verify sorted order.
	for i := 1; i < len(principals); i++ {
		if principals[i].String() < principals[i-1].String() {
			t.Error("principals should be sorted")
		}
	}
}

func TestResolveNotificationPrincipalsEmptyRoom(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!empty-room:bureau.local")

	declaration := stewardshipindex.Declaration{
		RoomID:   roomID,
		StateKey: "fleet/gpu",
		Content: stewardship.StewardshipContent{
			Tiers: []stewardship.StewardshipTier{
				{Principals: []string{"dev/*:bureau.local"}},
			},
		},
	}

	principals := ts.resolveNotificationPrincipals(declaration)
	if len(principals) != 0 {
		t.Errorf("expected 0 principals for empty room, got %d", len(principals))
	}
}

func TestResolveNotificationPrincipalsDedup(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!steward-room:bureau.local")

	ts.membersByRoom[roomID] = map[ref.UserID]roomMember{
		ref.MustParseUserID("@dev/lead:bureau.local"): {DisplayName: "Lead"},
	}

	// Same user matched by patterns in two different tiers.
	declaration := stewardshipindex.Declaration{
		RoomID:   roomID,
		StateKey: "fleet/gpu",
		Content: stewardship.StewardshipContent{
			Tiers: []stewardship.StewardshipTier{
				{Principals: []string{"dev/*:bureau.local"}},
				{Principals: []string{"dev/lead:bureau.local"}},
			},
		},
	}

	principals := ts.resolveNotificationPrincipals(declaration)
	if len(principals) != 1 {
		t.Fatalf("expected 1 principal (deduplicated), got %d", len(principals))
	}
}

// --- formatImmediateNotification tests ---

func TestFormatImmediateNotification(t *testing.T) {
	principals := []ref.UserID{
		ref.MustParseUserID("@dev/lead:bureau.local"),
		ref.MustParseUserID("@dev/worker:bureau.local"),
	}
	ticketRoomID := testRoomID("!tickets:bureau.local")

	content := ticket.TicketContent{
		Title:    "GPU cluster outage",
		Type:     ticket.TypeBug,
		Priority: 0,
		Affects:  []string{"fleet/gpu/a100"},
	}

	message := formatImmediateNotification("tkt-abc1", content, ticketRoomID, principals)

	if message.MsgType != "m.text" {
		t.Errorf("MsgType: got %q, want m.text", message.MsgType)
	}
	if !strings.Contains(message.Body, "tkt-abc1") {
		t.Error("body should contain ticket ID")
	}
	if !strings.Contains(message.Body, "GPU cluster outage") {
		t.Error("body should contain ticket title")
	}
	if !strings.Contains(message.Body, "bug") {
		t.Error("body should contain ticket type")
	}
	if !strings.Contains(message.Body, "P0") {
		t.Error("body should contain priority")
	}
	if !strings.Contains(message.Body, "fleet/gpu/a100") {
		t.Error("body should contain affects")
	}
	if message.Mentions == nil {
		t.Fatal("mentions should be set")
	}
	if len(message.Mentions.UserIDs) != 2 {
		t.Fatalf("mentions: expected 2 user IDs, got %d", len(message.Mentions.UserIDs))
	}
	if !strings.Contains(message.Body, "@dev/lead:bureau.local") {
		t.Error("body should mention @dev/lead:bureau.local")
	}
}

// --- formatDigestNotification tests ---

func TestFormatDigestNotification(t *testing.T) {
	declaration := stewardshipindex.Declaration{
		StateKey: "fleet/gpu",
		Content: stewardship.StewardshipContent{
			Description: "GPU fleet stewardship",
		},
	}

	tickets := []digestTicket{
		{ticketID: "tkt-a1", title: "GPU allocation", ticketType: ticket.TypeFeature, priority: 1},
		{ticketID: "tkt-b2", title: "GPU driver bug", ticketType: ticket.TypeBug, priority: 2},
		{ticketID: "tkt-c3", title: "GPU monitoring", ticketType: ticket.TypeTask, priority: 3},
	}

	principals := []ref.UserID{
		ref.MustParseUserID("@dev/lead:bureau.local"),
	}

	message := formatDigestNotification(declaration, tickets, principals)

	if message.MsgType != "m.text" {
		t.Errorf("MsgType: got %q, want m.text", message.MsgType)
	}
	if !strings.Contains(message.Body, "GPU fleet stewardship") {
		t.Error("body should contain declaration description")
	}
	if !strings.Contains(message.Body, "3 tickets") {
		t.Error("body should contain ticket count")
	}
	if !strings.Contains(message.Body, "tkt-a1") {
		t.Error("body should contain first ticket ID")
	}
	if !strings.Contains(message.Body, "GPU driver bug") {
		t.Error("body should contain second ticket title")
	}
	if !strings.Contains(message.Body, "P2") {
		t.Error("body should contain second ticket priority")
	}
	if message.Mentions == nil {
		t.Fatal("mentions should be set")
	}
	if len(message.Mentions.UserIDs) != 1 {
		t.Fatalf("mentions: expected 1 user ID, got %d", len(message.Mentions.UserIDs))
	}
}

func TestFormatDigestNotificationUsesStateKeyWhenNoDescription(t *testing.T) {
	declaration := stewardshipindex.Declaration{
		StateKey: "fleet/gpu",
		Content:  stewardship.StewardshipContent{},
	}

	message := formatDigestNotification(declaration, []digestTicket{
		{ticketID: "tkt-1", title: "Test", ticketType: ticket.TypeTask, priority: 2},
	}, []ref.UserID{ref.MustParseUserID("@dev/lead:bureau.local")})

	if !strings.Contains(message.Body, "fleet/gpu") {
		t.Error("body should use state_key when description is empty")
	}
}

// --- Integration tests with socket server ---

func TestCreateWithNotifyTypeP0Immediate(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	stewardRoomID := testRoomID("!steward-room:bureau.local")
	env.service.membersByRoom[stewardRoomID] = map[ref.UserID]roomMember{
		ref.MustParseUserID("@dev/lead:bureau.local"): {DisplayName: "Lead"},
	}
	env.service.stewardshipIndex.Put(stewardRoomID, "fleet/gpu", stewardship.StewardshipContent{
		Version:          1,
		ResourcePatterns: []string{"fleet/gpu/**"},
		NotifyTypes:      []ticket.TicketType{ticket.TypeTask},
		DigestInterval:   "1h",
		Tiers: []stewardship.StewardshipTier{
			{Principals: []string{"dev/*:bureau.local"}},
		},
	})

	// P0 ticket should bypass digest and send immediately.
	var result createResponse
	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!room:bureau.local",
		"title":    "P0 GPU outage",
		"type":     "task",
		"priority": 0,
		"affects":  []string{"fleet/gpu/a100"},
	}, &result)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	env.messenger.mu.Lock()
	messages := env.messenger.messages
	env.messenger.mu.Unlock()

	// Should have at least one message to the steward room.
	found := false
	for _, message := range messages {
		if message.RoomID == stewardRoomID.String() {
			found = true
			if !strings.Contains(message.Content.Body, result.ID) {
				t.Error("notification should contain ticket ID")
			}
			if !strings.Contains(message.Content.Body, "P0") {
				t.Error("notification should contain priority")
			}
			if message.Content.Mentions == nil || len(message.Content.Mentions.UserIDs) == 0 {
				t.Error("notification should mention steward principals")
			}
		}
	}
	if !found {
		t.Error("expected immediate notification to steward room for P0 ticket")
	}
}

func TestCreateWithNotifyTypeP1Immediate(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	stewardRoomID := testRoomID("!steward-room:bureau.local")
	env.service.membersByRoom[stewardRoomID] = map[ref.UserID]roomMember{
		ref.MustParseUserID("@dev/lead:bureau.local"): {DisplayName: "Lead"},
	}
	env.service.stewardshipIndex.Put(stewardRoomID, "fleet/gpu", stewardship.StewardshipContent{
		Version:          1,
		ResourcePatterns: []string{"fleet/gpu/**"},
		NotifyTypes:      []ticket.TicketType{ticket.TypeTask},
		DigestInterval:   "4h",
		Tiers: []stewardship.StewardshipTier{
			{Principals: []string{"dev/*:bureau.local"}},
		},
	})

	var result createResponse
	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!room:bureau.local",
		"title":    "P1 GPU issue",
		"type":     "task",
		"priority": 1,
		"affects":  []string{"fleet/gpu/a100"},
	}, &result)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	env.messenger.mu.Lock()
	messageCount := len(env.messenger.messages)
	env.messenger.mu.Unlock()

	if messageCount == 0 {
		t.Error("P1 tickets should bypass digest and send immediately")
	}
}

func TestCreateWithNotifyTypeDigestQueued(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	stewardRoomID := testRoomID("!steward-room:bureau.local")
	env.service.membersByRoom[stewardRoomID] = map[ref.UserID]roomMember{
		ref.MustParseUserID("@dev/lead:bureau.local"): {DisplayName: "Lead"},
	}
	env.service.stewardshipIndex.Put(stewardRoomID, "fleet/gpu", stewardship.StewardshipContent{
		Version:          1,
		ResourcePatterns: []string{"fleet/gpu/**"},
		NotifyTypes:      []ticket.TicketType{ticket.TypeTask},
		DigestInterval:   "1h",
		Tiers: []stewardship.StewardshipTier{
			{Principals: []string{"dev/*:bureau.local"}},
		},
	})

	// P2 ticket should be queued in digest, not sent immediately.
	var result createResponse
	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!room:bureau.local",
		"title":    "P2 GPU issue",
		"type":     "task",
		"priority": 2,
		"affects":  []string{"fleet/gpu/a100"},
	}, &result)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	env.messenger.mu.Lock()
	messageCount := len(env.messenger.messages)
	env.messenger.mu.Unlock()

	if messageCount != 0 {
		t.Errorf("P2 ticket with digest interval should not send immediately, got %d messages", messageCount)
	}

	// Verify the digest entry was created.
	key := digestKey{roomID: stewardRoomID, stateKey: "fleet/gpu"}
	entry, exists := env.service.digestTimers[key]
	if !exists {
		t.Fatal("expected digest entry to be created")
	}
	if len(entry.tickets) != 1 {
		t.Errorf("digest: expected 1 ticket, got %d", len(entry.tickets))
	}
	if entry.timer == nil {
		t.Error("digest timer should be set")
	}
}

func TestDigestFlushAfterInterval(t *testing.T) {
	env := newTestServer(t, mutationRooms(), testServerOpts{withTimers: true})
	defer env.cleanup()

	// Enable messenger notification so we can synchronize with
	// the digest loop goroutine.
	env.messenger.notify = make(chan struct{}, 16)

	stewardRoomID := testRoomID("!steward-room:bureau.local")
	env.service.membersByRoom[stewardRoomID] = map[ref.UserID]roomMember{
		ref.MustParseUserID("@dev/lead:bureau.local"): {DisplayName: "Lead"},
	}
	env.service.stewardshipIndex.Put(stewardRoomID, "fleet/gpu", stewardship.StewardshipContent{
		Version:          1,
		ResourcePatterns: []string{"fleet/gpu/**"},
		NotifyTypes:      []ticket.TicketType{ticket.TypeTask},
		DigestInterval:   "1h",
		Tiers: []stewardship.StewardshipTier{
			{Principals: []string{"dev/*:bureau.local"}},
		},
	})

	// Start the digest loop.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go env.service.startDigestLoop(ctx)

	// Create a P2 ticket — should be queued.
	var result createResponse
	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!room:bureau.local",
		"title":    "P2 GPU issue",
		"type":     "task",
		"priority": 2,
		"affects":  []string{"fleet/gpu/a100"},
	}, &result)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// No message yet.
	env.messenger.mu.Lock()
	beforeCount := len(env.messenger.messages)
	env.messenger.mu.Unlock()
	if beforeCount != 0 {
		t.Fatalf("expected 0 messages before clock advance, got %d", beforeCount)
	}

	// Wait for the digest timer to register, then advance the clock
	// past the 1h interval. AfterFunc fires synchronously in
	// Advance, signaling digestNotify. Then wait for the digest
	// loop goroutine to process and send the message.
	env.clock.WaitForTimers(1)
	env.clock.Advance(61 * time.Minute)
	<-env.messenger.notify

	env.messenger.mu.Lock()
	afterMessages := make([]sentMessage, len(env.messenger.messages))
	copy(afterMessages, env.messenger.messages)
	env.messenger.mu.Unlock()

	found := false
	for _, message := range afterMessages {
		if message.RoomID == stewardRoomID.String() {
			found = true
			if !strings.Contains(message.Content.Body, "digest") {
				t.Error("digest message should contain 'digest'")
			}
			if !strings.Contains(message.Content.Body, result.ID) {
				t.Error("digest should contain ticket ID")
			}
		}
	}
	if !found {
		t.Errorf("expected digest flush after clock advance, got %d total messages", len(afterMessages))
	}
}

func TestDigestAccumulatesMultipleTickets(t *testing.T) {
	env := newTestServer(t, mutationRooms(), testServerOpts{withTimers: true})
	defer env.cleanup()

	// Enable messenger notification for digest loop synchronization.
	env.messenger.notify = make(chan struct{}, 16)

	stewardRoomID := testRoomID("!steward-room:bureau.local")
	env.service.membersByRoom[stewardRoomID] = map[ref.UserID]roomMember{
		ref.MustParseUserID("@dev/lead:bureau.local"): {DisplayName: "Lead"},
	}
	env.service.stewardshipIndex.Put(stewardRoomID, "fleet/gpu", stewardship.StewardshipContent{
		Version:          1,
		ResourcePatterns: []string{"fleet/gpu/**"},
		NotifyTypes:      []ticket.TicketType{ticket.TypeTask},
		DigestInterval:   "1h",
		Tiers: []stewardship.StewardshipTier{
			{Principals: []string{"dev/*:bureau.local"}},
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go env.service.startDigestLoop(ctx)

	// Create three P2 tickets.
	var ticketIDs []string
	for _, title := range []string{"GPU issue 1", "GPU issue 2", "GPU issue 3"} {
		var result createResponse
		err := env.client.Call(context.Background(), "create", map[string]any{
			"room":     "!room:bureau.local",
			"title":    title,
			"type":     "task",
			"priority": 2,
			"affects":  []string{"fleet/gpu/a100"},
		}, &result)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		ticketIDs = append(ticketIDs, result.ID)
	}

	// Advance clock past interval and wait for the digest loop to
	// send the flush message.
	env.clock.WaitForTimers(1)
	env.clock.Advance(61 * time.Minute)
	<-env.messenger.notify

	env.messenger.mu.Lock()
	messages := make([]sentMessage, len(env.messenger.messages))
	copy(messages, env.messenger.messages)
	env.messenger.mu.Unlock()

	// Should be exactly one digest message with all three tickets.
	digestMessages := 0
	for _, message := range messages {
		if message.RoomID == stewardRoomID.String() {
			digestMessages++
			if !strings.Contains(message.Content.Body, "3 tickets") {
				t.Error("digest should mention 3 tickets")
			}
			for _, ticketID := range ticketIDs {
				if !strings.Contains(message.Content.Body, ticketID) {
					t.Errorf("digest should contain ticket ID %s", ticketID)
				}
			}
		}
	}
	if digestMessages != 1 {
		t.Errorf("expected 1 digest message, got %d", digestMessages)
	}
}

func TestCreateWithNotifyTypeNoDigestInterval(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	stewardRoomID := testRoomID("!steward-room:bureau.local")
	env.service.membersByRoom[stewardRoomID] = map[ref.UserID]roomMember{
		ref.MustParseUserID("@dev/lead:bureau.local"): {DisplayName: "Lead"},
	}
	// No DigestInterval — should send immediately for all priorities.
	env.service.stewardshipIndex.Put(stewardRoomID, "fleet/gpu", stewardship.StewardshipContent{
		Version:          1,
		ResourcePatterns: []string{"fleet/gpu/**"},
		NotifyTypes:      []ticket.TicketType{ticket.TypeTask},
		Tiers: []stewardship.StewardshipTier{
			{Principals: []string{"dev/*:bureau.local"}},
		},
	})

	var result createResponse
	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!room:bureau.local",
		"title":    "P3 GPU task",
		"type":     "task",
		"priority": 3,
		"affects":  []string{"fleet/gpu/a100"},
	}, &result)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	env.messenger.mu.Lock()
	messageCount := len(env.messenger.messages)
	env.messenger.mu.Unlock()

	if messageCount == 0 {
		t.Error("empty DigestInterval should send immediately even for low-priority tickets")
	}
}

func TestNotificationSentToDeclarationRoom(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	// Stewardship declaration is in a different room from tickets.
	stewardRoomID := testRoomID("!steward-room:bureau.local")
	ticketRoomID := testRoomID("!room:bureau.local")

	env.service.membersByRoom[stewardRoomID] = map[ref.UserID]roomMember{
		ref.MustParseUserID("@dev/lead:bureau.local"): {DisplayName: "Lead"},
	}
	env.service.stewardshipIndex.Put(stewardRoomID, "fleet/gpu", stewardship.StewardshipContent{
		Version:          1,
		ResourcePatterns: []string{"fleet/gpu/**"},
		NotifyTypes:      []ticket.TicketType{ticket.TypeTask},
		Tiers: []stewardship.StewardshipTier{
			{Principals: []string{"dev/*:bureau.local"}},
		},
	})

	var result createResponse
	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     ticketRoomID.String(),
		"title":    "GPU task",
		"type":     "task",
		"priority": 0,
		"affects":  []string{"fleet/gpu/a100"},
	}, &result)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	env.messenger.mu.Lock()
	messages := env.messenger.messages
	env.messenger.mu.Unlock()

	for _, message := range messages {
		if message.RoomID == ticketRoomID.String() {
			t.Error("notification should go to the declaration room, not the ticket room")
		}
	}

	found := false
	for _, message := range messages {
		if message.RoomID == stewardRoomID.String() {
			found = true
		}
	}
	if !found {
		t.Error("notification should be sent to the declaration's room")
	}
}

func TestUpdateAffectsTriggersNotification(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	stewardRoomID := testRoomID("!steward-room:bureau.local")
	env.service.membersByRoom[stewardRoomID] = map[ref.UserID]roomMember{
		ref.MustParseUserID("@dev/lead:bureau.local"): {DisplayName: "Lead"},
	}
	env.service.stewardshipIndex.Put(stewardRoomID, "fleet/gpu", stewardship.StewardshipContent{
		Version:          1,
		ResourcePatterns: []string{"fleet/gpu/**"},
		NotifyTypes:      []ticket.TicketType{ticket.TypeTask},
		Tiers: []stewardship.StewardshipTier{
			{Principals: []string{"dev/*:bureau.local"}},
		},
	})

	// Create without affects.
	var createResult createResponse
	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":  "!room:bureau.local",
		"title": "GPU task",
		"type":  "task",
	}, &createResult)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	env.messenger.mu.Lock()
	beforeCount := len(env.messenger.messages)
	env.messenger.mu.Unlock()
	if beforeCount != 0 {
		t.Fatalf("expected no notifications before update, got %d", beforeCount)
	}

	// Update to add affects — should trigger notification.
	err = env.client.Call(context.Background(), "update", map[string]any{
		"room":    "!room:bureau.local",
		"ticket":  createResult.ID,
		"affects": []string{"fleet/gpu/a100"},
	}, nil)
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	env.messenger.mu.Lock()
	afterCount := len(env.messenger.messages)
	env.messenger.mu.Unlock()
	if afterCount == 0 {
		t.Error("updating affects should trigger stewardship notification")
	}
}

func TestBatchCreateTriggersNotifications(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	stewardRoomID := testRoomID("!steward-room:bureau.local")
	env.service.membersByRoom[stewardRoomID] = map[ref.UserID]roomMember{
		ref.MustParseUserID("@dev/lead:bureau.local"): {DisplayName: "Lead"},
	}
	env.service.stewardshipIndex.Put(stewardRoomID, "fleet/gpu", stewardship.StewardshipContent{
		Version:          1,
		ResourcePatterns: []string{"fleet/gpu/**"},
		NotifyTypes:      []ticket.TicketType{ticket.TypeTask},
		Tiers: []stewardship.StewardshipTier{
			{Principals: []string{"dev/*:bureau.local"}},
		},
	})

	err := env.client.Call(context.Background(), "batch-create", map[string]any{
		"room": "!room:bureau.local",
		"tickets": []map[string]any{
			{"ref": "a", "title": "GPU task 1", "type": "task", "priority": 0, "affects": []string{"fleet/gpu/a100"}},
			{"ref": "b", "title": "GPU task 2", "type": "task", "priority": 0, "affects": []string{"fleet/gpu/v100"}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("batch-create: %v", err)
	}

	env.messenger.mu.Lock()
	messageCount := len(env.messenger.messages)
	env.messenger.mu.Unlock()

	// Each P0 ticket should trigger an immediate notification.
	if messageCount < 2 {
		t.Errorf("expected at least 2 immediate notifications for P0 batch, got %d", messageCount)
	}
}

func TestDigestFlushOnDeclarationRemoval(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	stewardRoomID := testRoomID("!steward-room:bureau.local")
	env.service.membersByRoom[stewardRoomID] = map[ref.UserID]roomMember{
		ref.MustParseUserID("@dev/lead:bureau.local"): {DisplayName: "Lead"},
	}
	env.service.stewardshipIndex.Put(stewardRoomID, "fleet/gpu", stewardship.StewardshipContent{
		Version:          1,
		ResourcePatterns: []string{"fleet/gpu/**"},
		NotifyTypes:      []ticket.TicketType{ticket.TypeTask},
		DigestInterval:   "1h",
		Tiers: []stewardship.StewardshipTier{
			{Principals: []string{"dev/*:bureau.local"}},
		},
	})

	// Create a P2 ticket to get something in the digest.
	var result createResponse
	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!room:bureau.local",
		"title":    "GPU task",
		"type":     "task",
		"priority": 2,
		"affects":  []string{"fleet/gpu/a100"},
	}, &result)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Verify digest has accumulated.
	key := digestKey{roomID: stewardRoomID, stateKey: "fleet/gpu"}
	if entry, exists := env.service.digestTimers[key]; !exists || len(entry.tickets) == 0 {
		t.Fatal("expected digest entry with accumulated ticket")
	}

	env.messenger.mu.Lock()
	beforeCount := len(env.messenger.messages)
	env.messenger.mu.Unlock()

	// Remove the stewardship declaration — should flush the digest.
	stateKey := "fleet/gpu"
	env.service.indexStewardshipEvent(context.Background(), stewardRoomID, fakeEvent("m.bureau.stewardship", &stateKey, map[string]any{}))

	env.messenger.mu.Lock()
	afterCount := len(env.messenger.messages)
	env.messenger.mu.Unlock()

	if afterCount <= beforeCount {
		t.Error("removing stewardship declaration should flush pending digest")
	}

	// Digest entry should be removed.
	if _, exists := env.service.digestTimers[key]; exists {
		t.Error("digest entry should be removed after declaration removal")
	}
}

func TestDigestFlushOnShutdown(t *testing.T) {
	env := newTestServer(t, mutationRooms(), testServerOpts{withTimers: true})
	defer env.cleanup()

	// Enable messenger notification for synchronization.
	env.messenger.notify = make(chan struct{}, 16)

	stewardRoomID := testRoomID("!steward-room:bureau.local")
	env.service.membersByRoom[stewardRoomID] = map[ref.UserID]roomMember{
		ref.MustParseUserID("@dev/lead:bureau.local"): {DisplayName: "Lead"},
	}
	env.service.stewardshipIndex.Put(stewardRoomID, "fleet/gpu", stewardship.StewardshipContent{
		Version:          1,
		ResourcePatterns: []string{"fleet/gpu/**"},
		NotifyTypes:      []ticket.TicketType{ticket.TypeTask},
		DigestInterval:   "1h",
		Tiers: []stewardship.StewardshipTier{
			{Principals: []string{"dev/*:bureau.local"}},
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	go env.service.startDigestLoop(ctx)

	// Create a P2 ticket — queued in digest, not sent immediately.
	var result createResponse
	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!room:bureau.local",
		"title":    "Pending GPU task",
		"type":     "task",
		"priority": 2,
		"affects":  []string{"fleet/gpu/a100"},
	}, &result)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	env.messenger.mu.Lock()
	beforeCount := len(env.messenger.messages)
	env.messenger.mu.Unlock()
	if beforeCount != 0 {
		t.Fatalf("expected 0 messages before shutdown, got %d", beforeCount)
	}

	// Cancel the context — the digest loop should flush pending
	// digests before returning.
	cancel()
	<-env.messenger.notify

	env.messenger.mu.Lock()
	afterCount := len(env.messenger.messages)
	env.messenger.mu.Unlock()
	if afterCount == 0 {
		t.Error("shutdown should flush pending digests")
	}
}

// fakeEvent builds a minimal messaging.Event for testing sync handlers.
func fakeEvent(eventType string, stateKey *string, content map[string]any) messaging.Event {
	return messaging.Event{
		Type:     ref.EventType(eventType),
		StateKey: stateKey,
		Content:  content,
	}
}
