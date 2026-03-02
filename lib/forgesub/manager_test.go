// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package forgesub

import (
	"log/slog"
	"testing"

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

// newTestManager creates a Manager with a discard logger for testing.
func newTestManager() *Manager {
	return NewManager(slog.Default())
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
		Events:   []string{"push", "issues"},
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
		Events:   []string{"push"},
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
		Events:   []string{forge.EventCategoryPush},
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
		Events:   []string{forge.EventCategoryPush}, // only push
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
			Action: string(forge.IssueClosed),
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
			Action: string(forge.IssueClosed),
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
			Action: string(forge.IssueClosed),
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
			Action: string(forge.IssueReopened),
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
		Events: []string{forge.EventCategoryPush},
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
			Events: []string{forge.EventCategoryPush},
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
