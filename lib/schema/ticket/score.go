// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

// TicketScore holds computed ranking dimensions for a ready ticket.
// Each dimension captures a different aspect of why a ticket should
// be worked on next: leverage (what it unblocks), urgency (downstream
// priority inheritance), staleness (how long it's been actionable),
// and effort (proxy for how much work it requires).
type TicketScore struct {
	// UnblockCount is the number of currently-blocked tickets that
	// would become newly ready if this ticket were closed. This is
	// the marginal readiness gain — tickets that have other open
	// blockers are not counted.
	UnblockCount int `json:"unblock_count"`

	// BorrowedPriority is the highest priority (lowest number)
	// among all transitively-dependent tickets. A P3 ticket that
	// blocks a P0 has BorrowedPriority 0. -1 if no dependents.
	BorrowedPriority int `json:"borrowed_priority"`

	// DaysSinceReady approximates how long this ticket has been
	// actionable. For tickets with blockers, uses the latest
	// ClosedAt among blockers. For tickets without blockers, uses
	// CreatedAt. Zero if the timestamp cannot be parsed.
	DaysSinceReady int `json:"days_since_ready"`

	// NoteCount is len(Notes), a proxy for investigation effort.
	// Tickets with many notes have typically required multiple
	// iterations to understand or fix.
	NoteCount int `json:"note_count"`

	// Composite is the weighted sum of all dimensions. Higher
	// values mean "assign this first." Computed by Score() using
	// the provided RankWeights.
	Composite float64 `json:"composite"`
}

// EpicHealthStats holds health metrics for an epic's open children.
// These go beyond simple closed/total progress to capture parallelism
// potential and structural depth of the remaining work.
type EpicHealthStats struct {
	// TotalChildren is the total number of children (any status).
	TotalChildren int `json:"total_children"`

	// ClosedChildren is the number of children with status "closed".
	ClosedChildren int `json:"closed_children"`

	// ReadyChildren is the number of children that pass isReady().
	// This is the parallelism width: how many agents could work on
	// this epic's children simultaneously.
	ReadyChildren int `json:"ready_children"`

	// ActiveFraction is (ready + in_progress) / max(open, 1) where
	// "open" means total - closed. Measures what fraction of
	// remaining work is currently actionable.
	ActiveFraction float64 `json:"active_fraction"`

	// CriticalDepth is the longest chain of open blocked_by edges
	// among the epic's children. This is the irreducible sequential
	// depth: the minimum number of serial steps to complete the
	// epic, regardless of parallelism.
	CriticalDepth int `json:"critical_depth"`
}
