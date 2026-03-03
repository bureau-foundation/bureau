// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package forgesub

import (
	"log/slog"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/forge"
)

// testRoomID creates a ref.RoomID for testing. Uses a deterministic
// format that matches the ref package's room ID pattern.
func testRoomID(name string) ref.RoomID {
	roomID, err := ref.ParseRoomID("!" + name + ":test.local")
	if err != nil {
		panic("bad test room ID: " + err.Error())
	}
	return roomID
}

// newTestManager creates a Manager with a real clock and default
// logger for testing.
func newTestManager() *Manager {
	return NewManager(clock.Real(), slog.Default())
}

// newTestSubscriber creates a Subscriber with a done channel for test
// lifecycle control.
func newTestSubscriber() (*Subscriber, chan struct{}) {
	done := make(chan struct{})
	return &Subscriber{
		Channel: make(chan SubscribeEvent, SubscriberChannelSize),
		Done:    done,
	}, done
}

// drainCaughtUp reads and discards the initial FrameCaughtUp event
// from the subscriber's channel. Fails the test if no caught_up
// arrives.
func drainCaughtUp(t *testing.T, subscriber *Subscriber) {
	t.Helper()
	select {
	case event := <-subscriber.Channel:
		if event.Frame.Type != forge.FrameCaughtUp {
			t.Fatalf("expected caught_up, got %s", event.Frame.Type)
		}
	default:
		t.Fatal("expected caught_up frame in channel")
	}
}

// receiveEvent reads one event from the subscriber's channel. Fails
// the test if no event is available.
func receiveEvent(t *testing.T, subscriber *Subscriber) SubscribeEvent {
	t.Helper()
	select {
	case event := <-subscriber.Channel:
		return event
	default:
		t.Fatal("expected event in channel")
		return SubscribeEvent{}
	}
}

// assertEmpty verifies the subscriber's channel is empty.
func assertEmpty(t *testing.T, subscriber *Subscriber) {
	t.Helper()
	select {
	case event := <-subscriber.Channel:
		t.Fatalf("expected empty channel, got frame type %s", event.Frame.Type)
	default:
	}
}

// --- Config ingestion tests ---

func TestUpdateRoomBinding(t *testing.T) {
	manager := newTestManager()
	roomID := testRoomID("room1")

	manager.UpdateRoomBinding(roomID, forge.RepositoryBinding{
		Provider: "github",
		Owner:    "octocat",
		Repo:     "hello",
	})

	// Verify binding is tracked.
	key := repoKey{Provider: "github", Repo: "octocat/hello"}
	manager.mu.RLock()
	defer manager.mu.RUnlock()

	rooms := manager.repoToRooms[key]
	if rooms == nil || rooms[roomID] == nil {
		t.Fatal("binding not tracked in repoToRooms")
	}

	repos := manager.roomToRepos[roomID]
	if _, exists := repos[key]; !exists {
		t.Fatal("binding not tracked in roomToRepos")
	}
}

func TestRemoveRoomBinding(t *testing.T) {
	manager := newTestManager()
	roomID := testRoomID("room1")

	manager.UpdateRoomBinding(roomID, forge.RepositoryBinding{
		Provider: "github",
		Owner:    "octocat",
		Repo:     "hello",
	})
	manager.RemoveRoomBinding(roomID, "github", "octocat/hello")

	key := repoKey{Provider: "github", Repo: "octocat/hello"}
	manager.mu.RLock()
	defer manager.mu.RUnlock()

	if rooms := manager.repoToRooms[key]; len(rooms) > 0 {
		t.Error("binding should be removed from repoToRooms")
	}
	if repos := manager.roomToRepos[roomID]; len(repos) > 0 {
		t.Error("binding should be removed from roomToRepos")
	}
}

func TestUpdateForgeConfig_AttachesToBinding(t *testing.T) {
	manager := newTestManager()
	roomID := testRoomID("room1")

	// Add binding first, then config.
	manager.UpdateRoomBinding(roomID, forge.RepositoryBinding{
		Provider: "github",
		Owner:    "octocat",
		Repo:     "hello",
	})
	manager.UpdateForgeConfig(roomID, forge.ForgeConfig{
		Provider: "github",
		Repo:     "octocat/hello",
		Events:   []forge.EventCategory{forge.EventCategoryPush, forge.EventCategoryIssues},
	})

	key := repoKey{Provider: "github", Repo: "octocat/hello"}
	manager.mu.RLock()
	defer manager.mu.RUnlock()

	config := manager.repoToRooms[key][roomID].Config
	if config == nil {
		t.Fatal("config should be attached to binding")
	}
	if len(config.Events) != 2 {
		t.Errorf("config.Events = %v, want 2 entries", config.Events)
	}
}

func TestUpdateForgeConfig_StoredBeforeBinding(t *testing.T) {
	manager := newTestManager()
	roomID := testRoomID("room1")

	// Config arrives before binding.
	manager.UpdateForgeConfig(roomID, forge.ForgeConfig{
		Provider: "github",
		Repo:     "octocat/hello",
		Events:   []forge.EventCategory{forge.EventCategoryPush},
	})

	// Now add the binding.
	manager.UpdateRoomBinding(roomID, forge.RepositoryBinding{
		Provider: "github",
		Owner:    "octocat",
		Repo:     "hello",
	})

	// Config should still be there.
	key := repoKey{Provider: "github", Repo: "octocat/hello"}
	manager.mu.RLock()
	defer manager.mu.RUnlock()

	config := manager.repoToRooms[key][roomID].Config
	if config == nil {
		t.Fatal("pre-binding config should be preserved after binding update")
	}
}

// --- Subscription registration tests ---

func TestAddRoomSubscriber_RequiresBinding(t *testing.T) {
	manager := newTestManager()
	roomID := testRoomID("room1")

	subscriber, _ := newTestSubscriber()
	subscription := &RoomSubscription{
		Subscriber: subscriber,
		RoomID:     roomID,
	}

	err := manager.AddRoomSubscriber(subscription)
	if err == nil {
		t.Fatal("expected error for room with no bindings")
	}
}

func TestAddRoomSubscriber_SendsCaughtUp(t *testing.T) {
	manager := newTestManager()
	roomID := testRoomID("room1")

	manager.UpdateRoomBinding(roomID, forge.RepositoryBinding{
		Provider: "github", Owner: "o", Repo: "r",
	})

	subscriber, _ := newTestSubscriber()
	subscription := &RoomSubscription{
		Subscriber: subscriber,
		RoomID:     roomID,
	}

	if err := manager.AddRoomSubscriber(subscription); err != nil {
		t.Fatalf("AddRoomSubscriber: %v", err)
	}

	drainCaughtUp(t, subscriber)
}

func TestAddEntitySubscriber_Validation(t *testing.T) {
	manager := newTestManager()

	subscriber, _ := newTestSubscriber()

	// Missing provider.
	err := manager.AddEntitySubscriber(&EntitySubscription{
		Subscriber: subscriber,
		Entity:     forge.EntityRef{Repo: "o/r", EntityType: forge.EntityTypeIssue, Number: 1},
	})
	if err == nil {
		t.Error("expected error for missing provider")
	}

	// Missing repo.
	err = manager.AddEntitySubscriber(&EntitySubscription{
		Subscriber: subscriber,
		Entity:     forge.EntityRef{Provider: "github", EntityType: forge.EntityTypeIssue, Number: 1},
	})
	if err == nil {
		t.Error("expected error for missing repo")
	}

	// Missing entity type.
	err = manager.AddEntitySubscriber(&EntitySubscription{
		Subscriber: subscriber,
		Entity:     forge.EntityRef{Provider: "github", Repo: "o/r", Number: 1},
	})
	if err == nil {
		t.Error("expected error for missing entity type")
	}
}

func TestAddEntitySubscriber_SendsCaughtUp(t *testing.T) {
	manager := newTestManager()

	subscriber, _ := newTestSubscriber()
	subscription := &EntitySubscription{
		Subscriber: subscriber,
		Entity: forge.EntityRef{
			Provider: "github", Repo: "o/r",
			EntityType: forge.EntityTypeIssue, Number: 42,
		},
	}

	if err := manager.AddEntitySubscriber(subscription); err != nil {
		t.Fatalf("AddEntitySubscriber: %v", err)
	}

	drainCaughtUp(t, subscriber)
}

// --- Dispatch tests ---

func TestDispatch_RoomSubscriber_ReceivesMatchingEvent(t *testing.T) {
	manager := newTestManager()
	roomID := testRoomID("room1")

	manager.UpdateRoomBinding(roomID, forge.RepositoryBinding{
		Provider: "github", Owner: "octocat", Repo: "hello",
	})
	manager.UpdateForgeConfig(roomID, forge.ForgeConfig{
		Provider: "github",
		Repo:     "octocat/hello",
		Events:   []forge.EventCategory{forge.EventCategoryPush},
	})

	subscriber, _ := newTestSubscriber()
	subscription := &RoomSubscription{Subscriber: subscriber, RoomID: roomID}
	if err := manager.AddRoomSubscriber(subscription); err != nil {
		t.Fatalf("AddRoomSubscriber: %v", err)
	}
	drainCaughtUp(t, subscriber)

	manager.Dispatch(&forge.Event{
		Type: forge.EventCategoryPush,
		Push: &forge.PushEvent{Provider: "github", Repo: "octocat/hello"},
	})

	event := receiveEvent(t, subscriber)
	if event.Frame.Type != forge.FrameEvent {
		t.Errorf("frame type = %s, want event", event.Frame.Type)
	}
	if event.Frame.Event == nil || event.Frame.Event.Type != forge.EventCategoryPush {
		t.Error("event should be a push event")
	}
}

func TestDispatch_RoomSubscriber_FilteredByCategory(t *testing.T) {
	manager := newTestManager()
	roomID := testRoomID("room1")

	manager.UpdateRoomBinding(roomID, forge.RepositoryBinding{
		Provider: "github", Owner: "o", Repo: "r",
	})
	manager.UpdateForgeConfig(roomID, forge.ForgeConfig{
		Provider: "github",
		Repo:     "o/r",
		Events:   []forge.EventCategory{forge.EventCategoryPush}, // only push
	})

	subscriber, _ := newTestSubscriber()
	subscription := &RoomSubscription{Subscriber: subscriber, RoomID: roomID}
	if err := manager.AddRoomSubscriber(subscription); err != nil {
		t.Fatalf("AddRoomSubscriber: %v", err)
	}
	drainCaughtUp(t, subscriber)

	// Dispatch an issue event — should be filtered out.
	manager.Dispatch(&forge.Event{
		Type:  forge.EventCategoryIssues,
		Issue: &forge.IssueEvent{Provider: "github", Repo: "o/r", Action: "opened"},
	})

	assertEmpty(t, subscriber)
}

func TestDispatch_RoomSubscriber_NoConfig(t *testing.T) {
	manager := newTestManager()
	roomID := testRoomID("room1")

	// Binding but no ForgeConfig — should reject all events
	// (nil config = default deny).
	manager.UpdateRoomBinding(roomID, forge.RepositoryBinding{
		Provider: "github", Owner: "o", Repo: "r",
	})

	subscriber, _ := newTestSubscriber()
	subscription := &RoomSubscription{Subscriber: subscriber, RoomID: roomID}
	if err := manager.AddRoomSubscriber(subscription); err != nil {
		t.Fatalf("AddRoomSubscriber: %v", err)
	}
	drainCaughtUp(t, subscriber)

	manager.Dispatch(&forge.Event{
		Type: forge.EventCategoryPush,
		Push: &forge.PushEvent{Provider: "github", Repo: "o/r"},
	})

	assertEmpty(t, subscriber)
}

func TestDispatch_EntitySubscriber_ReceivesMatchingEvent(t *testing.T) {
	manager := newTestManager()

	entity := forge.EntityRef{
		Provider: "github", Repo: "o/r",
		EntityType: forge.EntityTypeIssue, Number: 42,
	}

	subscriber, _ := newTestSubscriber()
	subscription := &EntitySubscription{Subscriber: subscriber, Entity: entity}
	if err := manager.AddEntitySubscriber(subscription); err != nil {
		t.Fatalf("AddEntitySubscriber: %v", err)
	}
	drainCaughtUp(t, subscriber)

	manager.Dispatch(&forge.Event{
		Type: forge.EventCategoryIssues,
		Issue: &forge.IssueEvent{
			Provider: "github", Repo: "o/r", Number: 42, Action: "labeled",
		},
	})

	event := receiveEvent(t, subscriber)
	if event.Frame.Type != forge.FrameEvent {
		t.Errorf("frame type = %s, want event", event.Frame.Type)
	}
}

func TestDispatch_EntitySubscriber_IgnoresNonMatchingEntity(t *testing.T) {
	manager := newTestManager()

	entity := forge.EntityRef{
		Provider: "github", Repo: "o/r",
		EntityType: forge.EntityTypeIssue, Number: 42,
	}

	subscriber, _ := newTestSubscriber()
	subscription := &EntitySubscription{Subscriber: subscriber, Entity: entity}
	if err := manager.AddEntitySubscriber(subscription); err != nil {
		t.Fatalf("AddEntitySubscriber: %v", err)
	}
	drainCaughtUp(t, subscriber)

	// Dispatch event for a different issue number.
	manager.Dispatch(&forge.Event{
		Type: forge.EventCategoryIssues,
		Issue: &forge.IssueEvent{
			Provider: "github", Repo: "o/r", Number: 99, Action: "opened",
		},
	})

	assertEmpty(t, subscriber)
}

func TestDispatch_ReviewTargetsParentPR(t *testing.T) {
	manager := newTestManager()

	// Subscribe to PR #15.
	entity := forge.EntityRef{
		Provider: "github", Repo: "o/r",
		EntityType: forge.EntityTypePullRequest, Number: 15,
	}

	subscriber, _ := newTestSubscriber()
	subscription := &EntitySubscription{Subscriber: subscriber, Entity: entity}
	if err := manager.AddEntitySubscriber(subscription); err != nil {
		t.Fatalf("AddEntitySubscriber: %v", err)
	}
	drainCaughtUp(t, subscriber)

	// Dispatch a review on PR #15.
	manager.Dispatch(&forge.Event{
		Type: forge.EventCategoryReview,
		Review: &forge.ReviewEvent{
			Provider: "github", Repo: "o/r", PRNumber: 15, State: "approved",
		},
	})

	event := receiveEvent(t, subscriber)
	if event.Frame.Type != forge.FrameEvent {
		t.Errorf("frame type = %s, want event", event.Frame.Type)
	}
	if event.Frame.Event.Type != forge.EventCategoryReview {
		t.Error("expected review event")
	}
}

func TestDispatch_PushHasNoEntity(t *testing.T) {
	manager := newTestManager()

	// Entity subscriber should NOT receive push events (push has no entity).
	entity := forge.EntityRef{
		Provider: "github", Repo: "o/r",
		EntityType: forge.EntityTypePullRequest, Number: 1,
	}

	subscriber, _ := newTestSubscriber()
	subscription := &EntitySubscription{Subscriber: subscriber, Entity: entity}
	if err := manager.AddEntitySubscriber(subscription); err != nil {
		t.Fatalf("AddEntitySubscriber: %v", err)
	}
	drainCaughtUp(t, subscriber)

	manager.Dispatch(&forge.Event{
		Type: forge.EventCategoryPush,
		Push: &forge.PushEvent{Provider: "github", Repo: "o/r"},
	})

	assertEmpty(t, subscriber)
}

// --- Entity close tests ---

func TestDispatch_EntityClose_SendsClosedFrame(t *testing.T) {
	manager := newTestManager()

	entity := forge.EntityRef{
		Provider: "github", Repo: "o/r",
		EntityType: forge.EntityTypeIssue, Number: 7,
	}

	subscriber, _ := newTestSubscriber()
	subscription := &EntitySubscription{Subscriber: subscriber, Entity: entity}
	if err := manager.AddEntitySubscriber(subscription); err != nil {
		t.Fatalf("AddEntitySubscriber: %v", err)
	}
	drainCaughtUp(t, subscriber)

	// Close the issue.
	manager.Dispatch(&forge.Event{
		Type: forge.EventCategoryIssues,
		Issue: &forge.IssueEvent{
			Provider: "github", Repo: "o/r", Number: 7,
			Action: forge.IssueClosed,
		},
	})

	// Should receive the event frame and then the entity_closed frame.
	event := receiveEvent(t, subscriber)
	if event.Frame.Type != forge.FrameEvent {
		t.Errorf("first frame type = %s, want event", event.Frame.Type)
	}

	closed := receiveEvent(t, subscriber)
	if closed.Frame.Type != forge.FrameEntityClosed {
		t.Errorf("second frame type = %s, want entity_closed", closed.Frame.Type)
	}
	if closed.Frame.EntityRef == nil {
		t.Fatal("entity_closed frame missing EntityRef")
	}
	if *closed.Frame.EntityRef != entity {
		t.Errorf("EntityRef = %+v, want %+v", *closed.Frame.EntityRef, entity)
	}
}

func TestDispatch_EphemeralSubscription_CleanedOnClose(t *testing.T) {
	manager := newTestManager()

	entity := forge.EntityRef{
		Provider: "github", Repo: "o/r",
		EntityType: forge.EntityTypeIssue, Number: 5,
	}

	subscriber, _ := newTestSubscriber()
	subscription := &EntitySubscription{
		Subscriber: subscriber,
		Entity:     entity,
		Persistent: false, // ephemeral
	}
	if err := manager.AddEntitySubscriber(subscription); err != nil {
		t.Fatalf("AddEntitySubscriber: %v", err)
	}
	drainCaughtUp(t, subscriber)

	if manager.EntitySubscriberCount(entity) != 1 {
		t.Fatal("expected 1 subscriber before close")
	}

	// Close the entity.
	manager.Dispatch(&forge.Event{
		Type: forge.EventCategoryIssues,
		Issue: &forge.IssueEvent{
			Provider: "github", Repo: "o/r", Number: 5,
			Action: forge.IssueClosed,
		},
	})

	// Ephemeral subscription should be cleaned up.
	if count := manager.EntitySubscriberCount(entity); count != 0 {
		t.Errorf("ephemeral subscriber count = %d after close, want 0", count)
	}
}

func TestDispatch_PersistentSubscription_SurvivesClose(t *testing.T) {
	manager := newTestManager()

	entity := forge.EntityRef{
		Provider: "github", Repo: "o/r",
		EntityType: forge.EntityTypeIssue, Number: 5,
	}

	subscriber, _ := newTestSubscriber()
	subscription := &EntitySubscription{
		Subscriber: subscriber,
		Entity:     entity,
		Persistent: true,
	}
	if err := manager.AddEntitySubscriber(subscription); err != nil {
		t.Fatalf("AddEntitySubscriber: %v", err)
	}
	drainCaughtUp(t, subscriber)

	// Close the entity.
	manager.Dispatch(&forge.Event{
		Type: forge.EventCategoryIssues,
		Issue: &forge.IssueEvent{
			Provider: "github", Repo: "o/r", Number: 5,
			Action: forge.IssueClosed,
		},
	})

	// Persistent subscription should survive.
	if count := manager.EntitySubscriberCount(entity); count != 1 {
		t.Errorf("persistent subscriber count = %d after close, want 1", count)
	}

	// Drain the event and entity_closed frames.
	receiveEvent(t, subscriber)
	receiveEvent(t, subscriber)

	// Subsequent events should still reach the subscriber.
	manager.Dispatch(&forge.Event{
		Type: forge.EventCategoryIssues,
		Issue: &forge.IssueEvent{
			Provider: "github", Repo: "o/r", Number: 5,
			Action: forge.IssueReopened,
		},
	})

	event := receiveEvent(t, subscriber)
	if event.Frame.Type != forge.FrameEvent {
		t.Error("persistent subscriber should still receive events after close")
	}
}

// --- Dead subscriber cleanup ---

func TestDispatch_CleansDisconnectedSubscribers(t *testing.T) {
	manager := newTestManager()
	roomID := testRoomID("room1")

	manager.UpdateRoomBinding(roomID, forge.RepositoryBinding{
		Provider: "github", Owner: "o", Repo: "r",
	})
	manager.UpdateForgeConfig(roomID, forge.ForgeConfig{
		Provider: "github", Repo: "o/r",
		Events: []forge.EventCategory{forge.EventCategoryPush},
	})

	// Add two subscribers. Close the first one's done channel.
	subscriber1, done1 := newTestSubscriber()
	sub1 := &RoomSubscription{Subscriber: subscriber1, RoomID: roomID}
	if err := manager.AddRoomSubscriber(sub1); err != nil {
		t.Fatalf("AddRoomSubscriber: %v", err)
	}
	drainCaughtUp(t, subscriber1)

	subscriber2, _ := newTestSubscriber()
	sub2 := &RoomSubscription{Subscriber: subscriber2, RoomID: roomID}
	if err := manager.AddRoomSubscriber(sub2); err != nil {
		t.Fatalf("AddRoomSubscriber: %v", err)
	}
	drainCaughtUp(t, subscriber2)

	// Disconnect subscriber 1.
	close(done1)

	// Dispatch triggers cleanup.
	manager.Dispatch(&forge.Event{
		Type: forge.EventCategoryPush,
		Push: &forge.PushEvent{Provider: "github", Repo: "o/r"},
	})

	// Subscriber 2 should receive the event.
	event := receiveEvent(t, subscriber2)
	if event.Frame.Type != forge.FrameEvent {
		t.Error("subscriber2 should receive the event")
	}

	// Subscriber 1 should be cleaned up.
	if count := manager.RoomSubscriberCount(roomID); count != 1 {
		t.Errorf("subscriber count = %d, want 1 (dead subscriber should be removed)", count)
	}
}

// --- Multi-room dispatch ---

func TestDispatch_MultipleRoomsForSameRepo(t *testing.T) {
	manager := newTestManager()
	room1 := testRoomID("room1")
	room2 := testRoomID("room2")

	// Both rooms bind the same repo.
	for _, roomID := range []ref.RoomID{room1, room2} {
		manager.UpdateRoomBinding(roomID, forge.RepositoryBinding{
			Provider: "github", Owner: "o", Repo: "r",
		})
		manager.UpdateForgeConfig(roomID, forge.ForgeConfig{
			Provider: "github", Repo: "o/r",
			Events: []forge.EventCategory{forge.EventCategoryPush},
		})
	}

	subscriber1, _ := newTestSubscriber()
	sub1 := &RoomSubscription{Subscriber: subscriber1, RoomID: room1}
	if err := manager.AddRoomSubscriber(sub1); err != nil {
		t.Fatalf("AddRoomSubscriber: %v", err)
	}
	drainCaughtUp(t, subscriber1)

	subscriber2, _ := newTestSubscriber()
	sub2 := &RoomSubscription{Subscriber: subscriber2, RoomID: room2}
	if err := manager.AddRoomSubscriber(sub2); err != nil {
		t.Fatalf("AddRoomSubscriber: %v", err)
	}
	drainCaughtUp(t, subscriber2)

	manager.Dispatch(&forge.Event{
		Type: forge.EventCategoryPush,
		Push: &forge.PushEvent{Provider: "github", Repo: "o/r"},
	})

	// Both subscribers should receive the event.
	receiveEvent(t, subscriber1)
	receiveEvent(t, subscriber2)
}

// --- Remove subscriber tests ---

func TestRemoveRoomSubscriber(t *testing.T) {
	manager := newTestManager()
	roomID := testRoomID("room1")

	manager.UpdateRoomBinding(roomID, forge.RepositoryBinding{
		Provider: "github", Owner: "o", Repo: "r",
	})

	subscriber, _ := newTestSubscriber()
	subscription := &RoomSubscription{Subscriber: subscriber, RoomID: roomID}
	if err := manager.AddRoomSubscriber(subscription); err != nil {
		t.Fatalf("AddRoomSubscriber: %v", err)
	}

	manager.RemoveRoomSubscriber(subscription)

	if count := manager.RoomSubscriberCount(roomID); count != 0 {
		t.Errorf("subscriber count = %d after removal, want 0", count)
	}
}

func TestRemoveEntitySubscriber(t *testing.T) {
	manager := newTestManager()

	entity := forge.EntityRef{
		Provider: "github", Repo: "o/r",
		EntityType: forge.EntityTypeIssue, Number: 1,
	}

	subscriber, _ := newTestSubscriber()
	subscription := &EntitySubscription{Subscriber: subscriber, Entity: entity}
	if err := manager.AddEntitySubscriber(subscription); err != nil {
		t.Fatalf("AddEntitySubscriber: %v", err)
	}

	manager.RemoveEntitySubscriber(subscription)

	if count := manager.EntitySubscriberCount(entity); count != 0 {
		t.Errorf("subscriber count = %d after removal, want 0", count)
	}
}

// --- Resync test ---

func TestDispatch_ResyncOnOverflow(t *testing.T) {
	manager := newTestManager()

	entity := forge.EntityRef{
		Provider: "github", Repo: "o/r",
		EntityType: forge.EntityTypeIssue, Number: 1,
	}

	// Create subscriber with a tiny buffer to force overflow.
	done := make(chan struct{})
	subscriber := &Subscriber{
		Channel: make(chan SubscribeEvent, 1), // tiny buffer
		Done:    done,
	}
	subscription := &EntitySubscription{Subscriber: subscriber, Entity: entity}
	if err := manager.AddEntitySubscriber(subscription); err != nil {
		t.Fatalf("AddEntitySubscriber: %v", err)
	}
	// caught_up fills the buffer (size 1).

	// Dispatch should overflow the channel and set resync.
	manager.Dispatch(&forge.Event{
		Type: forge.EventCategoryIssues,
		Issue: &forge.IssueEvent{
			Provider: "github", Repo: "o/r", Number: 1, Action: "labeled",
		},
	})

	if !subscriber.Resync.Load() {
		t.Error("expected resync flag to be set after channel overflow")
	}
}

// --- RoomsForEvent tests ---

func TestRoomsForEvent_NoBindings(t *testing.T) {
	manager := newTestManager()

	matches := manager.RoomsForEvent(&forge.Event{
		Type: forge.EventCategoryIssues,
		Issue: &forge.IssueEvent{
			Provider: "github", Repo: "o/r", Number: 1, Action: "opened",
		},
	})

	if len(matches) != 0 {
		t.Fatalf("expected no matches, got %d", len(matches))
	}
}

func TestRoomsForEvent_MatchesBinding(t *testing.T) {
	manager := newTestManager()
	roomID := testRoomID("room1")

	manager.UpdateRoomBinding(roomID, forge.RepositoryBinding{
		Provider: "github",
		Owner:    "octocat",
		Repo:     "hello",
	})

	matches := manager.RoomsForEvent(&forge.Event{
		Type: forge.EventCategoryIssues,
		Issue: &forge.IssueEvent{
			Provider: "github", Repo: "octocat/hello", Number: 1, Action: "opened",
		},
	})

	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
	if matches[0].RoomID != roomID {
		t.Errorf("expected room %s, got %s", roomID, matches[0].RoomID)
	}
	if matches[0].Config != nil {
		t.Error("expected nil config (no ForgeConfig event yet)")
	}
}

func TestRoomsForEvent_IncludesConfig(t *testing.T) {
	manager := newTestManager()
	roomID := testRoomID("room1")

	manager.UpdateRoomBinding(roomID, forge.RepositoryBinding{
		Provider: "github",
		Owner:    "octocat",
		Repo:     "hello",
	})
	manager.UpdateForgeConfig(roomID, forge.ForgeConfig{
		Version:   1,
		Provider:  "github",
		Repo:      "octocat/hello",
		Events:    []forge.EventCategory{forge.EventCategoryIssues},
		IssueSync: forge.IssueSyncImport,
	})

	matches := manager.RoomsForEvent(&forge.Event{
		Type: forge.EventCategoryIssues,
		Issue: &forge.IssueEvent{
			Provider: "github", Repo: "octocat/hello", Number: 1, Action: "opened",
		},
	})

	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
	if matches[0].Config == nil {
		t.Fatal("expected non-nil config")
	}
	if matches[0].Config.IssueSync != forge.IssueSyncImport {
		t.Errorf("expected IssueSync=%s, got %s", forge.IssueSyncImport, matches[0].Config.IssueSync)
	}
}

func TestRoomsForEvent_MultipleRooms(t *testing.T) {
	manager := newTestManager()
	room1 := testRoomID("room1")
	room2 := testRoomID("room2")

	manager.UpdateRoomBinding(room1, forge.RepositoryBinding{
		Provider: "github",
		Owner:    "octocat",
		Repo:     "hello",
	})
	manager.UpdateRoomBinding(room2, forge.RepositoryBinding{
		Provider: "github",
		Owner:    "octocat",
		Repo:     "hello",
	})

	matches := manager.RoomsForEvent(&forge.Event{
		Type: forge.EventCategoryIssues,
		Issue: &forge.IssueEvent{
			Provider: "github", Repo: "octocat/hello", Number: 1, Action: "opened",
		},
	})

	if len(matches) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(matches))
	}

	// Both rooms should be present (order not guaranteed).
	found := map[ref.RoomID]bool{}
	for _, match := range matches {
		found[match.RoomID] = true
	}
	if !found[room1] || !found[room2] {
		t.Errorf("expected both rooms, got %v", found)
	}
}

func TestRoomsForEvent_DifferentRepo(t *testing.T) {
	manager := newTestManager()
	roomID := testRoomID("room1")

	manager.UpdateRoomBinding(roomID, forge.RepositoryBinding{
		Provider: "github",
		Owner:    "octocat",
		Repo:     "hello",
	})

	// Event for a different repo should not match.
	matches := manager.RoomsForEvent(&forge.Event{
		Type: forge.EventCategoryIssues,
		Issue: &forge.IssueEvent{
			Provider: "github", Repo: "octocat/other", Number: 1, Action: "opened",
		},
	})

	if len(matches) != 0 {
		t.Fatalf("expected no matches for different repo, got %d", len(matches))
	}
}

func TestRoomsForEvent_InvalidEvent(t *testing.T) {
	manager := newTestManager()

	// Event with no provider/repo should return nil.
	matches := manager.RoomsForEvent(&forge.Event{Type: "unknown"})
	if matches != nil {
		t.Fatalf("expected nil for invalid event, got %v", matches)
	}
}

// --- Identity and auto-subscribe tests ---

func TestUpdateIdentity(t *testing.T) {
	manager := newTestManager()

	manager.UpdateIdentity(forge.ForgeIdentity{
		Version:    1,
		Provider:   "forgejo",
		MatrixUser: "@bureau/fleet/prod/agent/reviewer:bureau.local",
		ForgeUser:  "reviewer-bot",
	})

	// Verify the reverse lookup works via ProcessAutoSubscribe.
	// Set up a room with auto-subscribe enabled.
	roomID := testRoomID("project")
	manager.UpdateRoomBinding(roomID, forge.RepositoryBinding{
		Version:    1,
		Provider:   "forgejo",
		Owner:      "org",
		Repo:       "project",
		URL:        "https://forgejo.local/org/project",
		CloneHTTPS: "https://forgejo.local/org/project.git",
	})
	manager.UpdateForgeConfig(roomID, forge.ForgeConfig{
		Version:       1,
		Provider:      "forgejo",
		Repo:          "org/project",
		Events:        []forge.EventCategory{forge.EventCategoryPullRequest},
		AutoSubscribe: true,
	})

	event := &forge.Event{
		Type: forge.EventCategoryPullRequest,
		PullRequest: &forge.PullRequestEvent{
			Provider:          "forgejo",
			Repo:              "org/project",
			Number:            7,
			Action:            forge.PullRequestReviewRequested,
			Title:             "Add feature",
			Author:            "dev-bot",
			RequestedReviewer: "reviewer-bot",
		},
	}

	manager.ProcessAutoSubscribe(event, []InvolvedUser{
		{ForgeUsername: "reviewer-bot", Role: RoleReviewRequested},
	})

	if manager.PendingAutoSubscriptionCount("bureau/fleet/prod/agent/reviewer") != 1 {
		t.Fatal("expected 1 pending auto-subscription")
	}
}

func TestUpdateIdentity_SharedAccountSkipped(t *testing.T) {
	manager := newTestManager()

	// GitHub shared-account identities have empty ForgeUser —
	// no reverse lookup entry should be created.
	manager.UpdateIdentity(forge.ForgeIdentity{
		Version:    1,
		Provider:   "github",
		MatrixUser: "@bureau/fleet/prod/agent/coder:bureau.local",
		ForgeUser:  "", // shared account
	})

	// Verify no identity entry exists.
	manager.mu.RLock()
	count := len(manager.identities)
	manager.mu.RUnlock()

	if count != 0 {
		t.Fatalf("expected 0 identity entries for shared-account, got %d", count)
	}
}

func TestProcessAutoSubscribe_AssignRole(t *testing.T) {
	manager := newTestManager()

	manager.UpdateIdentity(forge.ForgeIdentity{
		Version:    1,
		Provider:   "forgejo",
		MatrixUser: "@bureau/fleet/prod/agent/handler:bureau.local",
		ForgeUser:  "handler-bot",
	})

	roomID := testRoomID("bugs")
	manager.UpdateRoomBinding(roomID, forge.RepositoryBinding{
		Version:    1,
		Provider:   "forgejo",
		Owner:      "org",
		Repo:       "bugs",
		URL:        "https://forgejo.local/org/bugs",
		CloneHTTPS: "https://forgejo.local/org/bugs.git",
	})
	manager.UpdateForgeConfig(roomID, forge.ForgeConfig{
		Version:       1,
		Provider:      "forgejo",
		Repo:          "org/bugs",
		Events:        []forge.EventCategory{forge.EventCategoryIssues},
		AutoSubscribe: true,
	})

	event := &forge.Event{
		Type: forge.EventCategoryIssues,
		Issue: &forge.IssueEvent{
			Provider: "forgejo",
			Repo:     "org/bugs",
			Number:   42,
			Action:   forge.IssueAssigned,
			Title:    "Fix the thing",
			Author:   "reporter",
			Assignee: "handler-bot",
		},
	}

	manager.ProcessAutoSubscribe(event, []InvolvedUser{
		{ForgeUsername: "handler-bot", Role: RoleAssignee},
	})

	if manager.PendingAutoSubscriptionCount("bureau/fleet/prod/agent/handler") != 1 {
		t.Fatal("expected 1 pending auto-subscription for assignee")
	}
}

func TestProcessAutoSubscribe_RuleDisabled(t *testing.T) {
	manager := newTestManager()

	manager.UpdateIdentity(forge.ForgeIdentity{
		Version:    1,
		Provider:   "forgejo",
		MatrixUser: "@bureau/fleet/prod/agent/manual:bureau.local",
		ForgeUser:  "manual-bot",
	})

	// Agent has on_assign disabled.
	manager.UpdateAutoSubscribeRules("bureau/fleet/prod/agent/manual", forge.ForgeAutoSubscribeRules{
		Version:         1,
		OnAuthor:        true,
		OnAssign:        false,
		OnMention:       true,
		OnReviewRequest: true,
	})

	roomID := testRoomID("repo")
	manager.UpdateRoomBinding(roomID, forge.RepositoryBinding{
		Version:    1,
		Provider:   "forgejo",
		Owner:      "org",
		Repo:       "repo",
		URL:        "https://forgejo.local/org/repo",
		CloneHTTPS: "https://forgejo.local/org/repo.git",
	})
	manager.UpdateForgeConfig(roomID, forge.ForgeConfig{
		Version:       1,
		Provider:      "forgejo",
		Repo:          "org/repo",
		Events:        []forge.EventCategory{forge.EventCategoryIssues},
		AutoSubscribe: true,
	})

	event := &forge.Event{
		Type: forge.EventCategoryIssues,
		Issue: &forge.IssueEvent{
			Provider: "forgejo",
			Repo:     "org/repo",
			Number:   10,
			Action:   forge.IssueAssigned,
			Title:    "Task",
			Author:   "someone",
			Assignee: "manual-bot",
		},
	}

	manager.ProcessAutoSubscribe(event, []InvolvedUser{
		{ForgeUsername: "manual-bot", Role: RoleAssignee},
	})

	if manager.PendingAutoSubscriptionCount("bureau/fleet/prod/agent/manual") != 0 {
		t.Fatal("expected no pending auto-subscription when on_assign is disabled")
	}
}

func TestProcessAutoSubscribe_AutoSubscribeDisabledInConfig(t *testing.T) {
	manager := newTestManager()

	manager.UpdateIdentity(forge.ForgeIdentity{
		Version:    1,
		Provider:   "forgejo",
		MatrixUser: "@bureau/fleet/prod/agent/worker:bureau.local",
		ForgeUser:  "worker-bot",
	})

	roomID := testRoomID("norepo")
	manager.UpdateRoomBinding(roomID, forge.RepositoryBinding{
		Version:    1,
		Provider:   "forgejo",
		Owner:      "org",
		Repo:       "norepo",
		URL:        "https://forgejo.local/org/norepo",
		CloneHTTPS: "https://forgejo.local/org/norepo.git",
	})
	manager.UpdateForgeConfig(roomID, forge.ForgeConfig{
		Version:       1,
		Provider:      "forgejo",
		Repo:          "org/norepo",
		Events:        []forge.EventCategory{forge.EventCategoryIssues},
		AutoSubscribe: false, // disabled
	})

	event := &forge.Event{
		Type: forge.EventCategoryIssues,
		Issue: &forge.IssueEvent{
			Provider: "forgejo",
			Repo:     "org/norepo",
			Number:   1,
			Action:   forge.IssueAssigned,
			Title:    "Task",
			Author:   "someone",
			Assignee: "worker-bot",
		},
	}

	manager.ProcessAutoSubscribe(event, []InvolvedUser{
		{ForgeUsername: "worker-bot", Role: RoleAssignee},
	})

	if manager.PendingAutoSubscriptionCount("bureau/fleet/prod/agent/worker") != 0 {
		t.Fatal("expected no pending auto-subscription when config.AutoSubscribe is false")
	}
}

func TestClaimAutoSubscriptions(t *testing.T) {
	manager := newTestManager()

	manager.UpdateIdentity(forge.ForgeIdentity{
		Version:    1,
		Provider:   "forgejo",
		MatrixUser: "@bureau/fleet/prod/agent/dev:bureau.local",
		ForgeUser:  "dev-bot",
	})

	roomID := testRoomID("app")
	manager.UpdateRoomBinding(roomID, forge.RepositoryBinding{
		Version:    1,
		Provider:   "forgejo",
		Owner:      "org",
		Repo:       "app",
		URL:        "https://forgejo.local/org/app",
		CloneHTTPS: "https://forgejo.local/org/app.git",
	})
	manager.UpdateForgeConfig(roomID, forge.ForgeConfig{
		Version:       1,
		Provider:      "forgejo",
		Repo:          "org/app",
		Events:        []forge.EventCategory{forge.EventCategoryPullRequest, forge.EventCategoryIssues},
		AutoSubscribe: true,
	})

	// Create two pending auto-subscriptions.
	prEvent := &forge.Event{
		Type: forge.EventCategoryPullRequest,
		PullRequest: &forge.PullRequestEvent{
			Provider:          "forgejo",
			Repo:              "org/app",
			Number:            5,
			Action:            forge.PullRequestReviewRequested,
			Title:             "PR five",
			Author:            "other",
			RequestedReviewer: "dev-bot",
		},
	}
	issueEvent := &forge.Event{
		Type: forge.EventCategoryIssues,
		Issue: &forge.IssueEvent{
			Provider: "forgejo",
			Repo:     "org/app",
			Number:   20,
			Action:   forge.IssueAssigned,
			Title:    "Issue twenty",
			Author:   "other",
			Assignee: "dev-bot",
		},
	}

	manager.ProcessAutoSubscribe(prEvent, []InvolvedUser{
		{ForgeUsername: "dev-bot", Role: RoleReviewRequested},
	})
	manager.ProcessAutoSubscribe(issueEvent, []InvolvedUser{
		{ForgeUsername: "dev-bot", Role: RoleAssignee},
	})

	if manager.PendingAutoSubscriptionCount("bureau/fleet/prod/agent/dev") != 2 {
		t.Fatalf("expected 2 pending, got %d",
			manager.PendingAutoSubscriptionCount("bureau/fleet/prod/agent/dev"))
	}

	// Claim them.
	claimed := manager.ClaimAutoSubscriptions("bureau/fleet/prod/agent/dev")
	if len(claimed) != 2 {
		t.Fatalf("expected 2 claimed, got %d", len(claimed))
	}

	// After claiming, pending should be empty.
	if manager.PendingAutoSubscriptionCount("bureau/fleet/prod/agent/dev") != 0 {
		t.Fatal("expected 0 pending after claim")
	}

	// Claiming again returns nil.
	again := manager.ClaimAutoSubscriptions("bureau/fleet/prod/agent/dev")
	if again != nil {
		t.Fatalf("expected nil on second claim, got %d refs", len(again))
	}
}

func TestProcessAutoSubscribe_NoDuplicates(t *testing.T) {
	manager := newTestManager()

	manager.UpdateIdentity(forge.ForgeIdentity{
		Version:    1,
		Provider:   "forgejo",
		MatrixUser: "@bureau/fleet/prod/agent/dup:bureau.local",
		ForgeUser:  "dup-bot",
	})

	roomID := testRoomID("dedupe")
	manager.UpdateRoomBinding(roomID, forge.RepositoryBinding{
		Version:    1,
		Provider:   "forgejo",
		Owner:      "org",
		Repo:       "dedupe",
		URL:        "https://forgejo.local/org/dedupe",
		CloneHTTPS: "https://forgejo.local/org/dedupe.git",
	})
	manager.UpdateForgeConfig(roomID, forge.ForgeConfig{
		Version:       1,
		Provider:      "forgejo",
		Repo:          "org/dedupe",
		Events:        []forge.EventCategory{forge.EventCategoryIssues},
		AutoSubscribe: true,
	})

	event := &forge.Event{
		Type: forge.EventCategoryIssues,
		Issue: &forge.IssueEvent{
			Provider: "forgejo",
			Repo:     "org/dedupe",
			Number:   1,
			Action:   forge.IssueAssigned,
			Title:    "Dup test",
			Author:   "someone",
			Assignee: "dup-bot",
		},
	}

	// Process the same event twice.
	manager.ProcessAutoSubscribe(event, []InvolvedUser{
		{ForgeUsername: "dup-bot", Role: RoleAssignee},
	})
	manager.ProcessAutoSubscribe(event, []InvolvedUser{
		{ForgeUsername: "dup-bot", Role: RoleAssignee},
	})

	if manager.PendingAutoSubscriptionCount("bureau/fleet/prod/agent/dup") != 1 {
		t.Fatalf("expected 1 pending (no duplicates), got %d",
			manager.PendingAutoSubscriptionCount("bureau/fleet/prod/agent/dup"))
	}
}

func TestEntityClose_CleansPendingAutoSubscriptions(t *testing.T) {
	manager := newTestManager()

	manager.UpdateIdentity(forge.ForgeIdentity{
		Version:    1,
		Provider:   "forgejo",
		MatrixUser: "@bureau/fleet/prod/agent/cleaner:bureau.local",
		ForgeUser:  "cleaner-bot",
	})

	roomID := testRoomID("lifecycle")
	manager.UpdateRoomBinding(roomID, forge.RepositoryBinding{
		Version:    1,
		Provider:   "forgejo",
		Owner:      "org",
		Repo:       "lifecycle",
		URL:        "https://forgejo.local/org/lifecycle",
		CloneHTTPS: "https://forgejo.local/org/lifecycle.git",
	})
	manager.UpdateForgeConfig(roomID, forge.ForgeConfig{
		Version:       1,
		Provider:      "forgejo",
		Repo:          "org/lifecycle",
		Events:        []forge.EventCategory{forge.EventCategoryIssues},
		AutoSubscribe: true,
	})

	// Create a pending auto-subscription.
	assignEvent := &forge.Event{
		Type: forge.EventCategoryIssues,
		Issue: &forge.IssueEvent{
			Provider: "forgejo",
			Repo:     "org/lifecycle",
			Number:   99,
			Action:   forge.IssueAssigned,
			Title:    "Will be closed",
			Author:   "someone",
			Assignee: "cleaner-bot",
		},
	}
	manager.ProcessAutoSubscribe(assignEvent, []InvolvedUser{
		{ForgeUsername: "cleaner-bot", Role: RoleAssignee},
	})

	if manager.PendingAutoSubscriptionCount("bureau/fleet/prod/agent/cleaner") != 1 {
		t.Fatal("expected 1 pending before close")
	}

	// Dispatch a close event for the same entity.
	closeEvent := &forge.Event{
		Type: forge.EventCategoryIssues,
		Issue: &forge.IssueEvent{
			Provider: "forgejo",
			Repo:     "org/lifecycle",
			Number:   99,
			Action:   forge.IssueClosed,
			Title:    "Will be closed",
			Author:   "someone",
		},
	}
	manager.Dispatch(closeEvent)

	// Pending auto-subscription should be cleaned up.
	if manager.PendingAutoSubscriptionCount("bureau/fleet/prod/agent/cleaner") != 0 {
		t.Fatal("expected 0 pending after entity close")
	}
}

func TestExtractLocalpart(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"@bureau/fleet/prod/agent/coder:bureau.local", "bureau/fleet/prod/agent/coder"},
		{"@simple:server.com", "simple"},
		{"invalid", ""},
		{"@:server.com", ""},
		{"@noserver", ""},
		{"", ""},
	}

	for _, test := range tests {
		result := extractLocalpart(test.input)
		if result != test.expected {
			t.Errorf("extractLocalpart(%q) = %q, want %q", test.input, result, test.expected)
		}
	}
}

// --- Attribution tracking tests ---

func TestRecordAndLookupAttribution(t *testing.T) {
	manager := newTestManager()

	manager.RecordAttribution("github", "org/repo", "pull_request", 42, "", "bureau/fleet/prod/agent/coder")

	agent, found := manager.LookupAttribution("github", "org/repo", "pull_request", 42, "")
	if !found {
		t.Fatal("expected attribution record to be found")
	}
	if agent != "bureau/fleet/prod/agent/coder" {
		t.Fatalf("expected agent bureau/fleet/prod/agent/coder, got %q", agent)
	}
}

func TestLookupAttribution_NotFound(t *testing.T) {
	manager := newTestManager()

	_, found := manager.LookupAttribution("github", "org/repo", "pull_request", 99, "")
	if found {
		t.Fatal("expected no attribution record")
	}
}

func TestLookupAttribution_CommitSHA(t *testing.T) {
	manager := newTestManager()

	manager.RecordAttribution("github", "org/repo", "commit", 0, "abc123def456", "bureau/fleet/prod/agent/coder")

	agent, found := manager.LookupAttribution("github", "org/repo", "commit", 0, "abc123def456")
	if !found {
		t.Fatal("expected attribution record for commit SHA")
	}
	if agent != "bureau/fleet/prod/agent/coder" {
		t.Fatalf("expected agent bureau/fleet/prod/agent/coder, got %q", agent)
	}
}

func TestAttribution_TTLExpiry(t *testing.T) {
	fakeClock := clock.Fake(time.Unix(1735689600, 0))
	manager := NewManager(fakeClock, slog.Default())

	manager.RecordAttribution("github", "org/repo", "pull_request", 10, "", "bureau/fleet/prod/agent/old")

	// Before expiry: should be found.
	agent, found := manager.LookupAttribution("github", "org/repo", "pull_request", 10, "")
	if !found || agent != "bureau/fleet/prod/agent/old" {
		t.Fatalf("expected attribution before expiry, got found=%v agent=%q", found, agent)
	}

	// Advance past TTL.
	fakeClock.Advance(attributionTTL + time.Second)

	// After expiry: should not be found.
	_, found = manager.LookupAttribution("github", "org/repo", "pull_request", 10, "")
	if found {
		t.Fatal("expected attribution to expire after TTL")
	}
}

func TestAttribution_LazyCleanup(t *testing.T) {
	fakeClock := clock.Fake(time.Unix(1735689600, 0))
	manager := NewManager(fakeClock, slog.Default())

	// Record two attributions.
	manager.RecordAttribution("github", "org/repo", "pull_request", 1, "", "agent-a")
	manager.RecordAttribution("github", "org/repo", "pull_request", 2, "", "agent-b")

	// Advance past TTL.
	fakeClock.Advance(attributionTTL + time.Second)

	// Recording a new attribution triggers lazy cleanup.
	manager.RecordAttribution("github", "org/repo", "pull_request", 3, "", "agent-c")

	// Old records should be cleaned.
	manager.mu.RLock()
	count := len(manager.attributions)
	manager.mu.RUnlock()

	if count != 1 {
		t.Fatalf("expected 1 attribution after cleanup, got %d", count)
	}

	// The surviving record should be the new one.
	agent, found := manager.LookupAttribution("github", "org/repo", "pull_request", 3, "")
	if !found || agent != "agent-c" {
		t.Fatalf("expected agent-c, got found=%v agent=%q", found, agent)
	}
}
