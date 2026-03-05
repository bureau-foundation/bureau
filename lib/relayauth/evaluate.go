// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package relayauth

import (
	"fmt"
	"strings"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// SourceRoom identifies a room attempting to relay a ticket into an
// ops room. The caller populates this from sync state or alias
// resolution.
type SourceRoom struct {
	// RoomID is the Matrix room ID of the source room.
	RoomID ref.RoomID

	// Alias is the source room's canonical alias. May be zero if the
	// room has no canonical alias set. Fleet and namespace matching
	// require an alias — rooms without aliases can only match via
	// explicit room ID sources.
	Alias ref.RoomAlias
}

// Result holds the outcome of relay policy evaluation.
type Result struct {
	// Authorized is true when the source room is permitted to relay
	// a ticket of the given type into the ops room.
	Authorized bool

	// DenialReason explains why authorization was denied. Empty when
	// Authorized is true.
	DenialReason string

	// MatchedSource is the first RelaySource that authorized the
	// request. Nil when Authorized is false.
	MatchedSource *schema.RelaySource

	// OutboundFilter specifies which fields cross the relay boundary
	// when mirroring ops room state back to the workspace ticket.
	// Nil means the caller should use default filtering (status and
	// close_reason only). Copied from the policy.
	OutboundFilter *schema.RelayFilter
}

// Evaluate checks whether a source room is authorized by the given
// relay policy to relay a ticket of the specified type into an ops
// room.
//
// If policy is nil, authorization is denied — a missing policy means
// the ops room has not been configured for relay (default-deny).
//
// AllowedTypes filtering is applied first: if the policy restricts
// ticket types and the given type is not in the list, authorization
// is denied before checking sources. An empty AllowedTypes list
// permits all ticket types.
//
// Sources are checked in order. The first matching source authorizes
// the request. If no source matches, authorization is denied.
func Evaluate(policy *schema.RelayPolicy, source SourceRoom, ticketType string) Result {
	if policy == nil {
		return Result{
			DenialReason: "no relay policy published in ops room",
		}
	}

	if len(policy.Sources) == 0 {
		return Result{
			DenialReason: "relay policy has no authorized sources",
		}
	}

	// Check ticket type restrictions before evaluating sources.
	if len(policy.AllowedTypes) > 0 {
		if !containsString(policy.AllowedTypes, ticketType) {
			return Result{
				DenialReason: fmt.Sprintf(
					"ticket type %q is not permitted by relay policy (allowed: %s)",
					ticketType,
					strings.Join(policy.AllowedTypes, ", "),
				),
			}
		}
	}

	// Extract source room alias localpart for prefix matching.
	// Empty when the source room has no canonical alias.
	sourceLocalpart := ""
	if !source.Alias.IsZero() {
		sourceLocalpart = source.Alias.Localpart()
	}

	for index := range policy.Sources {
		src := &policy.Sources[index]
		if matchesSource(src, source.RoomID, sourceLocalpart) {
			return Result{
				Authorized:     true,
				MatchedSource:  src,
				OutboundFilter: policy.OutboundFilter,
			}
		}
	}

	return Result{
		DenialReason: "no relay source matched the origin room",
	}
}

// matchesSource checks whether the given room matches a single
// RelaySource entry. Matching is fail-closed: unknown match
// strategies never authorize.
func matchesSource(source *schema.RelaySource, roomID ref.RoomID, sourceLocalpart string) bool {
	switch source.Match {
	case schema.RelayMatchRoom:
		// Exact room ID comparison. No alias needed.
		return roomID == source.Room

	case schema.RelayMatchFleetMember:
		if sourceLocalpart == "" {
			return false
		}
		// A room belongs to a fleet if its alias localpart starts with
		// the fleet localpart followed by "/". For example, localpart
		// "my_bureau/fleet/prod/workspace/feature-x" matches fleet
		// "my_bureau/fleet/prod" because it starts with
		// "my_bureau/fleet/prod/".
		return strings.HasPrefix(sourceLocalpart, source.Fleet+"/")

	case schema.RelayMatchNamespace:
		if sourceLocalpart == "" {
			return false
		}
		// A room belongs to a namespace if its alias localpart starts
		// with the namespace name followed by "/". For example,
		// localpart "my_bureau/fleet/prod/workspace/x" matches
		// namespace "my_bureau" because it starts with "my_bureau/".
		return strings.HasPrefix(sourceLocalpart, source.Namespace+"/")

	default:
		// Unknown match strategy — fail closed.
		return false
	}
}

// containsString reports whether the slice contains the target string.
func containsString(slice []string, target string) bool {
	for _, element := range slice {
		if element == target {
			return true
		}
	}
	return false
}
