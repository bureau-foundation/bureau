// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// Matcher selects services from the service directory. Compose matchers
// using And, HasCapability, OnMachine, and other constructors. For
// filtering criteria that no built-in constructor covers, callers can
// pass a function directly:
//
//	service.Matcher(func(s schema.Service) bool { return s.Protocol == "grpc" })
type Matcher func(schema.Service) bool

// HasCapability returns a Matcher that selects services advertising
// the given capability string.
func HasCapability(capability string) Matcher {
	return func(s schema.Service) bool {
		for _, c := range s.Capabilities {
			if c == capability {
				return true
			}
		}
		return false
	}
}

// HasAllCapabilities returns a Matcher that selects services advertising
// every specified capability. An empty list matches all services.
func HasAllCapabilities(capabilities ...string) Matcher {
	return func(s schema.Service) bool {
		for _, required := range capabilities {
			found := false
			for _, c := range s.Capabilities {
				if c == required {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true
	}
}

// OnMachine returns a Matcher that selects services running on the
// given machine.
func OnMachine(machine ref.Machine) Matcher {
	return func(s schema.Service) bool {
		return s.Machine == machine
	}
}

// And returns a Matcher that requires every sub-matcher to accept.
// An empty list matches all services.
func And(matchers ...Matcher) Matcher {
	return func(s schema.Service) bool {
		for _, m := range matchers {
			if !m(s) {
				return false
			}
		}
		return true
	}
}

// FindAll returns every service in the slice that satisfies the matcher.
func FindAll(services []schema.Service, match Matcher) []schema.Service {
	var result []schema.Service
	for _, s := range services {
		if match(s) {
			result = append(result, s)
		}
	}
	return result
}

// FindFirst returns the first service in the slice that satisfies the
// matcher. Returns the zero value and false if no service matches.
func FindFirst(services []schema.Service, match Matcher) (schema.Service, bool) {
	for _, s := range services {
		if match(s) {
			return s, true
		}
	}
	return schema.Service{}, false
}

// ParseServiceDirectory extracts typed Service entries from raw room
// state events. Skips events that are not m.bureau.service, have no
// state key, fail to parse, or represent deregistered services (zero
// principal).
func ParseServiceDirectory(events []messaging.Event) []schema.Service {
	var services []schema.Service
	for _, event := range events {
		if event.Type != schema.EventTypeService {
			continue
		}
		if event.StateKey == nil {
			continue
		}

		contentJSON, err := json.Marshal(event.Content)
		if err != nil {
			continue
		}

		var parsed schema.Service
		if err := json.Unmarshal(contentJSON, &parsed); err != nil {
			continue
		}

		// Skip deregistered services — an empty principal means the
		// state event was cleared by Deregister.
		if parsed.Principal.IsZero() {
			continue
		}

		services = append(services, parsed)
	}
	return services
}

// QueryServices fetches the service directory from the fleet's service
// room and returns all services matching the criteria. Intended for
// callers without a persistent sync loop (CLI commands, one-shot tools).
// Callers with an in-memory service cache (the daemon) should use
// FindAll or FindFirst on their cached data instead.
func QueryServices(ctx context.Context, session messaging.Session, fleet ref.Fleet, match Matcher) ([]schema.Service, error) {
	directory, err := fetchServiceDirectory(ctx, session, fleet)
	if err != nil {
		return nil, err
	}
	return FindAll(directory, match), nil
}

// QueryFirst fetches the service directory and returns the first
// matching service. Returns the zero Service and false when nothing
// matches.
func QueryFirst(ctx context.Context, session messaging.Session, fleet ref.Fleet, match Matcher) (schema.Service, bool, error) {
	directory, err := fetchServiceDirectory(ctx, session, fleet)
	if err != nil {
		return schema.Service{}, false, err
	}
	result, found := FindFirst(directory, match)
	return result, found, nil
}

// EnsureServiceInRoom invites a service to a room and waits for it to
// become operationally ready. "Ready" means the service has joined the
// room, processed the room's configuration, and sent an
// m.bureau.service_ready event. This is a stronger guarantee than
// membership alone — the service is accepting requests for this room.
//
// The room watcher starts before the invitation is sent so the ready
// event cannot be missed regardless of timing. If the service is
// already a member, the function assumes it is tracking the room and
// returns immediately — the ready event was sent on a previous join.
//
// The /sync calls use a room-scoped filter (only this room, only
// service_ready timeline events) and retry up to 5 times on transient
// errors (connection reset, timeout). This avoids the failure mode
// where an unfiltered sync across hundreds of rooms triggers a
// connection reset from the homeserver under load.
func EnsureServiceInRoom(ctx context.Context, session messaging.Session, roomID ref.RoomID, serviceUserID ref.UserID) error {
	logger := slog.With("room_id", roomID, "service_user", serviceUserID)

	// Fast path: if the service is already a member, it has already
	// processed room state and sent its ready event on a prior join.
	// The only case where membership exists without tracking is the
	// brief window during initial join — but that window is always
	// covered by a prior EnsureServiceInRoom call that waited for
	// the ready event.
	if isJoined, err := checkMembership(ctx, session, roomID, serviceUserID); err == nil && isJoined {
		logger.Info("service already joined, skipping wait")
		return nil
	}

	// Start watching BEFORE inviting so the ready event cannot be
	// missed. The filter scopes /sync to only this room and only
	// service_ready timeline events, which keeps responses small
	// even when the session is in hundreds of rooms.
	logger.Info("starting room watch for service ready")
	watcher, err := messaging.WatchRoom(ctx, session, roomID, &messaging.SyncFilter{
		TimelineTypes: []string{string(schema.EventTypeServiceReady)},
		ExcludeState:  true,
	})
	if err != nil {
		return fmt.Errorf("watching room %s for service ready: %w", roomID, err)
	}
	logger.Info("room watch started", "sync_position", watcher.SyncPosition())

	// Double-check membership after capturing the sync position.
	// ConfigureRoom may have already invited the service before
	// this function was called — if the service joined between the
	// initial membership check and WatchRoom creation, it has
	// already sent service_ready (which is past our sync token).
	// The second check catches this race without modifying
	// WatchRoom's contract.
	if isJoined, err := checkMembership(ctx, session, roomID, serviceUserID); err == nil && isJoined {
		logger.Info("service joined during watch setup, skipping wait")
		return nil
	}

	// Send the invitation. The watcher is already capturing events.
	if inviteErr := session.InviteUser(ctx, roomID, serviceUserID); inviteErr != nil {
		if !messaging.IsMatrixError(inviteErr, "M_FORBIDDEN") {
			return fmt.Errorf("inviting %s to room %s: %w", serviceUserID, roomID, inviteErr)
		}
		logger.Info("service already invited or joined (M_FORBIDDEN on invite)")
	}

	// Wait for the service to send m.bureau.service_ready. The
	// RoomWatcher retries up to 5 times on transient /sync errors
	// (connection reset, timeout) with idle connection cleanup,
	// matching the daemon's resilience to homeserver hiccups.
	logger.Info("waiting for service ready event")
	_, err = watcher.WaitForEvent(ctx, func(event messaging.Event) bool {
		return event.Type == schema.EventTypeServiceReady && event.Sender == serviceUserID
	})
	if err != nil {
		return fmt.Errorf("waiting for %s to become ready in room %s: %w", serviceUserID, roomID, err)
	}
	logger.Info("service ready confirmed")
	return nil
}

// checkMembership reads the m.room.member state event for a user in a
// room. Returns true if the user's membership is "join".
func checkMembership(ctx context.Context, session messaging.Session, roomID ref.RoomID, userID ref.UserID) (bool, error) {
	content, err := session.GetStateEvent(ctx, roomID, ref.EventType("m.room.member"), userID.String())
	if err != nil {
		return false, err
	}
	var member struct {
		Membership string `json:"membership"`
	}
	if err := json.Unmarshal(content, &member); err != nil {
		return false, err
	}
	return member.Membership == "join", nil
}

// fetchServiceDirectory resolves the fleet's service room alias and
// parses all service registrations from room state.
func fetchServiceDirectory(ctx context.Context, session messaging.Session, fleet ref.Fleet) ([]schema.Service, error) {
	serviceRoomAlias := fleet.ServiceRoomAlias()
	serviceRoomID, err := session.ResolveAlias(ctx, serviceRoomAlias)
	if err != nil {
		return nil, fmt.Errorf("resolving service room %s: %w", serviceRoomAlias, err)
	}

	events, err := session.GetRoomState(ctx, serviceRoomID)
	if err != nil {
		return nil, fmt.Errorf("reading service room state: %w", err)
	}

	return ParseServiceDirectory(events), nil
}
