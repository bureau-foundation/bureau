// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package authorization

import (
	"sort"
	"sync"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema"
)

// temporalEntry tracks a temporal grant alongside its expiry time and
// owning principal for efficient sweep operations.
type temporalEntry struct {
	// expiresAt is the parsed expiry time of the grant.
	expiresAt time.Time

	// principal is the localpart of the principal who holds this grant.
	principal string

	// grant is the temporal grant itself.
	grant schema.Grant
}

// Index holds per-principal resolved authorization policies and supports
// concurrent read access with single-writer updates. The daemon builds
// the index from machine config, room authorization policies, and
// per-principal assignments.
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
	grants           map[string][]schema.Grant
	denials          map[string][]schema.Denial
	allowances       map[string][]schema.Allowance
	allowanceDenials map[string][]schema.AllowanceDenial

	// Temporal grants indexed by expiry for efficient GC.
	temporalGrants []temporalEntry
}

// NewIndex creates an empty authorization index.
func NewIndex() *Index {
	return &Index{
		grants:           make(map[string][]schema.Grant),
		denials:          make(map[string][]schema.Denial),
		allowances:       make(map[string][]schema.Allowance),
		allowanceDenials: make(map[string][]schema.AllowanceDenial),
	}
}

// SetPrincipal replaces the resolved policy for a principal. The caller
// is responsible for merging machine defaults, room-level grants, and
// per-principal policies before calling this. Temporal grants for this
// principal are preserved — they are managed separately via
// AddTemporalGrant/RevokeTemporalGrant.
func (idx *Index) SetPrincipal(localpart string, policy schema.AuthorizationPolicy) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	// Merge temporal grants into the grant list.
	grants := make([]schema.Grant, len(policy.Grants))
	copy(grants, policy.Grants)
	for _, entry := range idx.temporalGrants {
		if entry.principal == localpart {
			grants = append(grants, entry.grant)
		}
	}

	idx.grants[localpart] = grants
	idx.denials[localpart] = policy.Denials
	idx.allowances[localpart] = policy.Allowances
	idx.allowanceDenials[localpart] = policy.AllowanceDenials
}

// RemovePrincipal removes all policy for a principal. Also removes any
// temporal grants held by this principal.
func (idx *Index) RemovePrincipal(localpart string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	delete(idx.grants, localpart)
	delete(idx.denials, localpart)
	delete(idx.allowances, localpart)
	delete(idx.allowanceDenials, localpart)

	// Remove temporal grants for this principal.
	filtered := idx.temporalGrants[:0]
	for _, entry := range idx.temporalGrants {
		if entry.principal != localpart {
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
func (idx *Index) AddTemporalGrant(localpart string, grant schema.Grant) bool {
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
		principal: localpart,
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
	idx.grants[localpart] = append(idx.grants[localpart], grant)

	return true
}

// RevokeTemporalGrant removes all temporal grants for a principal that
// are linked to a specific ticket. Returns the number of grants removed.
func (idx *Index) RevokeTemporalGrant(localpart, ticket string) int {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	removed := 0

	// Remove from temporal grant tracking.
	filtered := idx.temporalGrants[:0]
	for _, entry := range idx.temporalGrants {
		if entry.principal == localpart && entry.grant.Ticket == ticket {
			removed++
			continue
		}
		filtered = append(filtered, entry)
	}
	idx.temporalGrants = filtered

	// Rebuild the principal's grant list without the revoked grants.
	if removed > 0 {
		grants := idx.grants[localpart]
		rebuilt := make([]schema.Grant, 0, len(grants))
		for _, grant := range grants {
			if grant.Ticket == ticket {
				continue
			}
			rebuilt = append(rebuilt, grant)
		}
		idx.grants[localpart] = rebuilt
	}

	return removed
}

// SweepExpired removes temporal grants that have expired as of the
// given time. Returns the localparts of principals whose grants changed.
func (idx *Index) SweepExpired(now time.Time) []string {
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
	affectedSet := make(map[string]bool)
	expiredTickets := make(map[string]map[string]bool) // principal → set of Ticket values
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
	for localpart, ticketSet := range expiredTickets {
		grants := idx.grants[localpart]
		rebuilt := make([]schema.Grant, 0, len(grants))
		for _, grant := range grants {
			if grant.Ticket != "" && ticketSet[grant.Ticket] {
				continue
			}
			rebuilt = append(rebuilt, grant)
		}
		idx.grants[localpart] = rebuilt
	}

	affected := make([]string, 0, len(affectedSet))
	for localpart := range affectedSet {
		affected = append(affected, localpart)
	}
	sort.Strings(affected)
	return affected
}

// Grants returns a deep copy of the resolved grants for a principal.
// Returns nil if the principal is not in the index.
func (idx *Index) Grants(localpart string) []schema.Grant {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	grants := idx.grants[localpart]
	if grants == nil {
		return nil
	}
	return copyGrants(grants)
}

// Allowances returns a deep copy of the resolved allowances for a
// principal. Returns nil if the principal is not in the index.
func (idx *Index) Allowances(localpart string) []schema.Allowance {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	allowances := idx.allowances[localpart]
	if allowances == nil {
		return nil
	}
	return copyAllowances(allowances)
}

// copyGrants returns a deep copy of a grant slice. Inner string slices
// are copied so the caller cannot mutate the index's data.
func copyGrants(grants []schema.Grant) []schema.Grant {
	result := make([]schema.Grant, len(grants))
	for i, grant := range grants {
		result[i] = schema.Grant{
			ExpiresAt: grant.ExpiresAt,
			Ticket:    grant.Ticket,
			GrantedBy: grant.GrantedBy,
			GrantedAt: grant.GrantedAt,
		}
		if grant.Actions != nil {
			result[i].Actions = make([]string, len(grant.Actions))
			copy(result[i].Actions, grant.Actions)
		}
		if grant.Targets != nil {
			result[i].Targets = make([]string, len(grant.Targets))
			copy(result[i].Targets, grant.Targets)
		}
	}
	return result
}

// copyAllowances returns a deep copy of an allowance slice.
func copyAllowances(allowances []schema.Allowance) []schema.Allowance {
	result := make([]schema.Allowance, len(allowances))
	for i, allowance := range allowances {
		if allowance.Actions != nil {
			result[i].Actions = make([]string, len(allowance.Actions))
			copy(result[i].Actions, allowance.Actions)
		}
		if allowance.Actors != nil {
			result[i].Actors = make([]string, len(allowance.Actors))
			copy(result[i].Actors, allowance.Actors)
		}
	}
	return result
}
