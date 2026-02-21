// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

// The sync loop replaces the timer-based polling loop with an event-driven
// Matrix /sync long-poll. Instead of fetching all state every 30 seconds,
// the daemon waits for the homeserver to push state changes as they happen.
//
// Four categories of rooms are monitored:
//   - Config room: m.bureau.machine_config, m.bureau.credentials → reconcile
//   - Machines room: m.bureau.machine_status → peer address updates
//   - Services room: m.bureau.service → service directory updates
//   - Workspace rooms: m.bureau.workspace, m.bureau.project → reconcile
//     (joined dynamically when the daemon accepts invites)
//
// The sync loop is purely a notification mechanism: when state changes are
// detected in a room, the existing handler (reconcile, syncPeerAddresses,
// syncServiceDirectory) is called to re-read the current state. This avoids
// coupling the sync response format to the handler logic — handlers continue
// to work exactly as they did under polling.
//
// Invites are accepted automatically. The daemon is invited to workspace rooms
// by "bureau workspace create" and must join to read workspace state events
// (needed for StartCondition evaluation on deferred principals).
//
// Benefits over polling:
//   - Latency: changes detected within milliseconds (vs up to 30 seconds)
//   - Efficiency: no periodic HTTP calls when nothing changes
//   - Correctness: no race window where changes are missed between polls

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/messaging"
)

// syncFilter restricts the /sync response to event types the daemon cares
// about. This avoids downloading presence, account data, typing notifications,
// and other event types that the daemon doesn't use.
//
// The timeline section includes both state event types and m.room.message.
// State events can appear as timeline events with a non-nil state_key on
// incremental syncs. m.room.message is needed for command messages
// (m.bureau.command msgtype) posted by the CLI for remote workspace operations.
//
// Workspace event types (project, workspace) are included so that
// state changes in workspace rooms trigger re-reconciliation. The daemon
// uses evaluateStartCondition with direct GetStateEvent calls to check
// whether conditions are met — the sync filter just ensures the room
// appears in the response so the daemon knows to re-check.
var syncFilter = buildSyncFilter()

// buildSyncFilter constructs the Matrix /sync filter JSON from typed
// schema constants. Using constants instead of raw strings ensures that
// event type renames are caught at compile time.
func buildSyncFilter() string {
	stateEventTypes := []ref.EventType{
		schema.EventTypeMachineConfig,
		schema.EventTypeCredentials,
		schema.EventTypeMachineStatus,
		schema.EventTypeService,
		schema.EventTypeLayout,
		schema.EventTypeProject,
		schema.EventTypeWorkspace,
		schema.EventTypeWorktree,
		schema.EventTypeAuthorization,
		schema.EventTypeTemporalGrant,
		schema.EventTypeFleetService,
		schema.EventTypeHALease,
		schema.MatrixEventTypeRoomMember, // detect fleet controllers joining fleet room
	}

	timelineEventTypes := make([]ref.EventType, len(stateEventTypes)+1)
	copy(timelineEventTypes, stateEventTypes)
	timelineEventTypes[len(stateEventTypes)] = schema.MatrixEventTypeMessage

	emptyTypes := []ref.EventType{}

	filter := map[string]any{
		"room": map[string]any{
			"state": map[string]any{
				"types": stateEventTypes,
			},
			"timeline": map[string]any{
				"types": timelineEventTypes,
				"limit": 50,
			},
			"ephemeral": map[string]any{
				"types": emptyTypes,
			},
			"account_data": map[string]any{
				"types": emptyTypes,
			},
		},
		"presence": map[string]any{
			"types": emptyTypes,
		},
		"account_data": map[string]any{
			"types": emptyTypes,
		},
	}

	data, err := json.Marshal(filter)
	if err != nil {
		panic("building sync filter: " + err.Error())
	}
	return string(data)
}

// initialSync performs the first Matrix /sync to obtain a since token, then
// runs the startup handlers (reconcile, peer address sync, service directory
// sync) to establish baseline state. Called synchronously from run() before
// the incremental sync loop starts.
//
// Returns the next_batch token for the incremental sync loop. If the /sync
// call fails, returns an empty token — the sync loop will start from scratch.
func (d *Daemon) initialSync(ctx context.Context) (string, error) {
	// The initial /sync (no since token) returns immediately with the full
	// state snapshot. We don't set a long-poll timeout here.
	response, err := d.session.Sync(ctx, messaging.SyncOptions{
		Filter: syncFilter,
	})
	if err != nil {
		return "", fmt.Errorf("initial sync: %w", err)
	}

	d.logger.Info("initial sync complete",
		"next_batch", response.NextBatch,
		"joined_rooms", len(response.Rooms.Join),
		"pending_invites", len(response.Rooms.Invite),
	)

	// Accept any pending invites before running handlers. The daemon may
	// have been invited to workspace rooms while it was offline — joining
	// them now ensures evaluateStartCondition can read workspace state
	// events during the initial reconcile.
	for roomID := range response.Rooms.Invite {
		d.logger.Info("accepting pending invite", "room_id", roomID)
		if _, err := d.session.JoinRoom(ctx, roomID); err != nil {
			d.logger.Error("failed to accept pending invite", "room_id", roomID, "error", err)
		}
	}

	// Run all handlers unconditionally to establish baseline state.
	// These use their own GetStateEvent/GetRoomState calls, which is
	// slightly redundant with the sync response but keeps the handlers
	// decoupled from the sync response format.
	if err := d.reconcile(ctx); err != nil {
		d.logger.Error("initial reconciliation failed", "error", err)
	}
	if err := d.syncPeerAddresses(ctx); err != nil {
		d.logger.Error("initial peer address sync failed", "error", err)
	}
	if added, removed, updated, err := d.syncServiceDirectory(ctx); err != nil {
		d.logger.Error("initial service directory sync failed", "error", err)
	} else {
		d.logger.Info("service directory synced",
			"services", len(d.services),
			"local", len(d.localServices()),
			"remote", len(d.remoteServices()),
		)
		consumers := d.runningConsumers()
		d.reconcileServices(ctx, consumers, added, removed, updated)
		d.pushServiceDirectory(ctx, consumers)
		d.discoverSharedCache(ctx)
		d.discoverPushTargets(ctx)
	}

	// Evaluate HA leases for pre-existing critical services. Without
	// this, a daemon that boots while a critical service has an expired
	// lease won't attempt acquisition until the next fleet room state
	// change. Runs after the service directory sync so that the HA
	// watchdog has access to the current service state.
	if d.haWatchdog != nil {
		d.haWatchdog.syncFleetState(ctx)
		d.haWatchdog.evaluate(ctx)
	}

	return response.NextBatch, nil
}

// syncErrorHandler classifies /sync errors for the shared
// service.RunSyncLoop. Authentication failures (token revoked,
// account deactivated) are unrecoverable — the daemon triggers
// emergency shutdown and tells the loop to abort. All other errors
// are retried with exponential backoff by the loop.
func (d *Daemon) syncErrorHandler(err error) service.SyncErrorAction {
	if isAuthError(err) {
		d.logger.Error("machine account authentication failed, initiating emergency shutdown",
			"error", err)
		d.emergencyShutdown()
		return service.SyncAbort
	}
	return service.SyncRetry
}

// processSyncResponse inspects a /sync response for invites and state
// changes, triggering the appropriate handlers.
//
// Invites are accepted first: the daemon auto-joins any room it's invited
// to (workspace rooms are delivered via invite from "bureau workspace create").
//
// State events can appear in two places within a JoinedRoom:
//   - State.Events: state present at the start of the sync window (gap fill)
//   - Timeline.Events: events with a non-nil StateKey are state changes
//
// When a room has any state changes, the corresponding handler is called to
// re-read the current state. Non-core rooms (workspace rooms joined via
// invite) trigger reconcile so deferred principals can re-evaluate
// StartConditions. Handlers are called in dependency order: peer addresses
// before services (so relay routing has up-to-date addresses).
func (d *Daemon) processSyncResponse(ctx context.Context, response *messaging.SyncResponse) {
	var needsReconcile, needsPeerSync, needsServiceSync, needsHAEval bool

	// Accept any pending invites. The daemon is invited to workspace rooms
	// by "bureau workspace create" and must join to read workspace state
	// events needed for StartCondition evaluation on deferred principals.
	for roomID := range response.Rooms.Invite {
		d.logger.Info("accepting room invite", "room_id", roomID)
		if _, err := d.session.JoinRoom(ctx, roomID); err != nil {
			d.logger.Error("failed to accept room invite", "room_id", roomID, "error", err)
			continue
		}
		needsReconcile = true
	}

	// Detect config room eviction. Being kicked from the config room
	// means the admin has revoked or decommissioned this machine — the
	// daemon can no longer read credentials or machine config. The only
	// safe response is emergency shutdown: destroy all sandboxes and
	// exit. This handles the race where the revoke/decommission command
	// tombstones credential state events and kicks the machine in the
	// same sync batch — the room appears in Rooms.Leave (not Rooms.Join)
	// because the final membership state is "leave", so the tombstone
	// events are never visible to the Join handler.
	if _, kicked := response.Rooms.Leave[d.configRoomID]; kicked {
		d.logger.Error("evicted from config room, initiating emergency shutdown",
			"config_room", d.configRoomID)
		d.emergencyShutdown()
		return
	}

	for roomID, room := range response.Rooms.Join {
		if !roomHasStateChanges(room) {
			continue
		}

		switch roomID {
		case d.configRoomID:
			needsReconcile = true
		case d.machineRoomID:
			needsPeerSync = true
		case d.serviceRoomID:
			needsServiceSync = true
		case d.fleetRoomID:
			needsHAEval = true
		case d.systemRoomID:
			// The system room carries token signing keys and operational
			// messages. State changes here don't require reconciliation.
			continue
		default:
			// Non-core rooms (workspace rooms joined via invite) with
			// state changes trigger reconcile so deferred principals
			// can re-evaluate StartConditions.
			needsReconcile = true
		}
	}

	// Process temporal grant events before reconcile so that grants
	// added in this sync batch are already in the index when reconcile
	// calls SetPrincipal (which preserves existing temporal grants).
	d.processTemporalGrantEvents(response)

	if needsReconcile {
		d.logger.Info("state changed, reconciling")
		// Config or workspace state changed — clear all start failure
		// backoffs so the reconcile below can immediately retry principals
		// that were blocked by a now-potentially-resolved issue (new
		// credentials provisioned, template updated, config changed, etc.).
		d.reconcileMu.Lock()
		d.clearStartFailures()
		d.reconcileMu.Unlock()
		if err := d.reconcile(ctx); err != nil {
			d.logger.Error("reconciliation failed", "error", err)
		}
	}

	if needsPeerSync {
		d.logger.Info("machine room state changed, syncing peer addresses")
		if err := d.syncPeerAddresses(ctx); err != nil {
			d.logger.Error("peer address sync failed", "error", err)
		}
	}

	if needsServiceSync {
		d.logger.Info("service room state changed, syncing service directory")
		added, removed, updated, err := d.syncServiceDirectory(ctx)
		if err != nil {
			d.logger.Error("service directory sync failed", "error", err)
		} else {
			consumers := d.runningConsumers()
			d.reconcileServices(ctx, consumers, added, removed, updated)
			d.pushServiceDirectory(ctx, consumers)
			d.discoverSharedCache(ctx)
			d.discoverPushTargets(ctx)

			// Post a message naming each changed service so that
			// observers (tests, operators) can synchronize on specific
			// service events without matching unrelated changes from
			// other machines or tests sharing the global service room.
			if changeCount := len(added) + len(removed) + len(updated); changeCount > 0 {
				if _, err := d.sendEventRetry(ctx, d.configRoomID, schema.MatrixEventTypeMessage,
					schema.NewServiceDirectoryUpdatedMessage(added, removed, updated)); err != nil {
					d.logger.Error("failed to post service directory update", "error", err)
				}
			}

			// Service directory changed — clear service resolution
			// failures and trigger reconcile if any principals were
			// blocked waiting for a service that may now be available.
			d.reconcileMu.Lock()
			cleared := d.clearStartFailuresByCategory(failureCategoryServiceResolution)
			d.reconcileMu.Unlock()
			if cleared > 0 && !needsReconcile {
				d.logger.Info("service directory change cleared start failures, reconciling",
					"cleared_count", cleared)
				if err := d.reconcile(ctx); err != nil {
					d.logger.Error("reconciliation after service sync failed", "error", err)
				}
			}
		}
	}

	if needsHAEval {
		if d.haWatchdog != nil {
			d.logger.Info("fleet room state changed, evaluating HA leases")
			d.haWatchdog.syncFleetState(ctx)
			d.haWatchdog.evaluate(ctx)
		}
	}

	// Process command messages from all rooms. Commands can arrive in
	// workspace rooms, config rooms, or any room the daemon is joined to.
	// Authorization is checked per-command via room power levels.
	for roomID, room := range response.Rooms.Join {
		d.processCommandMessages(ctx, roomID, room.Timeline.Events)
	}
}

// processTemporalGrantEvents scans a sync response for m.bureau.temporal_grant
// state events and applies them to the authorization index. A temporal grant
// with non-empty content is added; an event with empty content (tombstone)
// revokes all grants for that principal linked to the same ticket (state key).
//
// Called before reconcile so that grants added in this sync batch are already
// in the index when reconcile calls SetPrincipal (which preserves existing
// temporal grants).
func (d *Daemon) processTemporalGrantEvents(response *messaging.SyncResponse) {
	for roomID, room := range response.Rooms.Join {
		// Check both state and timeline sections for temporal grant events.
		allEvents := make([]messaging.Event, 0, len(room.State.Events)+len(room.Timeline.Events))
		allEvents = append(allEvents, room.State.Events...)
		allEvents = append(allEvents, room.Timeline.Events...)

		for _, event := range allEvents {
			if event.Type != schema.EventTypeTemporalGrant {
				continue
			}
			if event.StateKey == nil {
				continue
			}

			stateKey := *event.StateKey // ticket reference or grant ID

			// Empty content is a tombstone — revoke the temporal grant.
			if len(event.Content) == 0 {
				// We don't know the principal without parsing, but the
				// state key is the ticket reference. Iterate all principals
				// in the index and revoke by ticket.
				for _, localpart := range d.authorizationIndex.Principals() {
					if count := d.authorizationIndex.RevokeTemporalGrant(localpart, stateKey); count > 0 {
						d.logger.Info("revoked temporal grant",
							"principal", localpart,
							"ticket", stateKey,
							"room_id", roomID,
							"revoked_count", count,
						)
					}
				}
				continue
			}

			// Event.Content is map[string]any from JSON deserialization.
			// Re-marshal to JSON so we can unmarshal into the typed struct.
			contentBytes, err := json.Marshal(event.Content)
			if err != nil {
				d.logger.Error("marshaling temporal grant event content",
					"room_id", roomID,
					"state_key", stateKey,
					"error", err,
				)
				continue
			}

			var content schema.TemporalGrantContent
			if err := json.Unmarshal(contentBytes, &content); err != nil {
				d.logger.Error("parsing temporal grant event",
					"room_id", roomID,
					"state_key", stateKey,
					"error", err,
				)
				continue
			}

			if content.Principal.IsZero() {
				d.logger.Warn("temporal grant has empty principal",
					"room_id", roomID,
					"state_key", stateKey,
				)
				continue
			}

			// Ensure the ticket field matches the state key for consistency.
			if content.Grant.Ticket == "" {
				content.Grant.Ticket = stateKey
			}

			// Stamp source provenance so the CLI can show where this grant
			// came from. Temporal grants already carry Ticket, GrantedBy,
			// and GrantedAt for detailed provenance.
			content.Grant.Source = schema.SourceTemporal

			if d.authorizationIndex.AddTemporalGrant(content.Principal, content.Grant) {
				d.logger.Info("added temporal grant",
					"principal", content.Principal,
					"ticket", content.Grant.Ticket,
					"expires_at", content.Grant.ExpiresAt,
					"room_id", roomID,
				)
			} else {
				d.logger.Warn("failed to add temporal grant (missing expiry or ticket)",
					"principal", content.Principal,
					"room_id", roomID,
					"state_key", stateKey,
				)
			}
		}
	}
}

// isAuthError returns true if the error indicates the daemon's Matrix
// account has been deactivated or its access token is invalid. These are
// unrecoverable: the daemon cannot sync or perform any Matrix operations,
// so it must shut down.
func isAuthError(err error) bool {
	return messaging.IsMatrixError(err, messaging.ErrCodeUnknownToken) ||
		messaging.IsMatrixError(err, messaging.ErrCodeForbidden)
}

// roomHasStateChanges returns true if the JoinedRoom contains any state
// events — either in the state section or as timeline events with a state key.
func roomHasStateChanges(room messaging.JoinedRoom) bool {
	if len(room.State.Events) > 0 {
		return true
	}
	for _, event := range room.Timeline.Events {
		if event.StateKey != nil {
			return true
		}
	}
	return false
}
