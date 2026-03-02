// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/lib/forgesub"
	"github.com/bureau-foundation/bureau/lib/schema/forge"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/service"
)

// TicketSyncer translates forge issue events into ticket service
// operations. When a GitHub issue is opened, edited, closed, or
// commented on, and the target room has issue_sync enabled, the
// syncer calls the ticket service's import or add-note action.
//
// The syncer is optional: if the GitHub service runs without a ticket
// service socket configured, ticket sync is silently disabled.
type TicketSyncer struct {
	client *service.ServiceClient
	logger *slog.Logger
}

// NewTicketSyncer creates a syncer that calls the ticket service at
// the given socket path, authenticated with the given token. Returns
// an error if the token cannot be read.
func NewTicketSyncer(socketPath, tokenPath string, logger *slog.Logger) (*TicketSyncer, error) {
	client, err := service.NewServiceClient(socketPath, tokenPath)
	if err != nil {
		return nil, fmt.Errorf("creating ticket service client: %w", err)
	}
	return &TicketSyncer{
		client: client,
		logger: logger,
	}, nil
}

// SyncEvent processes a forge event against all rooms that match the
// event's repo. For each room with issue_sync enabled, it translates
// the event into a ticket service operation.
//
// Only issue events and issue comments are synced. Other event types
// (push, PR, CI) are ignored — PR→ticket sync is a separate feature.
func (ts *TicketSyncer) SyncEvent(ctx context.Context, event *forge.Event, rooms []forgesub.RoomMatch) {
	switch event.Type {
	case forge.EventCategoryIssues:
		if event.Issue == nil {
			return
		}
		ts.syncIssueEvent(ctx, event.Issue, rooms)

	case forge.EventCategoryComment:
		if event.Comment == nil {
			return
		}
		// Only sync comments on issues, not PRs.
		if event.Comment.EntityType != "issue" {
			return
		}
		ts.syncCommentEvent(ctx, event.Comment, rooms)
	}
}

// syncIssueEvent handles issue lifecycle events: opened, edited,
// labeled, reopened, closed. Uses the ticket service's import action
// which is an idempotent upsert — repeated imports for the same issue
// update the existing ticket rather than creating duplicates.
func (ts *TicketSyncer) syncIssueEvent(ctx context.Context, issue *forge.IssueEvent, rooms []forgesub.RoomMatch) {
	for _, room := range rooms {
		if !issueSyncEnabled(room.Config) {
			continue
		}

		ticketID := issueTicketID(issue.Provider, issue.Repo, issue.Number)
		content := issueToTicketContent(issue)

		err := ts.client.Call(ctx, "import", importRequest{
			Room: room.RoomID.String(),
			Tickets: []importEntry{
				{ID: ticketID, Content: content},
			},
		}, nil)

		if err != nil {
			ts.logger.Error("ticket sync: import failed",
				"room_id", room.RoomID,
				"ticket_id", ticketID,
				"action", issue.Action,
				"error", err,
			)
			continue
		}

		ts.logger.Info("ticket sync: issue imported",
			"room_id", room.RoomID,
			"ticket_id", ticketID,
			"action", issue.Action,
			"title", issue.Title,
		)
	}
}

// syncCommentEvent handles new comments on issues. Uses the ticket
// service's add-note action to append the comment as a ticket note.
func (ts *TicketSyncer) syncCommentEvent(ctx context.Context, comment *forge.CommentEvent, rooms []forgesub.RoomMatch) {
	for _, room := range rooms {
		if !issueSyncEnabled(room.Config) {
			continue
		}

		ticketID := issueTicketID(comment.Provider, comment.Repo, comment.EntityNumber)

		// Build the note body with attribution. The ticket service
		// assigns the note author from the service token (the github
		// service's identity), so we include the GitHub username in
		// the body for traceability.
		noteBody := fmt.Sprintf("**%s** commented on [GitHub](%s):\n\n%s",
			comment.Author, comment.URL, comment.Body)

		err := ts.client.Call(ctx, "add-note", addNoteRequest{
			Room:   room.RoomID.String(),
			Ticket: ticketID,
			Body:   noteBody,
		}, nil)

		if err != nil {
			// The ticket might not exist yet (comment arrived before
			// the issue opened event). Log as warning, not error.
			ts.logger.Warn("ticket sync: add-note failed",
				"room_id", room.RoomID,
				"ticket_id", ticketID,
				"comment_author", comment.Author,
				"error", err,
			)
			continue
		}

		ts.logger.Info("ticket sync: comment added as note",
			"room_id", room.RoomID,
			"ticket_id", ticketID,
			"comment_author", comment.Author,
		)
	}
}

// --- Request types ---
//
// These mirror the ticket service's CBOR request types. Defined here
// rather than importing from the ticket service binary to avoid a
// build dependency between services. The wire format is the contract.

type importRequest struct {
	Room    string        `cbor:"room"`
	Tickets []importEntry `cbor:"tickets"`
}

type importEntry struct {
	ID      string               `cbor:"id"`
	Content ticket.TicketContent `cbor:"content"`
}

type addNoteRequest struct {
	Room   string `cbor:"room"`
	Ticket string `cbor:"ticket"`
	Body   string `cbor:"body"`
}

// --- Translation helpers ---

// issueSyncEnabled reports whether a room's forge config has issue
// sync enabled (import or bidirectional mode).
func issueSyncEnabled(config *forge.ForgeConfig) bool {
	if config == nil {
		return false
	}
	return config.IssueSync == forge.IssueSyncImport ||
		config.IssueSync == forge.IssueSyncBidirectional
}

// issueTicketID generates a deterministic ticket ID from a forge
// issue reference. The ID is stable across updates so that repeated
// imports for the same issue upsert the same ticket.
//
// Format: "gh-<owner>-<repo>-<number>" with slashes and dots replaced
// by hyphens. Example: "gh-bureau-foundation-bureau-42".
func issueTicketID(provider, repo string, number int) string {
	// Normalize the repo path into a valid ticket ID component.
	normalized := strings.NewReplacer("/", "-", ".", "-").Replace(repo)
	return fmt.Sprintf("gh-%s-%d", normalized, number)
}

// issueToTicketContent translates a forge IssueEvent into a
// TicketContent for the ticket service's import action.
func issueToTicketContent(issue *forge.IssueEvent) ticket.TicketContent {
	status := ticket.StatusOpen
	var closedAt string
	var closeReason string

	if issue.Action == string(forge.IssueClosed) {
		status = ticket.StatusClosed
		closedAt = time.Now().UTC().Format(time.RFC3339)
		closeReason = "Closed on GitHub"
	}

	// Prepend the GitHub link to the body so the ticket always
	// has a reference back to the source issue.
	bodyContent := issue.Body
	if bodyContent == "" {
		bodyContent = issue.Summary
	}
	body := issue.URL + "\n\n" + bodyContent

	return ticket.TicketContent{
		Version:     ticket.TicketContentVersion,
		Title:       issue.Title,
		Body:        body,
		Status:      status,
		Priority:    derivePriority(issue.Labels),
		Type:        deriveType(issue.Labels),
		Labels:      issue.Labels,
		ClosedAt:    closedAt,
		CloseReason: closeReason,
		Origin: &ticket.TicketOrigin{
			Source:      issue.Provider,
			ExternalRef: fmt.Sprintf("%s#%d", issue.Repo, issue.Number),
		},
	}
}

// derivePriority maps GitHub labels to Bureau ticket priorities.
// Returns the highest (lowest number) priority found in the labels,
// or 2 (medium) as the default when no priority label is present.
func derivePriority(labels []string) int {
	found := false
	best := 4 // start at lowest, track upward
	for _, label := range labels {
		lower := strings.ToLower(label)
		priority := -1
		switch {
		case lower == "p0" || lower == "critical":
			priority = 0
		case lower == "p1" || lower == "high" || lower == "high priority":
			priority = 1
		case lower == "p2" || lower == "medium":
			priority = 2
		case lower == "p3" || lower == "low" || lower == "low priority":
			priority = 3
		case lower == "p4" || lower == "backlog":
			priority = 4
		}
		if priority >= 0 {
			found = true
			if priority < best {
				best = priority
			}
		}
	}
	if !found {
		return 2 // default: medium
	}
	return best
}

// deriveType maps GitHub labels to Bureau ticket types. Returns the
// first matching type found, or TypeTask as the default.
func deriveType(labels []string) ticket.TicketType {
	for _, label := range labels {
		lower := strings.ToLower(label)
		switch lower {
		case "bug":
			return ticket.TypeBug
		case "feature", "enhancement":
			return ticket.TypeFeature
		case "documentation", "docs":
			return ticket.TypeDocs
		case "question":
			return ticket.TypeQuestion
		case "chore":
			return ticket.TypeChore
		}
	}
	return ticket.TypeTask
}
