// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import "github.com/bureau-foundation/bureau/lib/tui"

// Re-export heat tracker types from the shared TUI library so that
// existing code within this package can refer to them unqualified.

// HeatKind distinguishes different types of changes for color selection.
type HeatKind = tui.HeatKind

const (
	HeatPut    = tui.HeatPut
	HeatRemove = tui.HeatRemove
)

// HeatTracker maps item IDs to ignition timestamps for animated
// change highlighting.
type HeatTracker = tui.HeatTracker

// NewHeatTracker creates an empty heat tracker.
var NewHeatTracker = tui.NewHeatTracker

// heatTickInterval re-exports the animation tick interval for use in
// scheduleHeatTick.
const heatTickInterval = tui.HeatTickInterval
