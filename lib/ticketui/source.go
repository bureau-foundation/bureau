// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"sync"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/ticketindex"
)

// Snapshot is a point-in-time view of tickets with aggregate statistics.
type Snapshot struct {
	Entries []ticketindex.Entry
	Stats   ticketindex.Stats
}

// Event describes a single change to the ticket index, delivered via
// the [Source.Subscribe] channel for live-updating UIs.
type Event struct {
	TicketID string
	Kind     string // "put" or "remove"
	Content  ticket.TicketContent
}

// Source abstracts ticket data access for the TUI. Implementations
// range from a local in-memory index ([IndexSource]) to a CBOR socket
// client. The TUI code is identical regardless of backend.
type Source interface {
	// Ready returns actionable tickets: open+unblocked+gates-satisfied,
	// plus in_progress tickets already being worked on.
	Ready() Snapshot

	// Blocked returns tickets that are open but have unsatisfied
	// blockers or gates.
	Blocked() Snapshot

	// All returns every ticket regardless of status.
	All() Snapshot

	// Get returns a single ticket by ID.
	Get(ticketID string) (ticket.TicketContent, bool)

	// Children returns direct children of a parent ticket.
	Children(parentID string) []ticketindex.Entry

	// ChildProgress returns (total, closed) counts for a parent's
	// children.
	ChildProgress(parentID string) (total int, closed int)

	// Deps returns ticket IDs that ticketID transitively depends on.
	Deps(ticketID string) []string

	// Blocks returns ticket IDs that directly depend on ticketID.
	Blocks(ticketID string) []string

	// Score computes ranking dimensions for a ticket. The now
	// parameter drives staleness computation; callers pass the
	// current time rather than embedding a clock in the source.
	Score(ticketID string, now time.Time) ticketindex.TicketScore

	// EpicHealth returns health metrics for an epic's children:
	// parallelism width, active fraction, and critical path depth.
	EpicHealth(epicID string) ticketindex.EpicHealthStats

	// Subscribe returns a channel that receives Events when the
	// underlying data changes. Returns nil if live updates are not
	// supported (e.g., one-shot file load without --watch).
	Subscribe() <-chan Event
}

// IndexSource wraps a [ticketindex.Index] with a mutex for concurrent access
// and event dispatch. This is the local implementation used when loading
// from beads JSONL or when the ticket service runs in-process.
type IndexSource struct {
	mutex       sync.RWMutex
	index       *ticketindex.Index
	subscribers []chan Event
}

// NewIndexSource creates an IndexSource wrapping the given index.
func NewIndexSource(index *ticketindex.Index) *IndexSource {
	return &IndexSource{
		index: index,
	}
}

// Ready returns actionable tickets (open+unblocked and in_progress)
// with global statistics.
func (source *IndexSource) Ready() Snapshot {
	source.mutex.RLock()
	defer source.mutex.RUnlock()
	return Snapshot{
		Entries: source.index.Ready(),
		Stats:   source.index.Stats(),
	}
}

// Blocked returns open tickets with unsatisfied dependencies or gates.
func (source *IndexSource) Blocked() Snapshot {
	source.mutex.RLock()
	defer source.mutex.RUnlock()
	return Snapshot{
		Entries: source.index.Blocked(),
		Stats:   source.index.Stats(),
	}
}

// All returns every ticket in the index.
func (source *IndexSource) All() Snapshot {
	source.mutex.RLock()
	defer source.mutex.RUnlock()
	return Snapshot{
		Entries: source.index.List(ticketindex.Filter{}),
		Stats:   source.index.Stats(),
	}
}

// Get returns a single ticket by ID.
func (source *IndexSource) Get(ticketID string) (ticket.TicketContent, bool) {
	source.mutex.RLock()
	defer source.mutex.RUnlock()
	return source.index.Get(ticketID)
}

// Children returns direct children of a parent ticket.
func (source *IndexSource) Children(parentID string) []ticketindex.Entry {
	source.mutex.RLock()
	defer source.mutex.RUnlock()
	return source.index.Children(parentID)
}

// ChildProgress returns (total, closed) counts for a parent's children.
func (source *IndexSource) ChildProgress(parentID string) (total int, closed int) {
	source.mutex.RLock()
	defer source.mutex.RUnlock()
	return source.index.ChildProgress(parentID)
}

// Deps returns the transitive closure of dependencies for a ticket.
func (source *IndexSource) Deps(ticketID string) []string {
	source.mutex.RLock()
	defer source.mutex.RUnlock()
	return source.index.Deps(ticketID)
}

// Blocks returns ticket IDs that directly depend on the given ticket.
func (source *IndexSource) Blocks(ticketID string) []string {
	source.mutex.RLock()
	defer source.mutex.RUnlock()
	return source.index.Blocks(ticketID)
}

// Score computes ranking dimensions for a ticket using default weights.
func (source *IndexSource) Score(ticketID string, now time.Time) ticketindex.TicketScore {
	source.mutex.RLock()
	defer source.mutex.RUnlock()
	return source.index.Score(ticketID, now, ticketindex.DefaultRankWeights())
}

// EpicHealth returns health metrics for an epic's children.
func (source *IndexSource) EpicHealth(epicID string) ticketindex.EpicHealthStats {
	source.mutex.RLock()
	defer source.mutex.RUnlock()
	return source.index.EpicHealth(epicID)
}

// Subscribe returns a channel that receives Events when the index
// changes via [Put] or [Remove].
func (source *IndexSource) Subscribe() <-chan Event {
	source.mutex.Lock()
	defer source.mutex.Unlock()
	channel := make(chan Event, 64)
	source.subscribers = append(source.subscribers, channel)
	return channel
}

// Put adds or updates a ticket and dispatches an event to all
// subscribers. Safe for concurrent use.
func (source *IndexSource) Put(ticketID string, content ticket.TicketContent) {
	source.mutex.Lock()
	source.index.Put(ticketID, content)
	// Snapshot subscriber list under lock; dispatch after release.
	// The subscriber list is append-only, so this is safe.
	subscribers := source.subscribers
	source.mutex.Unlock()

	event := Event{TicketID: ticketID, Kind: "put", Content: content}
	for _, subscriber := range subscribers {
		select {
		case subscriber <- event:
		default:
			// Subscriber buffer full — drop event. The TUI will pick
			// up current state on the next snapshot refresh.
		}
	}
}

// Remove deletes a ticket and dispatches an event. Safe for concurrent use.
func (source *IndexSource) Remove(ticketID string) {
	source.mutex.Lock()
	content, exists := source.index.Get(ticketID)
	if !exists {
		source.mutex.Unlock()
		return
	}
	source.index.Remove(ticketID)
	subscribers := source.subscribers
	source.mutex.Unlock()

	event := Event{TicketID: ticketID, Kind: "remove", Content: content}
	for _, subscriber := range subscribers {
		select {
		case subscriber <- event:
		default:
		}
	}
}
