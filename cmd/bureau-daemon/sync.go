// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

// The sync loop replaces the timer-based polling loop with an event-driven
// Matrix /sync long-poll. Instead of fetching all state every 30 seconds,
// the daemon waits for the homeserver to push state changes as they happen.
//
// Three rooms are monitored:
//   - Config room: m.bureau.machine_config, m.bureau.credentials → reconcile
//   - Machines room: m.bureau.machine_status → peer address updates
//   - Services room: m.bureau.service → service directory updates
//
// The sync loop is purely a notification mechanism: when state changes are
// detected in a room, the existing handler (reconcile, syncPeerAddresses,
// syncServiceDirectory) is called to re-read the current state. This avoids
// coupling the sync response format to the handler logic — handlers continue
// to work exactly as they did under polling.
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

// syncFilter restricts the /sync response to state events the daemon cares
// about. This avoids downloading timeline messages, presence, account data,
// typing notifications, and other event types that the daemon doesn't use.
//
// The timeline section is included (with the same type filter) because on
// incremental syncs, state events can appear as timeline events with a
// non-nil state_key. The limit is kept small since the daemon only checks
// for the presence of state events, not their content.
const syncFilter = `{
	"room": {
		"state": {
			"types": [
				"m.bureau.machine_config",
				"m.bureau.credentials",
				"m.bureau.machine_status",
				"m.bureau.service",
				"m.bureau.layout"
			]
		},
		"timeline": {
			"types": [
				"m.bureau.machine_config",
				"m.bureau.credentials",
				"m.bureau.machine_status",
				"m.bureau.service",
				"m.bureau.layout"
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
	)

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

// processSyncResponse inspects a /sync response for state changes in the
// daemon's monitored rooms and triggers the appropriate handlers.
//
// State events can appear in two places within a JoinedRoom:
//   - State.Events: state present at the start of the sync window (gap fill)
//   - Timeline.Events: events with a non-nil StateKey are state changes
//
// When a room has any state changes, the corresponding handler is called to
// re-read the current state. The handlers are called in dependency order:
// peer addresses before services (so relay routing has up-to-date addresses).
func (d *Daemon) processSyncResponse(ctx context.Context, response *messaging.SyncResponse) {
	var needsReconcile, needsPeerSync, needsServiceSync bool

	for roomID, room := range response.Rooms.Join {
		if !roomHasStateChanges(room) {
			continue
		}

		switch roomID {
		case d.configRoomID:
			needsReconcile = true
		case d.machinesRoomID:
			needsPeerSync = true
		case d.servicesRoomID:
			needsServiceSync = true
		}
	}

	if needsReconcile {
		d.logger.Info("config room state changed, reconciling")
		if err := d.reconcile(ctx); err != nil {
			d.logger.Error("reconciliation failed", "error", err)
		}
	}

	if needsPeerSync {
		d.logger.Info("machines room state changed, syncing peer addresses")
		if err := d.syncPeerAddresses(ctx); err != nil {
			d.logger.Error("peer address sync failed", "error", err)
		}
	}

	if needsServiceSync {
		d.logger.Info("services room state changed, syncing service directory")
		added, removed, updated, err := d.syncServiceDirectory(ctx)
		if err != nil {
			d.logger.Error("service directory sync failed", "error", err)
		} else {
			d.reconcileServices(ctx, added, removed, updated)
			d.pushServiceDirectory(ctx)
		}
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
