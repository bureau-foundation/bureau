// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/tui"
)

// Re-export dropdown types from the shared TUI library.

// DropdownOption is a single selectable item in a dropdown overlay.
type DropdownOption = tui.DropdownOption

// DropdownOverlay renders a floating menu anchored at a screen position.
type DropdownOverlay = tui.DropdownOverlay

// dropdownSelectMsg is sent when the user selects a dropdown option.
// The model handles this message to dispatch the mutation.
type dropdownSelectMsg struct {
	field    string
	ticketID string
	value    string
}

// StatusTransitions returns the valid status transitions for a given
// current status. Each transition is a DropdownOption with the status
// value as both label and value.
func StatusTransitions(currentStatus string) []DropdownOption {
	switch currentStatus {
	case "open":
		return []DropdownOption{
			{Label: "IN_PROGRESS", Value: "in_progress"},
			{Label: "CLOSED", Value: "closed"},
		}
	case "in_progress":
		return []DropdownOption{
			{Label: "OPEN", Value: "open"},
			{Label: "BLOCKED", Value: "blocked"},
			{Label: "CLOSED", Value: "closed"},
		}
	case "blocked":
		return []DropdownOption{
			{Label: "OPEN", Value: "open"},
			{Label: "IN_PROGRESS", Value: "in_progress"},
			{Label: "CLOSED", Value: "closed"},
		}
	case "closed":
		return []DropdownOption{
			{Label: "OPEN", Value: "open"},
		}
	default:
		return nil
	}
}

// PriorityOptions returns dropdown options for priority selection,
// P0 through P4.
func PriorityOptions() []DropdownOption {
	return []DropdownOption{
		{Label: "P0", Value: "0"},
		{Label: "P1", Value: "1"},
		{Label: "P2", Value: "2"},
		{Label: "P3", Value: "3"},
		{Label: "P4", Value: "4"},
	}
}

// AssigneeOptions builds dropdown options from a member list for the
// assignee picker. Each member is displayed with a presence indicator
// dot (green for online, yellow for unavailable, dim for offline or
// unknown). If the ticket currently has an assignee, an "Unassign"
// option is prepended. The returned cursor is pre-positioned on the
// current assignee if one exists, or 0 otherwise.
func AssigneeOptions(members []MemberInfo, currentAssignee ref.UserID) (options []DropdownOption, cursor int) {
	if !currentAssignee.IsZero() {
		options = append(options, DropdownOption{
			Label: "  Unassign",
			Value: "",
		})
	}

	for _, member := range members {
		dot := "○" // dim dot for offline/unknown
		switch member.Presence {
		case "online":
			dot = "●" // filled dot for online
		case "unavailable":
			dot = "◐" // half dot for unavailable
		}

		// Show @localpart rather than full Matrix ID for readability.
		displayName := member.DisplayName
		if displayName == "" {
			displayName = member.UserID.Localpart()
		}

		label := dot + " " + displayName
		options = append(options, DropdownOption{
			Label: label,
			Value: member.UserID.String(),
		})
	}

	// Pre-select the current assignee if one exists.
	if !currentAssignee.IsZero() {
		for index, option := range options {
			if option.Value == currentAssignee.String() {
				cursor = index
				break
			}
		}
	}

	return options, cursor
}
