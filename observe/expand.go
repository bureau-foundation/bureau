// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import (
	"github.com/bureau-foundation/bureau/lib/schema/observation"
)

// RoomMember represents a room member for observe_members pane expansion.
// The daemon populates these from Matrix room membership queries and passes
// them to ExpandMembers.
type RoomMember struct {
	// Localpart is the principal's localpart (e.g., "iree/amdgpu/pm").
	Localpart string

	// Labels are the principal's organizational metadata from its
	// PrincipalAssignment. Used to filter against MemberFilter.Labels.
	// May be nil if the daemon does not have config for this principal
	// (e.g., a cross-machine member whose labels are unknown locally).
	Labels map[string]string
}

// ExpandMembers replaces ObserveMembers panes in the layout with concrete
// Observe panes, one per matching room member. The original layout is not
// modified; a new layout is returned.
//
// For each ObserveMembers pane:
//   - If MemberFilter.Labels is set, only members whose Labels contain all
//     specified key-value pairs (subset match) are included. If empty/nil,
//     all members are included.
//   - Each matching member becomes an Observe pane with the member's
//     localpart.
//   - The first expanded pane inherits the ObserveMembers pane's Split
//     and Size. Subsequent panes use the same split direction with no
//     explicit size (tmux divides evenly).
//   - If no members match, the ObserveMembers pane is removed entirely
//     (not replaced with an empty pane).
//
// Panes that are not ObserveMembers pass through unchanged.
func ExpandMembers(layout *Layout, members []RoomMember) *Layout {
	if layout == nil {
		return nil
	}

	expanded := &Layout{
		Prefix:  layout.Prefix,
		Windows: make([]Window, 0, len(layout.Windows)),
	}

	for _, window := range layout.Windows {
		expandedWindow := Window{
			Name:  window.Name,
			Panes: make([]Pane, 0, len(window.Panes)),
		}

		for _, pane := range window.Panes {
			if pane.ObserveMembers == nil {
				expandedWindow.Panes = append(expandedWindow.Panes, pane)
				continue
			}

			matching := filterMembers(members, pane.ObserveMembers)
			for memberIndex, member := range matching {
				memberPane := Pane{
					Observe: member.Localpart,
				}
				if memberIndex == 0 {
					// First expanded pane inherits the original
					// pane's position (split direction and size).
					memberPane.Split = pane.Split
					memberPane.Size = pane.Size
				} else {
					// Subsequent panes split in the same direction
					// as the original, with no explicit size (tmux
					// divides the remaining space evenly).
					memberPane.Split = pane.Split
					if memberPane.Split == "" {
						// The ObserveMembers pane was the first pane
						// in the window (no split direction). Default
						// to vertical stacking for multi-member views.
						memberPane.Split = observation.SplitVertical
					}
				}
				expandedWindow.Panes = append(expandedWindow.Panes, memberPane)
			}
		}

		// Only include windows that have at least one pane after expansion.
		// An ObserveMembers pane with zero matching members could leave an
		// empty window.
		if len(expandedWindow.Panes) > 0 {
			expanded.Windows = append(expanded.Windows, expandedWindow)
		}
	}

	return expanded
}

// filterMembers returns the subset of members that match the filter criteria.
// An empty/nil filter labels map matches all members. When filter labels are
// set, a member matches only if its labels contain every key-value pair in
// the filter (subset match).
func filterMembers(members []RoomMember, filter *MemberFilter) []RoomMember {
	if len(filter.Labels) == 0 {
		return members
	}

	var matching []RoomMember
	for _, member := range members {
		if labelsMatch(member.Labels, filter.Labels) {
			matching = append(matching, member)
		}
	}
	return matching
}

// labelsMatch reports whether the member's labels contain all the required
// key-value pairs. A nil member labels map only matches if required is empty.
func labelsMatch(memberLabels, required map[string]string) bool {
	for key, requiredValue := range required {
		if memberValue, ok := memberLabels[key]; !ok || memberValue != requiredValue {
			return false
		}
	}
	return true
}
