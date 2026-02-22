// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"time"
)

// HeatDecayDuration is how long an item glows after a change event.
// Heat starts at 1.0 and decays linearly to 0.0 over this duration.
const HeatDecayDuration = 5 * time.Second

// HeatTickInterval is the re-render interval while any items are hot.
// 100ms gives ~10fps animation for smooth color decay.
const HeatTickInterval = 100 * time.Millisecond

// HeatKind distinguishes different types of changes for color selection.
type HeatKind int

const (
	// HeatPut indicates an item was created or updated (amber glow).
	HeatPut HeatKind = iota
	// HeatRemove indicates an item was removed (red glow).
	HeatRemove
)

// heatEntry records when and how an item was last changed.
type heatEntry struct {
	ignition time.Time
	kind     HeatKind
}

// HeatTracker maps item IDs to ignition timestamps for animated
// change highlighting. Each change "ignites" an item, which then
// decays from full intensity to zero over [HeatDecayDuration].
type HeatTracker struct {
	entries map[string]heatEntry
}

// NewHeatTracker creates an empty heat tracker.
func NewHeatTracker() *HeatTracker {
	return &HeatTracker{
		entries: make(map[string]heatEntry),
	}
}

// Ignite records a change event for an item. Resets the decay timer
// if the item was already hot.
func (tracker *HeatTracker) Ignite(itemID string, kind HeatKind, now time.Time) {
	tracker.entries[itemID] = heatEntry{ignition: now, kind: kind}
}

// Heat returns the current intensity for an item: 1.0 at ignition,
// linearly decaying to 0.0 over [HeatDecayDuration]. Returns 0.0 for
// items that were never ignited or have fully decayed.
func (tracker *HeatTracker) Heat(itemID string, now time.Time) float64 {
	entry, exists := tracker.entries[itemID]
	if !exists {
		return 0.0
	}
	elapsed := now.Sub(entry.ignition)
	if elapsed >= HeatDecayDuration {
		return 0.0
	}
	return 1.0 - float64(elapsed)/float64(HeatDecayDuration)
}

// Kind returns the heat kind for an item (put or remove). Only
// meaningful when Heat() returns > 0.
func (tracker *HeatTracker) Kind(itemID string) HeatKind {
	entry, exists := tracker.entries[itemID]
	if !exists {
		return HeatPut
	}
	return entry.kind
}

// HasHot returns true if any tracked item still has heat > 0,
// meaning the tick timer should keep running for animation.
func (tracker *HeatTracker) HasHot(now time.Time) bool {
	for itemID, entry := range tracker.entries {
		if now.Sub(entry.ignition) < HeatDecayDuration {
			return true
		}
		// Garbage-collect fully decayed entries.
		delete(tracker.entries, itemID)
	}
	return false
}
