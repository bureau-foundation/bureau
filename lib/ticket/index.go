// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema"
)

// Entry pairs a ticket ID with its content. Returned by query methods.
type Entry struct {
	ID      string
	Content schema.TicketContent
}

// Filter controls which tickets [Index.List] returns. Zero-value fields
// mean "no filter" for that dimension. All non-zero fields must match
// (AND semantics).
type Filter struct {
	// Status matches tickets with this exact status.
	Status string

	// Priority matches tickets with this exact priority. Nil means
	// no filter (distinguishes "no filter" from "filter for priority 0
	// which is critical").
	Priority *int

	// Label matches tickets whose Labels slice contains this string.
	Label string

	// Assignee matches tickets assigned to this Matrix user ID.
	Assignee string

	// Type matches tickets with this exact type.
	Type string

	// Parent matches tickets whose Parent field equals this ticket ID.
	Parent string
}

// Stats holds aggregate counts across all tickets in the index.
type Stats struct {
	Total      int            `json:"total"`
	ByStatus   map[string]int `json:"by_status"`
	ByPriority map[int]int    `json:"by_priority"`
	ByType     map[string]int `json:"by_type"`
}

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

// RankWeights controls the composite score formula. Zero-valued
// weights are replaced with defaults by applyDefaults().
type RankWeights struct {
	Leverage  float64 // multiplier for UnblockCount
	Urgency   float64 // multiplier for (4 - effective_priority)
	Staleness float64 // multiplier for DaysSinceReady
	Effort    float64 // multiplier for 1/(1+NoteCount)
}

// DefaultRankWeights returns the default ranking weights. Leverage
// and urgency dominate because the beads data shows that dependency
// fan-out is high-variance (1 to 45+) and cross-epic priority
// inheritance is the primary signal a PM agent needs.
func DefaultRankWeights() RankWeights {
	return RankWeights{
		Leverage:  3.0,
		Urgency:   2.0,
		Staleness: 0.5,
		Effort:    0.5,
	}
}

// applyDefaults fills zero-valued weights with defaults.
func (weights RankWeights) applyDefaults() RankWeights {
	defaults := DefaultRankWeights()
	if weights.Leverage == 0 {
		weights.Leverage = defaults.Leverage
	}
	if weights.Urgency == 0 {
		weights.Urgency = defaults.Urgency
	}
	if weights.Staleness == 0 {
		weights.Staleness = defaults.Staleness
	}
	if weights.Effort == 0 {
		weights.Effort = defaults.Effort
	}
	return weights
}

// RankedEntry pairs a ticket entry with its computed score.
type RankedEntry struct {
	Entry
	Score TicketScore
}

// gateWatchKey identifies the event criteria a pending gate watches for.
// Used as a map key in the gate watch index for O(1) event-to-gate lookup.
type gateWatchKey struct {
	// eventType is the Matrix event type the gate watches
	// (e.g., EventTypePipelineResult, EventTypeTicket).
	eventType string

	// stateKey is the Matrix state key the gate watches. Empty means
	// "match any state key for this event type" — used by pipeline
	// gates (which match on content, not state key) and state_event
	// gates with no specific state key.
	stateKey string
}

// GateWatch identifies a specific pending gate on a specific ticket.
// Returned by [Index.WatchedGates] to allow direct gate access without
// scanning all tickets.
type GateWatch struct {
	TicketID  string
	GateIndex int
}

// Index is a per-room in-memory ticket index. It maintains secondary
// indexes for filtered queries and a dependency graph for readiness
// computation.
//
// Construct with [NewIndex]. Not safe for concurrent use.
type Index struct {
	tickets map[string]schema.TicketContent

	// Secondary indexes: dimension value → set of ticket IDs.
	byStatus   map[string]map[string]struct{}
	byPriority map[int]map[string]struct{}
	byLabel    map[string]map[string]struct{}
	byAssignee map[string]map[string]struct{}
	byType     map[string]map[string]struct{}

	// Parent → children reverse map.
	children map[string]map[string]struct{}

	// Dependency graph.
	// blockedBy: ticketID → set of ticket IDs it depends on (forward edges).
	// blocks: ticketID → set of ticket IDs that depend on it (reverse edges).
	blockedBy map[string]map[string]struct{}
	blocks    map[string]map[string]struct{}

	// Gate watch index: maps event criteria to pending gates that
	// watch for those events. Maintained by Put/Remove alongside
	// other secondary indexes. Enables O(1) event-to-gate lookup
	// instead of scanning all tickets with pending gates.
	//
	// Keys with empty stateKey are wildcard entries matching any
	// state key for that event type (used by pipeline gates and
	// state_event gates with no specific state key). Lookup checks
	// both the exact (eventType, stateKey) key and the wildcard
	// (eventType, "") key.
	gateWatch map[gateWatchKey]map[GateWatch]struct{}
}

// NewIndex returns an empty index ready for use.
func NewIndex() *Index {
	return &Index{
		tickets:    make(map[string]schema.TicketContent),
		byStatus:   make(map[string]map[string]struct{}),
		byPriority: make(map[int]map[string]struct{}),
		byLabel:    make(map[string]map[string]struct{}),
		byAssignee: make(map[string]map[string]struct{}),
		byType:     make(map[string]map[string]struct{}),
		children:   make(map[string]map[string]struct{}),
		blockedBy:  make(map[string]map[string]struct{}),
		blocks:     make(map[string]map[string]struct{}),
		gateWatch:  make(map[gateWatchKey]map[GateWatch]struct{}),
	}
}

// Len returns the number of tickets in the index.
func (idx *Index) Len() int {
	return len(idx.tickets)
}

// Put adds or updates a ticket in the index. If a ticket with the same
// ID already exists, it is replaced and all secondary indexes are
// updated to reflect the new content.
//
// Put does not validate the content — the caller (ticket service) is
// responsible for validation before writing to Matrix. The index
// faithfully stores whatever it receives, since the data has already
// been persisted as a Matrix state event.
func (idx *Index) Put(ticketID string, content schema.TicketContent) {
	if old, exists := idx.tickets[ticketID]; exists {
		idx.updateIndexes(ticketID, &old, removeFromStringIndex, removeFromIntIndex)
		idx.removeGateWatches(ticketID, &old)
	}

	// Clone all slice fields to break backing-array aliasing between
	// the stored content and the caller's copy. Go's value-type map
	// stores a copy of the struct, but slice headers in that copy
	// share backing arrays with the caller's original. If the caller
	// later modifies slice elements in-place (e.g., setting
	// gate.Status = "satisfied" before calling Put), those mutations
	// alias through to the stored copy, corrupting secondary index
	// maintenance that needs to see the original values for removal.
	// Cloning at the storage boundary makes the index self-protecting
	// regardless of caller mutation patterns.
	if len(content.Labels) > 0 {
		content.Labels = append([]string(nil), content.Labels...)
	}
	if len(content.BlockedBy) > 0 {
		content.BlockedBy = append([]string(nil), content.BlockedBy...)
	}
	if len(content.Gates) > 0 {
		cloned := make([]schema.TicketGate, len(content.Gates))
		copy(cloned, content.Gates)
		content.Gates = cloned
	}
	if len(content.Notes) > 0 {
		cloned := make([]schema.TicketNote, len(content.Notes))
		copy(cloned, content.Notes)
		content.Notes = cloned
	}
	if len(content.Attachments) > 0 {
		cloned := make([]schema.TicketAttachment, len(content.Attachments))
		copy(cloned, content.Attachments)
		content.Attachments = cloned
	}

	idx.tickets[ticketID] = content
	idx.updateIndexes(ticketID, &content, addToStringIndex, addToIntIndex)
	idx.addGateWatches(ticketID, &content)
}

// Remove deletes a ticket from the index and cleans up all secondary
// indexes. No-op if the ticket does not exist.
func (idx *Index) Remove(ticketID string) {
	old, exists := idx.tickets[ticketID]
	if !exists {
		return
	}
	idx.updateIndexes(ticketID, &old, removeFromStringIndex, removeFromIntIndex)
	idx.removeGateWatches(ticketID, &old)
	delete(idx.tickets, ticketID)
}

// Get returns the content of a single ticket. The second return value
// is false if the ticket does not exist.
func (idx *Index) Get(ticketID string) (schema.TicketContent, bool) {
	content, exists := idx.tickets[ticketID]
	return content, exists
}

// Ready returns all actionable tickets: those available to be picked
// up (status "open", all blockers closed, all gates satisfied) and
// those already being worked on (status "in_progress"). Results are
// sorted by priority (ascending, 0=critical first) then by creation
// time (oldest first).
func (idx *Index) Ready() []Entry {
	var result []Entry
	for id, content := range idx.tickets {
		if idx.isReady(&content) || content.Status == "in_progress" {
			result = append(result, Entry{ID: id, Content: content})
		}
	}
	sortEntries(result)
	return result
}

// Blocked returns all tickets that are open but cannot be started:
// they have at least one non-closed blocker or at least one unsatisfied
// gate. Results are sorted by priority then creation time.
func (idx *Index) Blocked() []Entry {
	var result []Entry
	for id, content := range idx.tickets {
		if content.Status != "open" {
			continue
		}
		if !idx.allBlockersClosed(&content) || !allGatesSatisfied(&content) {
			result = append(result, Entry{ID: id, Content: content})
		}
	}
	sortEntries(result)
	return result
}

// List returns tickets matching the given filter. All non-zero filter
// fields must match (AND semantics). An empty filter returns all
// tickets. Results are sorted by priority then creation time.
func (idx *Index) List(filter Filter) []Entry {
	var result []Entry
	for id, content := range idx.tickets {
		if matchesFilter(&content, &filter) {
			result = append(result, Entry{ID: id, Content: content})
		}
	}
	sortEntries(result)
	return result
}

// Grep searches ticket titles, bodies, and note bodies for a regex
// pattern. Returns matching tickets sorted by priority then creation
// time. Returns an error if the pattern is not valid regex.
func (idx *Index) Grep(pattern string) ([]Entry, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid grep pattern: %w", err)
	}

	var result []Entry
	for id, content := range idx.tickets {
		if grepMatches(re, &content) {
			result = append(result, Entry{ID: id, Content: content})
		}
	}
	sortEntries(result)
	return result, nil
}

// Children returns the direct children of a parent ticket (tickets
// whose Parent field equals parentID). Results are sorted by priority
// then creation time.
func (idx *Index) Children(parentID string) []Entry {
	childIDs, exists := idx.children[parentID]
	if !exists {
		return nil
	}
	result := make([]Entry, 0, len(childIDs))
	for childID := range childIDs {
		content, exists := idx.tickets[childID]
		if exists {
			result = append(result, Entry{ID: childID, Content: content})
		}
	}
	sortEntries(result)
	return result
}

// ChildProgress returns a summary of a parent ticket's children:
// total count and how many have status "closed". Useful for progress
// displays like "5 of 8 subtasks closed".
func (idx *Index) ChildProgress(parentID string) (total, closed int) {
	childIDs, exists := idx.children[parentID]
	if !exists {
		return 0, 0
	}
	for childID := range childIDs {
		content, exists := idx.tickets[childID]
		if !exists {
			continue
		}
		total++
		if content.Status == "closed" {
			closed++
		}
	}
	return total, closed
}

// PendingGates returns all tickets that have at least one gate with
// status "pending". Used by timer gate evaluation and cross-room gate
// evaluation, which need to scan all pending gates regardless of event
// criteria. For same-room event-driven evaluation, prefer
// [Index.WatchedGates] which provides O(1) lookup by event type and
// state key. Results are not sorted (the caller iterates gates, not
// display).
func (idx *Index) PendingGates() []Entry {
	var result []Entry
	for id, content := range idx.tickets {
		for i := range content.Gates {
			if content.Gates[i].Status == "pending" {
				result = append(result, Entry{ID: id, Content: content})
				break
			}
		}
	}
	return result
}

// WatchedGates returns pending gates that could match an event with
// the given type and state key. This is an O(1) lookup (per matching
// gate) instead of scanning all tickets with pending gates.
//
// The result includes both exact matches on (eventType, stateKey) and
// wildcard matches on (eventType, "") for gates that watch any state
// key of that event type.
//
// Callers must still run the full match function (e.g., matchGateEvent)
// on each result to verify content-level criteria (pipeline conclusion,
// content_match expressions, etc.) that the watch key doesn't capture.
//
// The returned slice is a snapshot: modifying the index (via Put after
// gate satisfaction) does not affect an in-progress iteration.
func (idx *Index) WatchedGates(eventType, stateKey string) []GateWatch {
	var result []GateWatch

	// Exact match: gates watching for this specific (eventType, stateKey).
	exactKey := gateWatchKey{eventType: eventType, stateKey: stateKey}
	for watch := range idx.gateWatch[exactKey] {
		result = append(result, watch)
	}

	// Wildcard match: gates watching for any state key of this event type.
	// Only check the wildcard key if stateKey is non-empty; when stateKey
	// is empty, the exact key IS the wildcard key and was already checked.
	if stateKey != "" {
		wildcardKey := gateWatchKey{eventType: eventType}
		for watch := range idx.gateWatch[wildcardKey] {
			result = append(result, watch)
		}
	}

	return result
}

// watchKeysForGate computes the watch map keys for a gate. Returns nil
// for gate types that are not event-driven (human, timer) or that are
// handled by a separate evaluation path (cross-room state_event gates).
func watchKeysForGate(gate *schema.TicketGate) []gateWatchKey {
	switch gate.Type {
	case "pipeline":
		// Pipeline gates watch for pipeline_result events with any
		// state key. The fine-grained match on pipeline_ref and
		// conclusion is done by matchGateEvent after the watch map
		// narrows candidates.
		return []gateWatchKey{{eventType: schema.EventTypePipelineResult}}

	case "ticket":
		// Ticket gates watch for a specific ticket's state event to
		// reach status "closed". The state key is the ticket ID.
		if gate.TicketID == "" {
			return nil
		}
		return []gateWatchKey{{eventType: schema.EventTypeTicket, stateKey: gate.TicketID}}

	case "state_event":
		// Cross-room gates (RoomAlias set) are evaluated separately
		// by evaluateCrossRoomGates, not the per-event watch path.
		if gate.RoomAlias != "" {
			return nil
		}
		if gate.EventType == "" {
			return nil
		}
		// When gate.StateKey is empty, the watch key is a wildcard
		// entry matching any state key for gate.EventType.
		return []gateWatchKey{{eventType: gate.EventType, stateKey: gate.StateKey}}

	default:
		// human, timer, and unknown types are not event-driven.
		return nil
	}
}

// addGateWatches registers watch entries for all pending gates on the
// given ticket. Called by Put after inserting the ticket into the
// primary map.
func (idx *Index) addGateWatches(ticketID string, content *schema.TicketContent) {
	for i := range content.Gates {
		if content.Gates[i].Status != "pending" {
			continue
		}
		for _, key := range watchKeysForGate(&content.Gates[i]) {
			watch := GateWatch{TicketID: ticketID, GateIndex: i}
			set, exists := idx.gateWatch[key]
			if !exists {
				set = make(map[GateWatch]struct{})
				idx.gateWatch[key] = set
			}
			set[watch] = struct{}{}
		}
	}
}

// removeGateWatches unregisters watch entries for all gates on the
// given ticket. Called by Put (before replacing) and Remove.
//
// This unconditionally attempts removal for every gate regardless of
// status. When Put is called after modifying a gate's status in-place
// (e.g., marking it "satisfied"), the old content read from the map
// may already reflect the modification due to slice backing-array
// aliasing. Skipping non-pending gates would leave orphaned watch
// entries. Unconditional removal is safe: the delete is a no-op for
// gates that were never in the watch map.
func (idx *Index) removeGateWatches(ticketID string, content *schema.TicketContent) {
	for i := range content.Gates {
		for _, key := range watchKeysForGate(&content.Gates[i]) {
			watch := GateWatch{TicketID: ticketID, GateIndex: i}
			set, exists := idx.gateWatch[key]
			if !exists {
				continue
			}
			delete(set, watch)
			if len(set) == 0 {
				delete(idx.gateWatch, key)
			}
		}
	}
}

// Stats returns aggregate counts across all tickets in the index.
func (idx *Index) Stats() Stats {
	stats := Stats{
		Total:      len(idx.tickets),
		ByStatus:   make(map[string]int),
		ByPriority: make(map[int]int),
		ByType:     make(map[string]int),
	}
	for _, content := range idx.tickets {
		stats.ByStatus[content.Status]++
		stats.ByPriority[content.Priority]++
		stats.ByType[content.Type]++
	}
	return stats
}

// --- Scoring and ranking ---

// UnblockScore computes the number of currently-blocked tickets that
// would become newly ready if ticketID were closed. A ticket counts
// only if ticketID is its sole remaining non-closed blocker and all
// its gates are satisfied. Returns 0 if the ticket has no dependents
// or does not exist.
func (idx *Index) UnblockScore(ticketID string) int {
	dependents, exists := idx.blocks[ticketID]
	if !exists {
		return 0
	}

	count := 0
	for dependentID := range dependents {
		dependent, exists := idx.tickets[dependentID]
		if !exists || dependent.Status != "open" {
			continue
		}
		if !allGatesSatisfied(&dependent) {
			continue
		}

		// Check whether ticketID is the only non-closed blocker.
		otherOpenBlockers := false
		for _, blockerID := range dependent.BlockedBy {
			if blockerID == ticketID {
				continue
			}
			blocker, exists := idx.tickets[blockerID]
			if !exists || blocker.Status != "closed" {
				otherOpenBlockers = true
				break
			}
		}
		if !otherOpenBlockers {
			count++
		}
	}
	return count
}

// BorrowedPriority returns the highest priority (lowest number) among
// all tickets transitively reachable by following blocks edges (reverse
// dependency direction). This captures "downstream urgency": a P3
// ticket that blocks a P0 has borrowed priority 0.
//
// Returns -1 if the ticket has no dependents or does not exist.
func (idx *Index) BorrowedPriority(ticketID string) int {
	dependents, exists := idx.blocks[ticketID]
	if !exists || len(dependents) == 0 {
		return -1
	}

	best := math.MaxInt
	visited := map[string]struct{}{ticketID: {}}
	queue := make([]string, 0, len(dependents))
	for id := range dependents {
		visited[id] = struct{}{}
		queue = append(queue, id)
	}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		content, exists := idx.tickets[current]
		if !exists {
			continue
		}
		if content.Priority < best {
			best = content.Priority
			if best == 0 {
				return 0 // Can't improve beyond critical.
			}
		}

		// Continue traversing along blocks edges (dependents of
		// current) to find transitive downstream priority.
		if nextDependents, exists := idx.blocks[current]; exists {
			for id := range nextDependents {
				if _, seen := visited[id]; !seen {
					visited[id] = struct{}{}
					queue = append(queue, id)
				}
			}
		}
	}

	if best == math.MaxInt {
		return -1
	}
	return best
}

// CriticalDepth computes the longest chain of open blocked_by edges
// among the children of epicID. Only edges between children of the
// same epic are followed. This is the irreducible sequential depth:
// the minimum number of serial dependency hops to close all children,
// regardless of how many agents work in parallel.
//
// Returns 0 if the epic has no children, no open children, or no
// inter-child dependencies.
func (idx *Index) CriticalDepth(epicID string) int {
	childIDs, exists := idx.children[epicID]
	if !exists {
		return 0
	}

	// Build the set of open children for fast membership checks.
	openChildren := make(map[string]struct{})
	for childID := range childIDs {
		content, exists := idx.tickets[childID]
		if exists && content.Status != "closed" {
			openChildren[childID] = struct{}{}
		}
	}

	if len(openChildren) == 0 {
		return 0
	}

	// For each open child, compute the longest path following
	// blocked_by edges restricted to open children of the same epic.
	// Memoize results to avoid redundant traversals.
	memo := make(map[string]int, len(openChildren))
	var depthOf func(string) int
	depthOf = func(ticketID string) int {
		if cached, exists := memo[ticketID]; exists {
			return cached
		}
		// Mark as visited (0) before recursing to handle cycles.
		memo[ticketID] = 0

		content, exists := idx.tickets[ticketID]
		if !exists {
			return 0
		}

		maxBlockerDepth := 0
		for _, blockerID := range content.BlockedBy {
			if _, isOpenChild := openChildren[blockerID]; !isOpenChild {
				continue
			}
			blockerDepth := depthOf(blockerID)
			if blockerDepth+1 > maxBlockerDepth {
				maxBlockerDepth = blockerDepth + 1
			}
		}
		memo[ticketID] = maxBlockerDepth
		return maxBlockerDepth
	}

	maxDepth := 0
	for childID := range openChildren {
		depth := depthOf(childID)
		if depth > maxDepth {
			maxDepth = depth
		}
	}
	return maxDepth
}

// EpicHealth computes health metrics for an epic's children: how many
// are closed, how many are ready, what fraction of remaining work is
// actionable, and the critical path depth.
func (idx *Index) EpicHealth(epicID string) EpicHealthStats {
	childIDs, exists := idx.children[epicID]
	if !exists {
		return EpicHealthStats{}
	}

	var stats EpicHealthStats
	for childID := range childIDs {
		content, exists := idx.tickets[childID]
		if !exists {
			continue
		}
		stats.TotalChildren++
		switch content.Status {
		case "closed":
			stats.ClosedChildren++
		case "in_progress":
			// Counted in active fraction below.
		}
		if idx.isReady(&content) {
			stats.ReadyChildren++
		}
	}

	openCount := stats.TotalChildren - stats.ClosedChildren
	if openCount > 0 {
		// Count in_progress children for active fraction.
		inProgressCount := 0
		for childID := range childIDs {
			content, exists := idx.tickets[childID]
			if exists && content.Status == "in_progress" {
				inProgressCount++
			}
		}
		stats.ActiveFraction = float64(stats.ReadyChildren+inProgressCount) / float64(openCount)
	}

	stats.CriticalDepth = idx.CriticalDepth(epicID)
	return stats
}

// Score computes all ranking dimensions for a single ticket. The now
// parameter is used to compute DaysSinceReady; weights control the
// Composite formula.
func (idx *Index) Score(ticketID string, now time.Time, weights RankWeights) TicketScore {
	weights = weights.applyDefaults()

	content, exists := idx.tickets[ticketID]
	if !exists {
		return TicketScore{BorrowedPriority: -1}
	}

	score := TicketScore{
		UnblockCount:     idx.UnblockScore(ticketID),
		BorrowedPriority: idx.BorrowedPriority(ticketID),
		NoteCount:        len(content.Notes),
	}

	// Compute days since ready.
	readyTimestamp := readySince(&content, idx)
	if readyTimestamp != "" {
		if readyTime, err := time.Parse(time.RFC3339, readyTimestamp); err == nil {
			days := int(now.Sub(readyTime).Hours() / 24)
			if days < 0 {
				days = 0
			}
			score.DaysSinceReady = days
		}
	}

	// Composite: higher = assign first.
	effectivePriority := content.Priority
	if score.BorrowedPriority >= 0 && score.BorrowedPriority < effectivePriority {
		effectivePriority = score.BorrowedPriority
	}

	score.Composite = weights.Leverage*float64(score.UnblockCount) +
		weights.Urgency*float64(4-effectivePriority) +
		weights.Staleness*float64(score.DaysSinceReady) +
		weights.Effort*(1.0/(1.0+float64(score.NoteCount)))

	return score
}

// Ranked returns all ready tickets sorted by composite score
// (descending — highest-value first). Each entry includes the full
// score breakdown so callers can display individual dimensions.
func (idx *Index) Ranked(now time.Time, weights RankWeights) []RankedEntry {
	ready := idx.Ready()
	result := make([]RankedEntry, len(ready))
	for i, entry := range ready {
		result[i] = RankedEntry{
			Entry: entry,
			Score: idx.Score(entry.ID, now, weights),
		}
	}
	slices.SortFunc(result, func(a, b RankedEntry) int {
		// Descending by composite score. Use strict float comparison
		// with a tiebreaker on priority then creation time.
		if a.Score.Composite != b.Score.Composite {
			if a.Score.Composite > b.Score.Composite {
				return -1
			}
			return 1
		}
		if a.Content.Priority != b.Content.Priority {
			return a.Content.Priority - b.Content.Priority
		}
		return strings.Compare(a.Content.CreatedAt, b.Content.CreatedAt)
	})
	return result
}

// readySince returns the ISO 8601 timestamp when the ticket became
// actionable. For tickets with blockers, this is the latest ClosedAt
// among blockers (when the last gate lifted). For tickets without
// blockers, this is CreatedAt (ready from birth).
func readySince(content *schema.TicketContent, idx *Index) string {
	if len(content.BlockedBy) == 0 {
		return content.CreatedAt
	}
	latest := ""
	for _, blockerID := range content.BlockedBy {
		blocker, exists := idx.tickets[blockerID]
		if !exists {
			continue
		}
		if blocker.ClosedAt > latest {
			latest = blocker.ClosedAt
		}
	}
	if latest == "" {
		return content.CreatedAt
	}
	return latest
}

// Deps returns the transitive closure of the dependency graph starting
// from ticketID — all tickets that ticketID transitively depends on
// (following blocked_by edges). The starting ticket is not included in
// the result. Returns nil if the ticket has no dependencies.
func (idx *Index) Deps(ticketID string) []string {
	visited := map[string]struct{}{ticketID: {}}
	queue := []string{ticketID}
	var deps []string

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		content, exists := idx.tickets[current]
		if !exists {
			continue
		}
		for _, blockerID := range content.BlockedBy {
			if _, seen := visited[blockerID]; !seen {
				visited[blockerID] = struct{}{}
				queue = append(queue, blockerID)
				deps = append(deps, blockerID)
			}
		}
	}
	slices.Sort(deps)
	return deps
}

// Blocks returns the ticket IDs that directly depend on ticketID (the
// reverse of blocked_by). Returns nil if nothing depends on this
// ticket.
func (idx *Index) Blocks(ticketID string) []string {
	dependents, exists := idx.blocks[ticketID]
	if !exists {
		return nil
	}
	result := make([]string, 0, len(dependents))
	for id := range dependents {
		result = append(result, id)
	}
	slices.Sort(result)
	return result
}

// WouldCycle returns true if adding the proposed blocked_by edges to
// ticketID would create a cycle in the dependency graph. The ticket
// service calls this before writing to Matrix to reject cyclic
// mutations.
//
// The check works by traversing the existing dependency graph from each
// proposed blocker: if any of them can reach ticketID through existing
// blocked_by edges, adding a new edge from ticketID to that blocker
// would close a cycle.
func (idx *Index) WouldCycle(ticketID string, proposedBlockedBy []string) bool {
	for _, blockerID := range proposedBlockedBy {
		if blockerID == ticketID {
			return true
		}
		if idx.canReach(blockerID, ticketID) {
			return true
		}
	}
	return false
}

// --- Internal helpers ---

// updateIndexes applies a string index operation and int index
// operation to every secondary index for the given ticket. Used by
// Put (with add operations) and Remove (with remove operations).
func (idx *Index) updateIndexes(
	ticketID string,
	content *schema.TicketContent,
	stringOp func(map[string]map[string]struct{}, string, string),
	intOp func(map[int]map[string]struct{}, int, string),
) {
	stringOp(idx.byStatus, content.Status, ticketID)
	intOp(idx.byPriority, content.Priority, ticketID)
	stringOp(idx.byType, content.Type, ticketID)

	for _, label := range content.Labels {
		stringOp(idx.byLabel, label, ticketID)
	}
	if content.Assignee != "" {
		stringOp(idx.byAssignee, content.Assignee, ticketID)
	}
	if content.Parent != "" {
		stringOp(idx.children, content.Parent, ticketID)
	}

	// Dependency graph: forward and reverse edges.
	for _, blockerID := range content.BlockedBy {
		stringOp(idx.blockedBy, ticketID, blockerID)
		stringOp(idx.blocks, blockerID, ticketID)
	}
}

// isReady returns true if the ticket is available to be picked up.
func (idx *Index) isReady(content *schema.TicketContent) bool {
	return content.Status == "open" &&
		idx.allBlockersClosed(content) &&
		allGatesSatisfied(content)
}

// allBlockersClosed returns true if every ticket in content.BlockedBy
// exists in the index and has status "closed". Returns false if any
// blocker is missing (dangling reference) or not closed.
func (idx *Index) allBlockersClosed(content *schema.TicketContent) bool {
	for _, blockerID := range content.BlockedBy {
		blocker, exists := idx.tickets[blockerID]
		if !exists || blocker.Status != "closed" {
			return false
		}
	}
	return true
}

// allGatesSatisfied returns true if every gate has status "satisfied".
func allGatesSatisfied(content *schema.TicketContent) bool {
	for i := range content.Gates {
		if content.Gates[i].Status != "satisfied" {
			return false
		}
	}
	return true
}

// matchesFilter returns true if the ticket matches all non-zero fields
// in the filter.
func matchesFilter(content *schema.TicketContent, filter *Filter) bool {
	if filter.Status != "" && content.Status != filter.Status {
		return false
	}
	if filter.Priority != nil && content.Priority != *filter.Priority {
		return false
	}
	if filter.Label != "" && !slices.Contains(content.Labels, filter.Label) {
		return false
	}
	if filter.Assignee != "" && content.Assignee != filter.Assignee {
		return false
	}
	if filter.Type != "" && content.Type != filter.Type {
		return false
	}
	if filter.Parent != "" && content.Parent != filter.Parent {
		return false
	}
	return true
}

// grepMatches returns true if the regex matches the ticket's title,
// body, or any note body.
func grepMatches(re *regexp.Regexp, content *schema.TicketContent) bool {
	if re.MatchString(content.Title) {
		return true
	}
	if re.MatchString(content.Body) {
		return true
	}
	for i := range content.Notes {
		if re.MatchString(content.Notes[i].Body) {
			return true
		}
	}
	return false
}

// canReach returns true if 'from' can reach 'target' by following
// blocked_by edges through the existing dependency graph.
func (idx *Index) canReach(from, target string) bool {
	visited := map[string]struct{}{from: {}}
	queue := []string{from}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		content, exists := idx.tickets[current]
		if !exists {
			continue
		}
		for _, blockerID := range content.BlockedBy {
			if blockerID == target {
				return true
			}
			if _, seen := visited[blockerID]; !seen {
				visited[blockerID] = struct{}{}
				queue = append(queue, blockerID)
			}
		}
	}
	return false
}

// sortEntries sorts by priority ascending (0=critical first), then by
// CreatedAt ascending (oldest first). Since CreatedAt is ISO 8601,
// string comparison provides correct chronological ordering.
func sortEntries(entries []Entry) {
	slices.SortFunc(entries, func(a, b Entry) int {
		if a.Content.Priority != b.Content.Priority {
			return a.Content.Priority - b.Content.Priority
		}
		return strings.Compare(a.Content.CreatedAt, b.Content.CreatedAt)
	})
}

// --- Generic index helpers ---

func addToStringIndex(index map[string]map[string]struct{}, key, value string) {
	set, exists := index[key]
	if !exists {
		set = make(map[string]struct{})
		index[key] = set
	}
	set[value] = struct{}{}
}

func removeFromStringIndex(index map[string]map[string]struct{}, key, value string) {
	set, exists := index[key]
	if !exists {
		return
	}
	delete(set, value)
	if len(set) == 0 {
		delete(index, key)
	}
}

func addToIntIndex(index map[int]map[string]struct{}, key int, value string) {
	set, exists := index[key]
	if !exists {
		set = make(map[string]struct{})
		index[key] = set
	}
	set[value] = struct{}{}
}

func removeFromIntIndex(index map[int]map[string]struct{}, key int, value string) {
	set, exists := index[key]
	if !exists {
		return
	}
	delete(set, value)
	if len(set) == 0 {
		delete(index, key)
	}
}
