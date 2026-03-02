// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package forgesub

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/forge"
)

// repoKey uniquely identifies a repository on a forge provider. Used
// as a map key for the repo-to-rooms index.
type repoKey struct {
	Provider string // "github", "forgejo", "gitlab"
	Repo     string // "owner/repo"
}

// identityKey maps a forge provider + username to the identity lookup
// table. Used for reverse lookups during auto-subscribe: given a
// forge username from a webhook, find the Bureau entity.
type identityKey struct {
	Provider      string
	ForgeUsername string
}

// roomConfig holds the per-room, per-repo configuration ingested from
// /sync state events.
type roomConfig struct {
	Binding forge.RepositoryBinding
	Config  *forge.ForgeConfig // nil until a ForgeConfig event is received
}

// Manager manages forge event subscriptions for a single forge
// connector instance. Thread-safe: Dispatch is called from the
// webhook goroutine, subscription methods from per-connection
// goroutines, and config update methods from the /sync loop.
type Manager struct {
	mu     sync.RWMutex
	logger *slog.Logger

	// Repo binding index: repo → rooms with that binding.
	repoToRooms map[repoKey]map[ref.RoomID]*roomConfig
	// Inverse index: room → repos bound to it (for cleanup).
	roomToRepos map[ref.RoomID]map[repoKey]struct{}

	// Subscriber registries.
	roomSubscribers   map[ref.RoomID][]*RoomSubscription
	entitySubscribers map[forge.EntityRef][]*EntitySubscription

	// Identity reverse lookup: forge username → Bureau entity
	// localpart. Updated from m.bureau.forge_identity state events.
	// For shared-account forges (GitHub App), forge_user is empty
	// and no entry is created — on_author requires proxy attribution.
	identities map[identityKey]string

	// Per-agent auto-subscribe rules. Keyed by Bureau entity
	// localpart. Updated from m.bureau.forge_auto_subscribe state
	// events. Agents without explicit rules get all-enabled defaults.
	autoSubscribeRules map[string]forge.ForgeAutoSubscribeRules

	// Pending auto-subscriptions: entity refs waiting for an agent
	// to connect with auto:true. Keyed by Bureau entity localpart.
	// Created by ProcessAutoSubscribe, consumed by
	// ClaimAutoSubscriptions.
	pendingAutoSubscriptions map[string]map[forge.EntityRef]struct{}
}

// NewManager creates a subscription manager for a forge connector.
func NewManager(logger *slog.Logger) *Manager {
	return &Manager{
		logger:                   logger,
		repoToRooms:              make(map[repoKey]map[ref.RoomID]*roomConfig),
		roomToRepos:              make(map[ref.RoomID]map[repoKey]struct{}),
		roomSubscribers:          make(map[ref.RoomID][]*RoomSubscription),
		entitySubscribers:        make(map[forge.EntityRef][]*EntitySubscription),
		identities:               make(map[identityKey]string),
		autoSubscribeRules:       make(map[string]forge.ForgeAutoSubscribeRules),
		pendingAutoSubscriptions: make(map[string]map[forge.EntityRef]struct{}),
	}
}

// --- Config ingestion ---

// UpdateRoomBinding adds or updates a repository binding for a room.
// Called from the /sync handler when m.bureau.repository state events
// are received.
func (m *Manager) UpdateRoomBinding(roomID ref.RoomID, binding forge.RepositoryBinding) {
	key := repoKey{Provider: binding.Provider, Repo: binding.Owner + "/" + binding.Repo}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Ensure the room set exists for this repo.
	rooms, exists := m.repoToRooms[key]
	if !exists {
		rooms = make(map[ref.RoomID]*roomConfig)
		m.repoToRooms[key] = rooms
	}

	// Preserve existing ForgeConfig if we're updating a binding.
	existing := rooms[roomID]
	var config *forge.ForgeConfig
	if existing != nil {
		config = existing.Config
	}

	rooms[roomID] = &roomConfig{
		Binding: binding,
		Config:  config,
	}

	// Update inverse index.
	if _, exists := m.roomToRepos[roomID]; !exists {
		m.roomToRepos[roomID] = make(map[repoKey]struct{})
	}
	m.roomToRepos[roomID][key] = struct{}{}

	m.logger.Debug("room binding updated",
		"room_id", roomID,
		"provider", binding.Provider,
		"repo", binding.Owner+"/"+binding.Repo,
	)
}

// RemoveRoomBinding removes a repository binding for a room. Called
// when a binding state event has empty content (redacted/tombstoned).
// Existing room subscribers stay connected but stop receiving events
// for this repo.
func (m *Manager) RemoveRoomBinding(roomID ref.RoomID, provider, repo string) {
	key := repoKey{Provider: provider, Repo: repo}

	m.mu.Lock()
	defer m.mu.Unlock()

	rooms := m.repoToRooms[key]
	if rooms != nil {
		delete(rooms, roomID)
		if len(rooms) == 0 {
			delete(m.repoToRooms, key)
		}
	}

	repos := m.roomToRepos[roomID]
	if repos != nil {
		delete(repos, key)
		if len(repos) == 0 {
			delete(m.roomToRepos, roomID)
		}
	}

	m.logger.Debug("room binding removed",
		"room_id", roomID,
		"provider", provider,
		"repo", repo,
	)
}

// UpdateForgeConfig sets the per-room, per-repo forge configuration.
// Called from the /sync handler when m.bureau.forge_config state
// events are received. The config's state key matches the binding's
// state key (provider/owner/repo).
//
// If no binding exists yet for this repo in this room, the config is
// stored and takes effect when the binding arrives.
func (m *Manager) UpdateForgeConfig(roomID ref.RoomID, config forge.ForgeConfig) {
	key := repoKey{Provider: config.Provider, Repo: config.Repo}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Try to attach to an existing binding.
	rooms := m.repoToRooms[key]
	if rooms != nil {
		if existing := rooms[roomID]; existing != nil {
			existing.Config = &config
			m.logger.Debug("forge config updated (attached to binding)",
				"room_id", roomID,
				"provider", config.Provider,
				"repo", config.Repo,
			)
			return
		}
	}

	// No binding yet — create a config-only entry so it's ready
	// when the binding arrives.
	if rooms == nil {
		rooms = make(map[ref.RoomID]*roomConfig)
		m.repoToRooms[key] = rooms
	}
	rooms[roomID] = &roomConfig{Config: &config}

	// Update inverse index.
	if _, exists := m.roomToRepos[roomID]; !exists {
		m.roomToRepos[roomID] = make(map[repoKey]struct{})
	}
	m.roomToRepos[roomID][key] = struct{}{}

	m.logger.Debug("forge config stored (no binding yet)",
		"room_id", roomID,
		"provider", config.Provider,
		"repo", config.Repo,
	)
}

// --- Subscription registration ---

// AddRoomSubscriber registers a subscriber for all events in a room
// that pass the room's ForgeConfig filters. Returns an error if the
// room has no repository bindings.
//
// Sends a FrameCaughtUp event to the subscriber's channel before
// returning (under lock), so no events can arrive between
// registration and the caught_up marker.
func (m *Manager) AddRoomSubscriber(subscription *RoomSubscription) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	repos := m.roomToRepos[subscription.RoomID]
	if len(repos) == 0 {
		return fmt.Errorf("room %s has no repository bindings", subscription.RoomID)
	}

	m.roomSubscribers[subscription.RoomID] = append(
		m.roomSubscribers[subscription.RoomID], subscription)

	// Caught_up under the lock ensures atomicity: no events can
	// be dispatched between registration and this marker.
	subscription.Channel <- SubscribeEvent{
		Frame: forge.SubscribeFrame{Type: forge.FrameCaughtUp},
	}

	m.logger.Info("room subscriber added",
		"room_id", subscription.RoomID,
		"total", len(m.roomSubscribers[subscription.RoomID]),
	)

	return nil
}

// AddEntitySubscriber registers a subscriber for events targeting a
// specific forge entity. Returns an error if the entity ref is
// missing required fields.
//
// Sends a FrameCaughtUp event to the subscriber's channel before
// returning (under lock), unless SuppressCaughtUp is set (used when
// auto-subscribe activates entity subscriptions on an existing
// subscribe stream).
func (m *Manager) AddEntitySubscriber(subscription *EntitySubscription) error {
	if subscription.Entity.Provider == "" {
		return fmt.Errorf("entity subscription requires a provider")
	}
	if subscription.Entity.Repo == "" {
		return fmt.Errorf("entity subscription requires a repo")
	}
	if subscription.Entity.EntityType == "" {
		return fmt.Errorf("entity subscription requires an entity type")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.entitySubscribers[subscription.Entity] = append(
		m.entitySubscribers[subscription.Entity], subscription)

	if !subscription.SuppressCaughtUp {
		subscription.Channel <- SubscribeEvent{
			Frame: forge.SubscribeFrame{Type: forge.FrameCaughtUp},
		}
	}

	m.logger.Info("entity subscriber added",
		"entity", subscription.Entity,
		"persistent", subscription.Persistent,
		"total", len(m.entitySubscribers[subscription.Entity]),
	)

	return nil
}

// RemoveRoomSubscriber explicitly removes a room subscriber. The
// Manager also removes subscribers automatically when their Done
// channel is closed (detected during fanout). Explicit removal keeps
// the registries tidy immediately rather than waiting for the next
// dispatch cycle.
func (m *Manager) RemoveRoomSubscriber(subscription *RoomSubscription) {
	m.mu.Lock()
	defer m.mu.Unlock()

	subscribers := m.roomSubscribers[subscription.RoomID]
	for i, existing := range subscribers {
		if existing == subscription {
			m.roomSubscribers[subscription.RoomID] = append(subscribers[:i], subscribers[i+1:]...)
			break
		}
	}
	if len(m.roomSubscribers[subscription.RoomID]) == 0 {
		delete(m.roomSubscribers, subscription.RoomID)
	}
}

// RemoveEntitySubscriber explicitly removes an entity subscriber.
func (m *Manager) RemoveEntitySubscriber(subscription *EntitySubscription) {
	m.mu.Lock()
	defer m.mu.Unlock()

	subscribers := m.entitySubscribers[subscription.Entity]
	for i, existing := range subscribers {
		if existing == subscription {
			m.entitySubscribers[subscription.Entity] = append(subscribers[:i], subscribers[i+1:]...)
			break
		}
	}
	if len(m.entitySubscribers[subscription.Entity]) == 0 {
		delete(m.entitySubscribers, subscription.Entity)
	}
}

// RoomSubscriberCount returns the number of active subscribers for a
// room.
func (m *Manager) RoomSubscriberCount(roomID ref.RoomID) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.roomSubscribers[roomID])
}

// EntitySubscriberCount returns the number of active subscribers for
// an entity.
func (m *Manager) EntitySubscriberCount(entity forge.EntityRef) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.entitySubscribers[entity])
}

// --- Room config queries ---

// RoomMatch pairs a room ID with its forge configuration. Returned by
// RoomsForEvent to let callers (e.g., ticket sync) inspect per-room
// config without reaching into the Manager's internal structures.
type RoomMatch struct {
	RoomID ref.RoomID
	Config *forge.ForgeConfig // nil if no ForgeConfig event received yet
}

// RoomsForEvent returns all rooms that have a repository binding
// matching the event's provider and repo, along with each room's
// ForgeConfig. This is the same lookup that Dispatch uses internally
// for subscriber fanout, exposed as a public query for consumers
// that need to act on events based on per-room config (e.g., issue
// → ticket sync checking IssueSync mode).
//
// Returns nil if the event has no provider/repo or no rooms match.
func (m *Manager) RoomsForEvent(event *forge.Event) []RoomMatch {
	repo := event.Repo()
	provider := event.Provider()
	if repo == "" || provider == "" {
		return nil
	}

	key := repoKey{Provider: provider, Repo: repo}

	m.mu.RLock()
	defer m.mu.RUnlock()

	rooms := m.repoToRooms[key]
	if len(rooms) == 0 {
		return nil
	}

	matches := make([]RoomMatch, 0, len(rooms))
	for roomID, config := range rooms {
		matches = append(matches, RoomMatch{
			RoomID: roomID,
			Config: config.Config,
		})
	}
	return matches
}

// --- Event dispatch ---

// Dispatch routes a forge event to all matching subscribers. Called
// from the webhook handler goroutine after event translation.
//
// The dispatch sequence:
//   - Extract repo key from the event
//   - For each room with a binding for that repo: check ForgeConfig
//     filters, then notify room subscribers via non-blocking send
//   - Extract entity ref (if applicable — push events have none)
//   - Notify entity subscribers for the matching entity ref
//   - If the event closes an entity (issue closed, PR merged, CI
//     completed), send an entity_closed frame and clean up ephemeral
//     subscriptions
//
// Takes a write lock because fanout may remove disconnected
// subscribers (same pattern as ticket service notifySubscribers).
func (m *Manager) Dispatch(event *forge.Event) {
	repo := event.Repo()
	provider := event.Provider()
	if repo == "" || provider == "" {
		m.logger.Warn("dispatch: event missing repo or provider",
			"type", event.Type,
		)
		return
	}

	key := repoKey{Provider: provider, Repo: repo}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Room subscriber dispatch: for each room with a binding for
	// this repo, check config filters and notify subscribers.
	rooms := m.repoToRooms[key]
	for roomID, config := range rooms {
		if !matchesForgeConfig(config.Config, event) {
			continue
		}
		m.notifyRoomSubscribers(roomID, SubscribeEvent{
			Frame: forge.SubscribeFrame{
				Type:  forge.FrameEvent,
				Event: event,
			},
		})
	}

	// Entity subscriber dispatch.
	entityRef, hasEntity := event.EntityRefFromEvent()
	if hasEntity {
		m.notifyEntitySubscribers(entityRef, SubscribeEvent{
			Frame: forge.SubscribeFrame{
				Type:  forge.FrameEvent,
				Event: event,
			},
		})
	}

	// Entity close handling: notify subscribers and clean up
	// ephemeral subscriptions.
	if hasEntity && event.IsEntityClose() {
		m.notifyEntitySubscribers(entityRef, SubscribeEvent{
			Frame: forge.SubscribeFrame{
				Type:      forge.FrameEntityClosed,
				EntityRef: &entityRef,
			},
		})
		m.cleanEphemeralEntitySubscribers(entityRef)
	}
}

// --- Fanout helpers (must be called with m.mu held as write lock) ---

// notifyRoomSubscribers dispatches an event to all subscribers for a
// room. Uses reverse iteration so removals don't shift unvisited
// elements.
func (m *Manager) notifyRoomSubscribers(roomID ref.RoomID, event SubscribeEvent) {
	subscribers := m.roomSubscribers[roomID]
	if len(subscribers) == 0 {
		return
	}

	for i := len(subscribers) - 1; i >= 0; i-- {
		if !trySend(subscribers[i].Subscriber, event) {
			// Subscriber disconnected — remove.
			subscribers = append(subscribers[:i], subscribers[i+1:]...)
		}
	}

	if len(subscribers) == 0 {
		delete(m.roomSubscribers, roomID)
	} else {
		m.roomSubscribers[roomID] = subscribers
	}
}

// notifyEntitySubscribers dispatches an event to all subscribers for
// an entity. Uses reverse iteration for safe removal.
func (m *Manager) notifyEntitySubscribers(entity forge.EntityRef, event SubscribeEvent) {
	subscribers := m.entitySubscribers[entity]
	if len(subscribers) == 0 {
		return
	}

	for i := len(subscribers) - 1; i >= 0; i-- {
		if !trySend(subscribers[i].Subscriber, event) {
			subscribers = append(subscribers[:i], subscribers[i+1:]...)
		}
	}

	if len(subscribers) == 0 {
		delete(m.entitySubscribers, entity)
	} else {
		m.entitySubscribers[entity] = subscribers
	}
}

// cleanEphemeralEntitySubscribers removes all non-persistent entity
// subscriptions and pending auto-subscriptions for the given entity.
// Called after sending entity_closed frames. The subscriber's Done
// channel is NOT closed by the Manager — the subscriber's owner can
// drain remaining events and disconnect gracefully.
func (m *Manager) cleanEphemeralEntitySubscribers(entity forge.EntityRef) {
	// Remove ephemeral (non-persistent) live subscriptions.
	subscribers := m.entitySubscribers[entity]
	if len(subscribers) > 0 {
		kept := subscribers[:0]
		for _, subscription := range subscribers {
			if subscription.Persistent {
				kept = append(kept, subscription)
			}
		}

		if len(kept) == 0 {
			delete(m.entitySubscribers, entity)
		} else {
			m.entitySubscribers[entity] = kept
		}
	}

	// Remove pending auto-subscriptions for this entity.
	for localpart, entities := range m.pendingAutoSubscriptions {
		delete(entities, entity)
		if len(entities) == 0 {
			delete(m.pendingAutoSubscriptions, localpart)
		}
	}
}

// --- Identity and auto-subscribe ---

// UpdateIdentity adds or updates a forge identity mapping. Called
// from the /sync handler when m.bureau.forge_identity state events
// are received. For shared-account forges where ForgeUser is empty,
// no reverse lookup entry is created (on_author requires proxy
// attribution).
func (m *Manager) UpdateIdentity(identity forge.ForgeIdentity) {
	if identity.ForgeUser == "" {
		return
	}

	localpart := extractLocalpart(identity.MatrixUser)
	if localpart == "" {
		m.logger.Warn("identity mapping has invalid matrix_user",
			"matrix_user", identity.MatrixUser,
			"provider", identity.Provider,
		)
		return
	}

	key := identityKey{
		Provider:      identity.Provider,
		ForgeUsername: identity.ForgeUser,
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.identities[key] = localpart

	m.logger.Debug("identity mapping updated",
		"provider", identity.Provider,
		"forge_user", identity.ForgeUser,
		"bureau_entity", localpart,
	)
}

// RemoveIdentity removes an identity mapping. Called when a forge
// identity state event is redacted.
func (m *Manager) RemoveIdentity(provider, forgeUsername string) {
	key := identityKey{Provider: provider, ForgeUsername: forgeUsername}

	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.identities, key)
}

// AutoSubscribeRules returns the auto-subscribe rules for a Bureau
// entity. Returns DefaultAutoSubscribeRules if no explicit rules
// have been set.
func (m *Manager) AutoSubscribeRules(entityLocalpart string) forge.ForgeAutoSubscribeRules {
	m.mu.RLock()
	defer m.mu.RUnlock()

	rules, exists := m.autoSubscribeRules[entityLocalpart]
	if !exists {
		return forge.DefaultAutoSubscribeRules()
	}
	return rules
}

// UpdateAutoSubscribeRules sets the auto-subscribe rules for a Bureau
// entity. Called from the /sync handler when
// m.bureau.forge_auto_subscribe state events are received.
func (m *Manager) UpdateAutoSubscribeRules(entityLocalpart string, rules forge.ForgeAutoSubscribeRules) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.autoSubscribeRules[entityLocalpart] = rules

	m.logger.Debug("auto-subscribe rules updated",
		"entity", entityLocalpart,
		"on_author", rules.OnAuthor,
		"on_assign", rules.OnAssign,
		"on_mention", rules.OnMention,
		"on_review_request", rules.OnReviewRequest,
	)
}

// ProcessAutoSubscribe evaluates auto-subscribe rules for an event's
// involved users. For each user whose identity maps to a Bureau
// entity with matching rules, a pending auto-subscription is created.
//
// The involved users are extracted by the provider-specific connector
// from the webhook payload. The Manager performs the provider-agnostic
// rule evaluation.
//
// Auto-subscribe only fires for events in rooms where ForgeConfig has
// AutoSubscribe enabled. The event must have an entity ref (push
// events have no entity and are skipped).
func (m *Manager) ProcessAutoSubscribe(event *forge.Event, involved []InvolvedUser) {
	if len(involved) == 0 {
		return
	}

	entityRef, hasEntity := event.EntityRefFromEvent()
	if !hasEntity {
		return
	}

	provider := event.Provider()
	repo := event.Repo()
	if provider == "" || repo == "" {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Check that at least one room with this repo has auto-subscribe
	// enabled.
	key := repoKey{Provider: provider, Repo: repo}
	rooms := m.repoToRooms[key]
	autoSubscribeEnabled := false
	for _, config := range rooms {
		if config.Config != nil && config.Config.AutoSubscribe {
			autoSubscribeEnabled = true
			break
		}
	}
	if !autoSubscribeEnabled {
		return
	}

	for _, user := range involved {
		localpart, found := m.identities[identityKey{
			Provider:      provider,
			ForgeUsername: user.ForgeUsername,
		}]
		if !found {
			continue
		}

		rules, hasRules := m.autoSubscribeRules[localpart]
		if !hasRules {
			rules = forge.DefaultAutoSubscribeRules()
		}

		if !roleMatchesRules(user.Role, rules) {
			continue
		}

		// Don't create duplicate pending subscriptions.
		if pending, exists := m.pendingAutoSubscriptions[localpart]; exists {
			if _, already := pending[entityRef]; already {
				continue
			}
		}

		// Also skip if the entity already has a live subscription
		// for this agent (checking all entity subscribers would
		// require an owner field; for now, just record pending and
		// let ClaimAutoSubscriptions deduplicate on activation).

		if m.pendingAutoSubscriptions[localpart] == nil {
			m.pendingAutoSubscriptions[localpart] = make(map[forge.EntityRef]struct{})
		}
		m.pendingAutoSubscriptions[localpart][entityRef] = struct{}{}

		m.logger.Info("auto-subscribe recorded",
			"entity", localpart,
			"role", user.Role,
			"forge_user", user.ForgeUsername,
			"entity_ref", entityRef,
		)
	}
}

// ClaimAutoSubscriptions returns and removes all pending
// auto-subscriptions for a Bureau entity. Called when an agent
// connects with auto:true on a subscribe stream.
func (m *Manager) ClaimAutoSubscriptions(entityLocalpart string) []forge.EntityRef {
	m.mu.Lock()
	defer m.mu.Unlock()

	pending := m.pendingAutoSubscriptions[entityLocalpart]
	if len(pending) == 0 {
		return nil
	}

	refs := make([]forge.EntityRef, 0, len(pending))
	for entityRef := range pending {
		refs = append(refs, entityRef)
	}

	delete(m.pendingAutoSubscriptions, entityLocalpart)

	m.logger.Info("auto-subscriptions claimed",
		"entity", entityLocalpart,
		"count", len(refs),
	)

	return refs
}

// PendingAutoSubscriptionCount returns the number of pending
// auto-subscriptions for a Bureau entity.
func (m *Manager) PendingAutoSubscriptionCount(entityLocalpart string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.pendingAutoSubscriptions[entityLocalpart])
}

// roleMatchesRules checks whether an involvement role matches any
// enabled auto-subscribe rule.
func roleMatchesRules(role InvolvementRole, rules forge.ForgeAutoSubscribeRules) bool {
	switch role {
	case RoleAuthor:
		return rules.OnAuthor
	case RoleAssignee:
		return rules.OnAssign
	case RoleReviewRequested:
		return rules.OnReviewRequest
	case RoleMentioned:
		return rules.OnMention
	default:
		return false
	}
}

// extractLocalpart extracts the localpart from a Matrix user ID
// ("@localpart:server" → "localpart"). Returns empty string if the
// ID is malformed.
func extractLocalpart(matrixUserID string) string {
	if !strings.HasPrefix(matrixUserID, "@") {
		return ""
	}
	colonIndex := strings.Index(matrixUserID, ":")
	if colonIndex <= 1 {
		return ""
	}
	return matrixUserID[1:colonIndex]
}
