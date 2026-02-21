// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package authorization

import (
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// temporalEntry tracks a temporal grant alongside its expiry time and
// owning principal for efficient sweep operations.
type temporalEntry struct {
	// expiresAt is the parsed expiry time of the grant.
	expiresAt time.Time

	// principal is the full Matrix user ID of the principal who holds
	// this grant.
	principal ref.UserID

	// grant is the temporal grant itself.
	grant schema.Grant
}

// Index holds per-principal resolved authorization policies and supports
// concurrent read access with single-writer updates. The daemon builds
// the index from machine config, room authorization policies, and
// per-principal assignments.
//
// Principals are identified by their full Matrix user ID (ref.UserID).
// This is critical for federation safety: @agent/pm:server-a and
// @agent/pm:server-b are different entities with independent policies.
//
// Read operations (Authorized, Grants, Allowances) acquire a read lock.
// Write operations (SetPrincipal, RemovePrincipal, AddTemporalGrant,
// etc.) acquire a write lock. The index is designed for high read
// frequency and low write frequency (updates arrive via /sync, typically
// a few per second; authorization checks happen on every observation
// request, service discovery query, etc.).
type Index struct {
	mu sync.RWMutex

	// Per-principal resolved policy (merged from all sources).
	// Keyed by full Matrix user ID (ref.UserID).
	grants           map[ref.UserID][]schema.Grant
	denials          map[ref.UserID][]schema.Denial
	allowances       map[ref.UserID][]schema.Allowance
	allowanceDenials map[ref.UserID][]schema.AllowanceDenial

	// Temporal grants indexed by expiry for efficient GC.
	temporalGrants []temporalEntry
}

// NewIndex creates an empty authorization index.
func NewIndex() *Index {
	return &Index{
		grants:           make(map[ref.UserID][]schema.Grant),
		denials:          make(map[ref.UserID][]schema.Denial),
		allowances:       make(map[ref.UserID][]schema.Allowance),
		allowanceDenials: make(map[ref.UserID][]schema.AllowanceDenial),
	}
}

// SetPrincipal replaces the resolved policy for a principal. The caller
// is responsible for merging machine defaults, room-level grants, and
// per-principal policies before calling this. Temporal grants for this
// principal are preserved — they are managed separately via
// AddTemporalGrant/RevokeTemporalGrant.
func (idx *Index) SetPrincipal(userID ref.UserID, policy schema.AuthorizationPolicy) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	// Merge temporal grants into the grant list.
	grants := make([]schema.Grant, len(policy.Grants))
	copy(grants, policy.Grants)
	for _, entry := range idx.temporalGrants {
		if entry.principal == userID {
			grants = append(grants, entry.grant)
		}
	}

	idx.grants[userID] = grants
	idx.denials[userID] = policy.Denials
	idx.allowances[userID] = policy.Allowances
	idx.allowanceDenials[userID] = policy.AllowanceDenials
}

// RemovePrincipal removes all policy for a principal. Also removes any
// temporal grants held by this principal.
func (idx *Index) RemovePrincipal(userID ref.UserID) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	delete(idx.grants, userID)
	delete(idx.denials, userID)
	delete(idx.allowances, userID)
	delete(idx.allowanceDenials, userID)

	// Remove temporal grants for this principal.
	filtered := idx.temporalGrants[:0]
	for _, entry := range idx.temporalGrants {
		if entry.principal != userID {
			filtered = append(filtered, entry)
		}
	}
	idx.temporalGrants = filtered
}

// AddTemporalGrant adds a time-bounded grant for a principal. The grant
// is merged into the principal's resolved grants immediately. Returns
// false if the grant has no ExpiresAt, no Ticket, or the expiry cannot
// be parsed. The Ticket field is required because SweepExpired and
// RevokeTemporalGrant use it to identify which grants to remove.
func (idx *Index) AddTemporalGrant(userID ref.UserID, grant schema.Grant) bool {
	if grant.ExpiresAt == "" {
		return false
	}
	if grant.Ticket == "" {
		return false
	}
	expiresAt, err := time.Parse(time.RFC3339, grant.ExpiresAt)
	if err != nil {
		return false
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()

	entry := temporalEntry{
		expiresAt: expiresAt,
		principal: userID,
		grant:     grant,
	}

	// Insert sorted by expiry.
	position := sort.Search(len(idx.temporalGrants), func(i int) bool {
		return idx.temporalGrants[i].expiresAt.After(expiresAt)
	})
	idx.temporalGrants = append(idx.temporalGrants, temporalEntry{})
	copy(idx.temporalGrants[position+1:], idx.temporalGrants[position:])
	idx.temporalGrants[position] = entry

	// Add to the principal's grant list.
	idx.grants[userID] = append(idx.grants[userID], grant)

	return true
}

// RevokeTemporalGrant removes all temporal grants for a principal that
// are linked to a specific ticket. Returns the number of grants removed.
func (idx *Index) RevokeTemporalGrant(userID ref.UserID, ticket string) int {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	removed := 0

	// Remove from temporal grant tracking.
	filtered := idx.temporalGrants[:0]
	for _, entry := range idx.temporalGrants {
		if entry.principal == userID && entry.grant.Ticket == ticket {
			removed++
			continue
		}
		filtered = append(filtered, entry)
	}
	idx.temporalGrants = filtered

	// Rebuild the principal's grant list without the revoked grants.
	if removed > 0 {
		grants := idx.grants[userID]
		rebuilt := make([]schema.Grant, 0, len(grants))
		for _, grant := range grants {
			if grant.Ticket == ticket {
				continue
			}
			rebuilt = append(rebuilt, grant)
		}
		idx.grants[userID] = rebuilt
	}

	return removed
}

// SweepExpired removes temporal grants that have expired as of the
// given time. Returns the user IDs of principals whose grants changed.
func (idx *Index) SweepExpired(now time.Time) []ref.UserID {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	if len(idx.temporalGrants) == 0 {
		return nil
	}

	// Find how many entries have expired. Since the list is sorted by
	// expiry, we scan from the front.
	expiredCount := 0
	for _, entry := range idx.temporalGrants {
		if entry.expiresAt.After(now) {
			break
		}
		expiredCount++
	}

	if expiredCount == 0 {
		return nil
	}

	// Collect affected principals and the expired grants' ticket
	// references. We match by Ticket (not ExpiresAt) to avoid
	// accidentally removing static grants that happen to share an
	// expiry timestamp.
	affectedSet := make(map[ref.UserID]bool)
	expiredTickets := make(map[ref.UserID]map[string]bool) // principal → set of Ticket values
	for i := 0; i < expiredCount; i++ {
		entry := idx.temporalGrants[i]
		affectedSet[entry.principal] = true
		if expiredTickets[entry.principal] == nil {
			expiredTickets[entry.principal] = make(map[string]bool)
		}
		expiredTickets[entry.principal][entry.grant.Ticket] = true
	}

	// Remove expired entries from tracking.
	idx.temporalGrants = idx.temporalGrants[expiredCount:]

	// Rebuild grant lists for affected principals, removing only
	// grants whose Ticket matches an expired temporal entry.
	for userID, ticketSet := range expiredTickets {
		grants := idx.grants[userID]
		rebuilt := make([]schema.Grant, 0, len(grants))
		for _, grant := range grants {
			if grant.Ticket != "" && ticketSet[grant.Ticket] {
				continue
			}
			rebuilt = append(rebuilt, grant)
		}
		idx.grants[userID] = rebuilt
	}

	affected := make([]ref.UserID, 0, len(affectedSet))
	for userID := range affectedSet {
		affected = append(affected, userID)
	}
	sort.Slice(affected, func(i, j int) bool {
		return affected[i].String() < affected[j].String()
	})
	return affected
}

// Principals returns the user IDs of all principals in the index.
// The order is not guaranteed.
func (idx *Index) Principals() []ref.UserID {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	result := make([]ref.UserID, 0, len(idx.grants))
	for userID := range idx.grants {
		result = append(result, userID)
	}
	return result
}

// Grants returns a deep copy of the resolved grants for a principal.
// Returns nil if the principal is not in the index.
func (idx *Index) Grants(userID ref.UserID) []schema.Grant {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	grants := idx.grants[userID]
	if grants == nil {
		return nil
	}
	return copyGrants(grants)
}

// Allowances returns a deep copy of the resolved allowances for a
// principal. Returns nil if the principal is not in the index.
func (idx *Index) Allowances(userID ref.UserID) []schema.Allowance {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	allowances := idx.allowances[userID]
	if allowances == nil {
		return nil
	}
	return copyAllowances(allowances)
}

// Denials returns a deep copy of the resolved denials for a principal.
// Returns nil if the principal is not in the index.
func (idx *Index) Denials(userID ref.UserID) []schema.Denial {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	denials := idx.denials[userID]
	if denials == nil {
		return nil
	}
	return copyDenials(denials)
}

// AllowanceDenials returns a deep copy of the resolved allowance denials
// for a principal. Returns nil if the principal is not in the index.
func (idx *Index) AllowanceDenials(userID ref.UserID) []schema.AllowanceDenial {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	allowanceDenials := idx.allowanceDenials[userID]
	if allowanceDenials == nil {
		return nil
	}
	return copyAllowanceDenials(allowanceDenials)
}

// copyGrants returns a deep copy of a grant slice. Inner string slices
// are cloned so the caller cannot mutate the index's data.
func copyGrants(grants []schema.Grant) []schema.Grant {
	result := make([]schema.Grant, len(grants))
	for i, grant := range grants {
		grant.Actions = slices.Clone(grant.Actions)
		grant.Targets = slices.Clone(grant.Targets)
		result[i] = grant
	}
	return result
}

// copyAllowances returns a deep copy of an allowance slice.
func copyAllowances(allowances []schema.Allowance) []schema.Allowance {
	result := make([]schema.Allowance, len(allowances))
	for i, allowance := range allowances {
		allowance.Actions = slices.Clone(allowance.Actions)
		allowance.Actors = slices.Clone(allowance.Actors)
		result[i] = allowance
	}
	return result
}

// copyDenials returns a deep copy of a denial slice.
func copyDenials(denials []schema.Denial) []schema.Denial {
	result := make([]schema.Denial, len(denials))
	for i, denial := range denials {
		denial.Actions = slices.Clone(denial.Actions)
		denial.Targets = slices.Clone(denial.Targets)
		result[i] = denial
	}
	return result
}

// copyAllowanceDenials returns a deep copy of an allowance denial slice.
func copyAllowanceDenials(denials []schema.AllowanceDenial) []schema.AllowanceDenial {
	result := make([]schema.AllowanceDenial, len(denials))
	for i, denial := range denials {
		denial.Actions = slices.Clone(denial.Actions)
		denial.Actors = slices.Clone(denial.Actors)
		result[i] = denial
	}
	return result
}
