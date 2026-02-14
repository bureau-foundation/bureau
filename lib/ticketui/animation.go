// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"time"
)

// heatDecayDuration is how long a ticket glows after a change event.
// Heat starts at 1.0 and decays linearly to 0.0 over this duration.
const heatDecayDuration = 5 * time.Second

// heatTickInterval is the re-render interval while any tickets are hot.
// 100ms gives ~10fps animation for smooth color decay.
const heatTickInterval = 100 * time.Millisecond

// HeatKind distinguishes different types of changes for color selection.
type HeatKind int

const (
	// HeatPut indicates a ticket was created or updated (amber glow).
	HeatPut HeatKind = iota
	// HeatRemove indicates a ticket was removed (red glow).
	HeatRemove
)

// heatEntry records when and how a ticket was last changed.
type heatEntry struct {
	ignition time.Time
	kind     HeatKind
}

// HeatTracker maps ticket IDs to ignition timestamps for animated
// change highlighting. Each change "ignites" a ticket, which then
// decays from full intensity to zero over [heatDecayDuration].
type HeatTracker struct {
	entries map[string]heatEntry
}

// NewHeatTracker creates an empty heat tracker.
func NewHeatTracker() *HeatTracker {
	return &HeatTracker{
		entries: make(map[string]heatEntry),
	}
}

// Ignite records a change event for a ticket. Resets the decay timer
// if the ticket was already hot.
func (tracker *HeatTracker) Ignite(ticketID string, kind HeatKind, now time.Time) {
	tracker.entries[ticketID] = heatEntry{ignition: now, kind: kind}
}

// Heat returns the current intensity for a ticket: 1.0 at ignition,
// linearly decaying to 0.0 over [heatDecayDuration]. Returns 0.0 for
// tickets that were never ignited or have fully decayed.
func (tracker *HeatTracker) Heat(ticketID string, now time.Time) float64 {
	entry, exists := tracker.entries[ticketID]
	if !exists {
		return 0.0
	}
	elapsed := now.Sub(entry.ignition)
	if elapsed >= heatDecayDuration {
		return 0.0
	}
	return 1.0 - float64(elapsed)/float64(heatDecayDuration)
}

// Kind returns the heat kind for a ticket (put or remove). Only
// meaningful when Heat() returns > 0.
func (tracker *HeatTracker) Kind(ticketID string) HeatKind {
	entry, exists := tracker.entries[ticketID]
	if !exists {
		return HeatPut
	}
	return entry.kind
}

// HasHot returns true if any tracked ticket still has heat > 0,
// meaning the tick timer should keep running for animation.
func (tracker *HeatTracker) HasHot(now time.Time) bool {
	for ticketID, entry := range tracker.entries {
		if now.Sub(entry.ignition) < heatDecayDuration {
			return true
		}
		// Garbage-collect fully decayed entries.
		delete(tracker.entries, ticketID)
	}
	return false
}
