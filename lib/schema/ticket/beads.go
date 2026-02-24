// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/bureau-foundation/bureau/lib/ref"
)

// BeadsEntry is the JSON structure of a single line in a beads JSONL
// file. Field names match the beads_rust serialization format.
type BeadsEntry struct {
	ID           string            `json:"id"`
	Title        string            `json:"title"`
	Description  string            `json:"description"`
	Status       string            `json:"status"`
	Priority     int               `json:"priority"`
	IssueType    string            `json:"issue_type"`
	Labels       []string          `json:"labels"`
	CreatedAt    string            `json:"created_at"`
	CreatedBy    string            `json:"created_by"`
	UpdatedAt    string            `json:"updated_at"`
	ClosedAt     string            `json:"closed_at"`
	CloseReason  string            `json:"close_reason"`
	Dependencies []BeadsDependency `json:"dependencies"`
}

// BeadsDependency represents a dependency relationship in the beads
// format. The IssueID field identifies the ticket that has the
// dependency; DependsOnID identifies the target.
type BeadsDependency struct {
	IssueID     string `json:"issue_id"`
	DependsOnID string `json:"depends_on_id"`
	Type        string `json:"type"` // "blocks" or "parent-child"
}

// BeadsToTicketContent converts a beads JSONL entry to a TicketContent.
//
// Field mapping:
//   - id: caller handles (not part of TicketContent)
//   - title -> Title
//   - description -> Body
//   - status -> Status (open, in_progress, closed)
//   - priority -> Priority (0-4, same scale)
//   - issue_type -> Type (task, bug, feature, epic, chore, docs, question)
//   - labels -> Labels
//   - created_at -> CreatedAt, created_by -> CreatedBy
//   - updated_at -> UpdatedAt
//   - closed_at -> ClosedAt, close_reason -> CloseReason
//   - dependencies[type="blocks"] -> BlockedBy (depends_on_id values)
//   - dependencies[type="parent-child"] -> Parent (depends_on_id value)
func BeadsToTicketContent(entry BeadsEntry) TicketContent {
	content := TicketContent{
		Version:     1,
		Title:       entry.Title,
		Body:        entry.Description,
		Status:      TicketStatus(entry.Status),
		Priority:    entry.Priority,
		Type:        TicketType(entry.IssueType),
		Labels:      entry.Labels,
		CreatedBy:   beadsParseUserID(entry.CreatedBy),
		CreatedAt:   entry.CreatedAt,
		UpdatedAt:   entry.UpdatedAt,
		ClosedAt:    entry.ClosedAt,
		CloseReason: entry.CloseReason,
	}

	for _, dependency := range entry.Dependencies {
		// Only process dependencies belonging to this entry.
		// The beads format always sets issue_id to the owning entry,
		// but we filter defensively.
		if dependency.IssueID != entry.ID {
			continue
		}
		switch dependency.Type {
		case "blocks":
			// depends_on_id blocks this issue — this issue is
			// blocked by depends_on_id.
			content.BlockedBy = append(content.BlockedBy, dependency.DependsOnID)
		case "parent-child":
			// depends_on_id is the parent of this issue.
			content.Parent = dependency.DependsOnID
		}
	}

	return content
}

// beadsParseUserID parses a beads created_by string as a Matrix user
// ID. Beads entries come from a local issue tracker where the
// created_by field may not be a valid Matrix user ID format. Returns
// the zero value for non-Matrix identifiers.
func beadsParseUserID(raw string) ref.UserID {
	userID, _ := ref.ParseUserID(raw)
	return userID
}

// RenameBeadsIDs renames ticket IDs and all internal references from
// the source beads prefix to the target prefix. This includes:
//   - The ticket ID itself (e.g., "bd-10g2" -> "tkt-10g2")
//   - BlockedBy entries
//   - Parent field
//   - References in the Body text matching the pattern prefix-[0-9a-z]+
//
// Returns the renamed ID and modified content. The original entry is
// not modified.
func RenameBeadsIDs(id string, content TicketContent, sourcePrefix, targetPrefix string) (string, TicketContent) {
	rename := func(ticketID string) string {
		return renameBeadsID(ticketID, sourcePrefix, targetPrefix)
	}

	newID := rename(id)

	// Rename blocked_by references.
	if len(content.BlockedBy) > 0 {
		renamed := make([]string, len(content.BlockedBy))
		for i, blockerID := range content.BlockedBy {
			renamed[i] = rename(blockerID)
		}
		content.BlockedBy = renamed
	}

	// Rename parent reference.
	if content.Parent != "" {
		content.Parent = rename(content.Parent)
	}

	// Rename references in the body text.
	if content.Body != "" {
		content.Body = renameBeadsRefsInText(content.Body, sourcePrefix, targetPrefix)
	}

	// Rename references in the title (less common but possible).
	if content.Title != "" {
		content.Title = renameBeadsRefsInText(content.Title, sourcePrefix, targetPrefix)
	}

	// Rename references in notes.
	if len(content.Notes) > 0 {
		renamed := make([]TicketNote, len(content.Notes))
		copy(renamed, content.Notes)
		for i := range renamed {
			renamed[i].Body = renameBeadsRefsInText(renamed[i].Body, sourcePrefix, targetPrefix)
		}
		content.Notes = renamed
	}

	return newID, content
}

// renameBeadsID replaces the beads prefix in a ticket ID with the
// target prefix. IDs that don't start with the source prefix are
// returned unchanged.
func renameBeadsID(id, sourcePrefix, targetPrefix string) string {
	if strings.HasPrefix(id, sourcePrefix+"-") {
		return targetPrefix + id[len(sourcePrefix):]
	}
	return id
}

// renameBeadsRefsInText replaces all occurrences of beads ticket
// references in free text. Matches word-boundary-delimited patterns
// like "bd-10g2" and replaces the prefix portion.
func renameBeadsRefsInText(text, sourcePrefix, targetPrefix string) string {
	pattern := fmt.Sprintf(`\b%s-([0-9a-z]+)\b`, regexp.QuoteMeta(sourcePrefix))
	re := regexp.MustCompile(pattern)
	return re.ReplaceAllString(text, targetPrefix+"-$1")
}
