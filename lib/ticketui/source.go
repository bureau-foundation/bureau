// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"context"
	"sync"
	"time"

	"github.com/bureau-foundation/bureau/lib/ref"
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

	// Pipelines returns all pipeline tickets (Type == "pipeline")
	// across all statuses. Used by the Pipelines tab for sectioned
	// display (active, waiting, scheduled, history).
	Pipelines() Snapshot

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

// Mutator is an optional interface that Source implementations can
// provide to support ticket mutations. The TUI checks for this via
// type assertion on the source; when present, interactive mutation
// controls (status dropdown, priority selector, title edit, note add)
// are enabled. When absent (e.g., file-backed IndexSource), mutation
// UI elements are hidden.
//
// ServiceSource implements this interface; IndexSource does not.
//
// All methods are one-shot calls that send a mutation to the ticket
// service. Results arrive asynchronously through the subscribe stream
// and update the local index. Callers should not update the local state
// directly — the subscribe stream is the single source of truth.
type Mutator interface {
	// UpdateStatus transitions the ticket to a new status. The
	// assignee parameter is required when status is "in_progress"
	// (pass the operator's user ID) and must be empty otherwise.
	// Transitions to "closed" delegate to Close; transitions from
	// "closed" delegate to Reopen.
	UpdateStatus(ctx context.Context, ticketID, status, assignee string) error

	// UpdatePriority changes the ticket's priority (0-4).
	UpdatePriority(ctx context.Context, ticketID string, priority int) error

	// UpdateTitle changes the ticket's title.
	UpdateTitle(ctx context.Context, ticketID, title string) error

	// CloseTicket transitions the ticket to "closed" with an optional
	// reason. Pass empty string for no reason.
	CloseTicket(ctx context.Context, ticketID, reason string) error

	// ReopenTicket transitions a closed ticket back to "open".
	ReopenTicket(ctx context.Context, ticketID string) error

	// AddNote appends a note to the ticket. The author and
	// timestamp are set by the ticket service.
	AddNote(ctx context.Context, ticketID, body string) error

	// UpdateAssignee assigns or unassigns a ticket. The ticket
	// service enforces assignee/status atomicity (in_progress
	// requires an assignee, and an assignee requires in_progress),
	// so this method handles the coupling:
	//   - Assign (non-empty, ticket is open/blocked): transitions
	//     to in_progress with the given assignee
	//   - Reassign (non-empty, ticket already in_progress): updates
	//     the assignee without changing status
	//   - Unassign (empty): transitions to open, which auto-clears
	//     the assignee
	UpdateAssignee(ctx context.Context, ticketID, assignee string) error

	// SetDisposition sets the calling reviewer's disposition on a
	// ticket in review status. The gateID identifies which review
	// gate the disposition applies to. Valid dispositions are
	// "approved", "changes_requested", and "commented".
	SetDisposition(ctx context.Context, ticketID, gateID, disposition string) error
}

// MemberInfo describes a joined member of a tracked room, enriched
// with presence state from the ticket service's /sync loop. Used by
// the assignee dropdown to show availability indicators.
type MemberInfo struct {
	UserID          ref.UserID `cbor:"user_id"`
	DisplayName     string     `cbor:"display_name"`
	Presence        string     `cbor:"presence"`
	CurrentlyActive bool       `cbor:"currently_active"`
}

// MemberLister is an optional interface that Source implementations can
// provide to supply room member information for the assignee dropdown.
// The TUI checks for this via type assertion on the source; when
// present, the assignee click target and keyboard shortcut are enabled.
//
// ServiceSource implements this interface; IndexSource does not (there
// is no ticket service to query in file mode).
type MemberLister interface {
	// Members returns the cached list of joined members for the
	// connected room, sorted by presence (online first) and then
	// alphabetically by display name. Returns nil if members have
	// not been fetched yet.
	Members() []MemberInfo
}

// LoadingStater is an optional interface that Source implementations can
// provide to report their loading progress. The TUI checks for this via
// type assertion and displays appropriate loading indicators when the
// source is still receiving its initial snapshot.
//
// ServiceSource implements this interface; IndexSource does not (it is
// always fully loaded at construction time).
type LoadingStater interface {
	// LoadingState returns the current stream phase:
	//   "connecting"    — not yet connected to the service
	//   "loading"       — connected, receiving initial snapshot
	//   "open_complete" — open tickets loaded, Ready/Blocked tabs usable
	//   "caught_up"     — full snapshot received, live events flowing
	LoadingState() string
}

// loadingStateLabel returns a human-readable label for a loading state
// string, suitable for display in the TUI status bar or empty view.
func loadingStateLabel(state string) string {
	switch state {
	case "connecting":
		return "Connecting..."
	case "loading":
		return "Loading tickets..."
	case "open_complete":
		return "Loading history..."
	default:
		return "Loading..."
	}
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

// Pipelines returns all pipeline tickets across all statuses.
func (source *IndexSource) Pipelines() Snapshot {
	source.mutex.RLock()
	defer source.mutex.RUnlock()
	return Snapshot{
		Entries: source.index.List(ticketindex.Filter{Type: "pipeline"}),
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
