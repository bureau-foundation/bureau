// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Layout sync: each running principal has a layout watcher goroutine that
// bidirectionally syncs the tmux session layout with a Matrix state event:
//
//   - tmux → Matrix: the watcher monitors the tmux session via control mode
//     (tmux -C) for layout-relevant notifications (window add/close/rename,
//     pane split/resize). On change, it reads the full layout, diffs against
//     the last-published version, and publishes a state event update if
//     anything changed.
//
//   - Matrix → tmux: on sandbox creation, if a layout state event already
//     exists for the principal, the watcher applies it to the newly created
//     tmux session. This restores the agent's last-known workspace.
//
// The layout state event lives in the per-machine config room with the
// principal's localpart as the state key. The SourceMachine field stamps
// which machine published it, enabling loop prevention in multi-machine
// scenarios.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
	"github.com/bureau-foundation/bureau/observe"
)

// layoutWatcher tracks a running layout sync goroutine for a single principal.
type layoutWatcher struct {
	cancel context.CancelFunc

	// ready is closed when the ControlClient is attached and any initial
	// layout restore from Matrix has completed. Callers can wait on this
	// before triggering tmux operations that should produce layout events.
	ready chan struct{}

	// done is closed when the goroutine exits (either cleanly or on error).
	done chan struct{}
}

// startLayoutWatcher begins monitoring the tmux session for a principal.
// Safe to call multiple times for the same principal (subsequent calls are
// no-ops). The watcher runs until stopped or the parent context is cancelled.
//
// Requires d.tmuxServer to be non-nil. In production, this is always true
// (set in run()). If nil, the watcher is not started and a warning is logged.
func (d *Daemon) startLayoutWatcher(ctx context.Context, localpart string) {
	if d.tmuxServer == nil {
		d.logger.Warn("layout watcher not started: tmux server not configured",
			"principal", localpart)
		return
	}

	d.layoutWatchersMu.Lock()
	defer d.layoutWatchersMu.Unlock()

	if _, exists := d.layoutWatchers[localpart]; exists {
		return
	}

	watchContext, cancel := context.WithCancel(ctx)
	watcher := &layoutWatcher{
		cancel: cancel,
		ready:  make(chan struct{}),
		done:   make(chan struct{}),
	}
	d.layoutWatchers[localpart] = watcher

	go d.runLayoutWatcher(watchContext, localpart, watcher.ready, watcher.done)
}

// stopLayoutWatcher cancels the layout watcher for a principal and waits
// for the goroutine to exit. No-op if no watcher is running.
func (d *Daemon) stopLayoutWatcher(localpart string) {
	d.layoutWatchersMu.Lock()
	watcher, exists := d.layoutWatchers[localpart]
	if !exists {
		d.layoutWatchersMu.Unlock()
		return
	}
	delete(d.layoutWatchers, localpart)
	d.layoutWatchersMu.Unlock()

	watcher.cancel()
	<-watcher.done
}

// layoutWatcherReady returns the ready channel for a watcher, or nil
// if no watcher exists for the given principal. The ready channel
// closes when the ControlClient is attached and any initial layout
// restore has completed.
func (d *Daemon) layoutWatcherReady(localpart string) <-chan struct{} {
	d.layoutWatchersMu.Lock()
	defer d.layoutWatchersMu.Unlock()
	watcher, exists := d.layoutWatchers[localpart]
	if !exists {
		return nil
	}
	return watcher.ready
}

// layoutWatcherDone returns the done channel for a watcher, or nil
// if no watcher exists for the given principal. The done channel
// closes when the watcher goroutine exits.
func (d *Daemon) layoutWatcherDone(localpart string) <-chan struct{} {
	d.layoutWatchersMu.Lock()
	defer d.layoutWatchersMu.Unlock()
	watcher, exists := d.layoutWatchers[localpart]
	if !exists {
		return nil
	}
	return watcher.done
}

// stopAllLayoutWatchers cancels all running layout watchers and waits for
// all goroutines to exit. Called during daemon shutdown.
func (d *Daemon) stopAllLayoutWatchers() {
	d.layoutWatchersMu.Lock()
	watchers := make(map[string]*layoutWatcher, len(d.layoutWatchers))
	for localpart, watcher := range d.layoutWatchers {
		watchers[localpart] = watcher
	}
	d.layoutWatchers = make(map[string]*layoutWatcher)
	d.layoutWatchersMu.Unlock()

	// Cancel all watchers first, then wait. This ensures parallel
	// shutdown rather than sequential.
	for _, watcher := range watchers {
		watcher.cancel()
	}
	for _, watcher := range watchers {
		<-watcher.done
	}
}

// runLayoutWatcher is the main goroutine for a single principal's layout
// sync. It restores the layout from Matrix (if available), then watches
// for changes and publishes updates.
func (d *Daemon) runLayoutWatcher(ctx context.Context, localpart string, ready, done chan struct{}) {
	defer close(done)

	sessionName := "bureau/" + localpart

	// Start the control mode client to watch for layout changes. Must be
	// started before restoring from Matrix so we don't miss layout events
	// triggered by the restore itself (the debounce + LayoutEqual check
	// prevents spurious re-publishes).
	controlClient, err := observe.NewControlClient(ctx, d.tmuxServer, sessionName)
	if err != nil {
		d.logger.Error("start layout control client failed",
			"principal", localpart,
			"error", err,
		)
		close(ready)
		return
	}
	defer controlClient.Stop()

	// Try to restore the layout from Matrix. If a layout event exists for
	// this principal, apply it to the (newly created) tmux session and use
	// it as the baseline for change detection.
	var lastPublished *observe.Layout
	if stored, err := d.readLayoutEvent(ctx, localpart); err == nil {
		if err := observe.ApplyLayout(d.tmuxServer, sessionName, stored); err != nil {
			d.logger.Error("restore layout from matrix failed",
				"principal", localpart,
				"error", err,
			)
		} else {
			lastPublished = stored
			d.logger.Info("layout restored from matrix",
				"principal", localpart,
				"windows", len(stored.Windows),
			)
		}
	} else if !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
		d.logger.Error("read layout event failed",
			"principal", localpart,
			"error", err,
		)
	}

	// Signal that initialization is complete: the ControlClient is attached
	// and any restore from Matrix has been applied.
	close(ready)

	// Watch for layout changes and publish to Matrix.
	for range controlClient.Events() {
		current, err := observe.ReadTmuxLayout(d.tmuxServer, sessionName)
		if err != nil {
			d.logger.Error("read tmux layout failed",
				"principal", localpart,
				"error", err,
			)
			continue
		}

		if observe.LayoutEqual(current, lastPublished) {
			continue
		}

		content := observe.LayoutToSchema(current)
		content.SourceMachine = d.machineUserID

		if _, err := d.session.SendStateEvent(ctx, d.configRoomID,
			schema.EventTypeLayout, localpart, content); err != nil {
			d.logger.Error("publish layout failed",
				"principal", localpart,
				"error", err,
			)
			continue
		}

		lastPublished = current
		d.logger.Info("layout published",
			"principal", localpart,
			"windows", len(current.Windows),
		)
	}
}

// readLayoutEvent reads the layout state event for a principal from the
// config room and returns it as a runtime Layout.
func (d *Daemon) readLayoutEvent(ctx context.Context, localpart string) (*observe.Layout, error) {
	raw, err := d.session.GetStateEvent(ctx, d.configRoomID, schema.EventTypeLayout, localpart)
	if err != nil {
		return nil, err
	}

	var content schema.LayoutContent
	if err := json.Unmarshal(raw, &content); err != nil {
		return nil, fmt.Errorf("parsing layout event for %q: %w", localpart, err)
	}

	return observe.SchemaToLayout(content), nil
}
