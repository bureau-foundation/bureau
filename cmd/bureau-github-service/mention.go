// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/bureau-foundation/bureau/lib/forgesub"
	"github.com/bureau-foundation/bureau/lib/schema/forge"
	"github.com/bureau-foundation/bureau/messaging"
)

// MentionDispatcher detects @bot-username mentions in forge comment
// events and posts them to the corresponding Bureau rooms as Matrix
// messages. Authorization is based on the author_association field
// from the webhook payload — no additional API calls required.
type MentionDispatcher struct {
	session messaging.Session
	logger  *slog.Logger
}

// Dispatch checks whether the event is a comment that mentions a
// configured bot username, and if so posts it to each matching room.
// Rooms without MentionDispatch config are silently skipped.
func (dispatcher *MentionDispatcher) Dispatch(ctx context.Context, event *forge.Event, rooms []forgesub.RoomMatch) {
	if event.Type != forge.EventCategoryComment || event.Comment == nil {
		return
	}
	comment := event.Comment

	// Extract all @mentions from the comment body. The returned
	// usernames are lowercased and deduplicated.
	mentions := forgesub.ExtractMentions(comment.Body)
	if len(mentions) == 0 {
		return
	}

	for _, room := range rooms {
		dispatcher.dispatchToRoom(ctx, comment, mentions, room)
	}
}

// dispatchToRoom evaluates mention dispatch for a single room.
func (dispatcher *MentionDispatcher) dispatchToRoom(ctx context.Context, comment *forge.CommentEvent, mentions []string, room forgesub.RoomMatch) {
	config := room.Config
	if config == nil || config.MentionDispatch == nil {
		return
	}
	dispatchConfig := config.MentionDispatch

	// Check that the comment mentions the configured bot username.
	// ExtractMentions returns lowercased usernames, so compare
	// case-insensitively.
	botLower := strings.ToLower(dispatchConfig.BotUsername)
	mentioned := false
	for _, mention := range mentions {
		if mention == botLower {
			mentioned = true
			break
		}
	}
	if !mentioned {
		return
	}

	// Self-mention suppression: if the comment author IS the bot,
	// skip dispatch to avoid feedback loops when Bureau posts
	// comments back to GitHub.
	if strings.EqualFold(comment.Author, dispatchConfig.BotUsername) {
		dispatcher.logger.Debug("mention dispatch: skipping self-mention",
			"room_id", room.RoomID,
			"author", comment.Author,
		)
		return
	}

	// Authorization: check author_association against the configured
	// minimum level.
	authorAssociation := forge.AuthorAssociation(comment.AuthorAssociation)
	minimumAssociation := dispatchConfig.EffectiveMinAssociation()
	if !authorAssociation.MeetsMinimum(minimumAssociation) {
		dispatcher.logger.Info("mention dispatch: author below minimum association",
			"room_id", room.RoomID,
			"author", comment.Author,
			"author_association", comment.AuthorAssociation,
			"min_association", minimumAssociation,
		)
		return
	}

	// Build and send the message.
	message := formatMentionMessage(comment, dispatchConfig.BotUsername)
	content := messaging.NewTextMessage(message)

	eventID, err := dispatcher.session.SendMessage(ctx, room.RoomID, content)
	if err != nil {
		dispatcher.logger.Warn("mention dispatch: failed to send message",
			"room_id", room.RoomID,
			"author", comment.Author,
			"error", err,
		)
		return
	}

	dispatcher.logger.Info("mention dispatch: posted to room",
		"room_id", room.RoomID,
		"event_id", eventID,
		"author", comment.Author,
		"repo", comment.Repo,
		"entity_number", comment.EntityNumber,
	)
}

// formatMentionMessage builds the plain-text Matrix message for a
// mention dispatch. Includes attribution, the comment body, and a
// link back to the GitHub comment.
func formatMentionMessage(comment *forge.CommentEvent, botUsername string) string {
	titleSuffix := ""
	if comment.EntityTitle != "" {
		titleSuffix = fmt.Sprintf(" (%s)", comment.EntityTitle)
	}

	// Include a "PR" label for pull request comments since GitHub's
	// issue_comment webhook fires for both issues and PRs.
	entityPrefix := ""
	if comment.EntityType == forge.EntityTypePullRequest {
		entityPrefix = " PR"
	}

	header := fmt.Sprintf("[GitHub] %s mentioned @%s on %s%s#%d%s",
		comment.Author,
		botUsername,
		comment.Repo,
		entityPrefix,
		comment.EntityNumber,
		titleSuffix,
	)

	var builder strings.Builder
	builder.WriteString(header)
	builder.WriteByte('\n')

	if comment.Body != "" {
		builder.WriteByte('\n')
		builder.WriteString(comment.Body)
		builder.WriteByte('\n')
	}

	if comment.URL != "" {
		builder.WriteByte('\n')
		builder.WriteString(comment.URL)
	}

	return builder.String()
}

// mentionDispatchEnabled reports whether any room in the list has
// mention dispatch configured. Used as a fast-path check to avoid
// extracting mentions when no rooms care.
func mentionDispatchEnabled(rooms []forgesub.RoomMatch) bool {
	for _, room := range rooms {
		if room.Config != nil && room.Config.MentionDispatch != nil {
			return true
		}
	}
	return false
}
