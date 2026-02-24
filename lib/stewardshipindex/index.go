// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package stewardshipindex

import (
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/stewardship"
)

// Declaration is a stewardship declaration stored in the index with
// its room context. Each declaration corresponds to a single
// m.bureau.stewardship state event in a specific room.
type Declaration struct {
	// RoomID is the Matrix room containing this declaration.
	RoomID ref.RoomID

	// StateKey is the Matrix state key identifying this declaration
	// within the room (e.g., "fleet/gpu", "workspace/lib/schema").
	StateKey string

	// Content is the stewardship declaration content.
	Content stewardship.StewardshipContent
}

// Match represents a resolved match between a ticket's resource
// identifier and a stewardship declaration's resource pattern.
type Match struct {
	// Declaration is the matched stewardship declaration.
	Declaration Declaration

	// MatchedPattern is the specific resource pattern from the
	// declaration's ResourcePatterns that produced this match.
	MatchedPattern string

	// MatchedResource is the specific resource identifier from
	// the ticket's Affects field that triggered this match.
	MatchedResource string
}

// declarationKey uniquely identifies a stewardship declaration by
// its room and state key. Used as a map key in the index.
type declarationKey struct {
	roomID   ref.RoomID
	stateKey string
}

// Index is an in-memory index of stewardship declarations across all
// rooms the ticket service is a member of. It supports fast resolution
// of ticket Affects entries against stewardship ResourcePatterns using
// hierarchical glob matching.
//
// The index is maintained incrementally: the ticket service calls Put
// and Remove as stewardship state events arrive via /sync. Resolution
// is a linear scan over all declarations — sub-microsecond for any
// realistic deployment size (~50 declarations).
//
// Construct with [NewIndex]. Not safe for concurrent use.
type Index struct {
	declarations map[declarationKey]Declaration
}

// NewIndex returns an empty index ready for use.
func NewIndex() *Index {
	return &Index{
		declarations: make(map[declarationKey]Declaration),
	}
}

// Len returns the total number of stewardship declarations in the
// index across all rooms.
func (idx *Index) Len() int {
	return len(idx.declarations)
}

// Put adds or updates a stewardship declaration in the index. If a
// declaration with the same (roomID, stateKey) already exists, it is
// replaced. Put does not validate the content — the caller is
// responsible for validation before writing to Matrix.
func (idx *Index) Put(roomID ref.RoomID, stateKey string, content stewardship.StewardshipContent) {
	key := declarationKey{roomID: roomID, stateKey: stateKey}
	idx.declarations[key] = Declaration{
		RoomID:   roomID,
		StateKey: stateKey,
		Content:  content,
	}
}

// Remove deletes a stewardship declaration from the index. No-op if
// the declaration does not exist.
func (idx *Index) Remove(roomID ref.RoomID, stateKey string) {
	key := declarationKey{roomID: roomID, stateKey: stateKey}
	delete(idx.declarations, key)
}

// Resolve finds all stewardship declarations whose ResourcePatterns
// match any of the given resource identifiers. Returns nil if affects
// is empty or no declarations match.
//
// Each match records which specific pattern and resource produced the
// match. A single declaration can produce multiple matches if multiple
// patterns match multiple resources. The caller is responsible for
// grouping matches by declaration when building review gates.
//
// Matching uses principal.MatchPattern, the same hierarchical
// slash-separated glob engine used by authorization grant targets.
func (idx *Index) Resolve(affects []string) []Match {
	if len(affects) == 0 {
		return nil
	}
	var matches []Match
	for _, declaration := range idx.declarations {
		matches = idx.matchDeclaration(declaration, affects, matches)
	}
	return matches
}

// ResolveForRoom finds matching declarations scoped to a single room.
// Same semantics as Resolve but only considers declarations in the
// specified room.
func (idx *Index) ResolveForRoom(roomID ref.RoomID, affects []string) []Match {
	if len(affects) == 0 {
		return nil
	}
	var matches []Match
	for key, declaration := range idx.declarations {
		if key.roomID != roomID {
			continue
		}
		matches = idx.matchDeclaration(declaration, affects, matches)
	}
	return matches
}

// DeclarationsInRoom returns all declarations in the given room.
// Returns nil if the room has no declarations.
func (idx *Index) DeclarationsInRoom(roomID ref.RoomID) []Declaration {
	var result []Declaration
	for key, declaration := range idx.declarations {
		if key.roomID == roomID {
			result = append(result, declaration)
		}
	}
	return result
}

// matchDeclaration checks whether any of the declaration's
// ResourcePatterns match any of the given resource identifiers.
// Appends matches to the provided slice and returns it.
func (idx *Index) matchDeclaration(declaration Declaration, affects []string, matches []Match) []Match {
	for _, resource := range affects {
		for _, pattern := range declaration.Content.ResourcePatterns {
			if principal.MatchPattern(pattern, resource) {
				matches = append(matches, Match{
					Declaration:     declaration,
					MatchedPattern:  pattern,
					MatchedResource: resource,
				})
			}
		}
	}
	return matches
}
