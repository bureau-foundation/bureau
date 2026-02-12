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
	"fmt"
	"time"

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
const syncFilter = `{
	"room": {
		"state": {
			"types": [
				"m.bureau.machine_config",
				"m.bureau.credentials",
				"m.bureau.machine_status",
				"m.bureau.service",
				"m.bureau.layout",
				"m.bureau.project",
				"m.bureau.workspace"
			]
		},
		"timeline": {
			"types": [
				"m.bureau.machine_config",
				"m.bureau.credentials",
				"m.bureau.machine_status",
				"m.bureau.service",
				"m.bureau.layout",
				"m.bureau.project",
				"m.bureau.workspace",
				"m.room.message"
			],
			"limit": 50
		},
		"ephemeral": {
			"types": []
		},
		"account_data": {
			"types": []
		}
	},
	"presence": {
		"types": []
	},
	"account_data": {
		"types": []
	}
}`

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
		d.reconcileServices(ctx, added, removed, updated)
		d.pushServiceDirectory(ctx)
	}

	return response.NextBatch, nil
}

// syncLoop runs the incremental Matrix /sync long-poll loop. It receives
// the since token from initialSync and long-polls for changes. When state
// events arrive for a monitored room, the corresponding handler is called.
//
// The long-poll timeout is 30 seconds. When no events arrive within that
// window, the homeserver returns an empty response and the loop immediately
// re-polls. When events do arrive, the homeserver returns immediately.
//
// On transient errors, the loop retries with exponential backoff (1s → 30s).
// On context cancellation (daemon shutdown), the loop exits cleanly.
func (d *Daemon) syncLoop(ctx context.Context, sinceToken string) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		options := messaging.SyncOptions{
			Since:      sinceToken,
			Timeout:    30000, // 30 seconds in milliseconds
			SetTimeout: true,
			Filter:     syncFilter,
		}

		response, err := d.session.Sync(ctx, options)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			d.logger.Error("sync failed, retrying", "error", err, "backoff", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}

		backoff = time.Second
		sinceToken = response.NextBatch

		d.processSyncResponse(ctx, response)
	}
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
	var needsReconcile, needsPeerSync, needsServiceSync bool

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
		default:
			// Non-core rooms (workspace rooms joined via invite) with
			// state changes trigger reconcile so deferred principals
			// can re-evaluate StartConditions.
			needsReconcile = true
		}
	}

	if needsReconcile {
		d.logger.Info("state changed, reconciling")
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
			d.reconcileServices(ctx, added, removed, updated)
			d.pushServiceDirectory(ctx)
		}
	}

	// Process command messages from all rooms. Commands can arrive in
	// workspace rooms, config rooms, or any room the daemon is joined to.
	// Authorization is checked per-command via room power levels.
	for roomID, room := range response.Rooms.Join {
		d.processCommandMessages(ctx, roomID, room.Timeline.Events)
	}
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
