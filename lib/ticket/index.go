// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

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
	Total      int
	ByStatus   map[string]int
	ByPriority map[int]int
	ByType     map[string]int
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
	}
	idx.tickets[ticketID] = content
	idx.updateIndexes(ticketID, &content, addToStringIndex, addToIntIndex)
}

// Remove deletes a ticket from the index and cleans up all secondary
// indexes. No-op if the ticket does not exist.
func (idx *Index) Remove(ticketID string) {
	old, exists := idx.tickets[ticketID]
	if !exists {
		return
	}
	idx.updateIndexes(ticketID, &old, removeFromStringIndex, removeFromIntIndex)
	delete(idx.tickets, ticketID)
}

// Get returns the content of a single ticket. The second return value
// is false if the ticket does not exist.
func (idx *Index) Get(ticketID string) (schema.TicketContent, bool) {
	content, exists := idx.tickets[ticketID]
	return content, exists
}

// Ready returns all tickets that are available to be picked up: status
// is "open", all blocked_by tickets are closed, and all gates are
// satisfied. Results are sorted by priority (ascending, 0=critical
// first) then by creation time (oldest first).
func (idx *Index) Ready() []Entry {
	var result []Entry
	for id, content := range idx.tickets {
		if idx.isReady(&content) {
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
// status "pending". The gate evaluator in the ticket service uses
// this to know which tickets to check when a state event arrives.
// Results are not sorted (the caller iterates gates, not display).
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
