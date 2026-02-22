// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/ticketindex"
)

// beadsEntry is the JSON structure of a single line in a beads JSONL
// file. Field names match the beads_rust serialization format.
type beadsEntry struct {
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
	Dependencies []beadsDependency `json:"dependencies"`
}

// beadsDependency represents a dependency relationship in the beads
// format. The issue_id field identifies the ticket that has the
// dependency; depends_on_id identifies the target.
type beadsDependency struct {
	IssueID     string `json:"issue_id"`
	DependsOnID string `json:"depends_on_id"`
	Type        string `json:"type"` // "blocks" or "parent-child"
}

// LoadBeadsFile reads a beads JSONL file and returns an IndexSource
// populated with the converted tickets. Each line in the file is an
// independent JSON object representing one ticket.
//
// Field mapping from beads to TicketContent:
//   - id: kept as-is (e.g., "bd-3vk5") as the ticket ID key
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
func LoadBeadsFile(path string) (*IndexSource, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open beads file: %w", err)
	}
	defer file.Close()

	index := ticketindex.NewIndex()
	scanner := bufio.NewScanner(file)

	// Beads entries can be large (long descriptions, many deps).
	// Default 64KB scanner buffer may be insufficient.
	const maxLineSize = 1024 * 1024
	scanner.Buffer(make([]byte, 0, maxLineSize), maxLineSize)

	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var entry beadsEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNumber, err)
		}

		if entry.ID == "" {
			return nil, fmt.Errorf("line %d: missing id field", lineNumber)
		}

		content := beadsToTicketContent(entry)
		index.Put(entry.ID, content)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read beads file: %w", err)
	}

	return NewIndexSource(index), nil
}

// beadsToTicketContent converts a beads JSONL entry to a TicketContent.
func beadsToTicketContent(entry beadsEntry) ticket.TicketContent {
	content := ticket.TicketContent{
		Version:     1,
		Title:       entry.Title,
		Body:        entry.Description,
		Status:      entry.Status,
		Priority:    entry.Priority,
		Type:        entry.IssueType,
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
