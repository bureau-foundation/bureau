// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/forgesub"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/forge"
	"github.com/bureau-foundation/bureau/messaging"
)

// --- Test helpers ---

// recordingSession captures messages sent via SendMessage for test
// assertions. Implements the subset of messaging.Session used by the
// mention dispatcher.
type recordingSession struct {
	messaging.Session
	messages []sentMessage
}

type sentMessage struct {
	roomID  ref.RoomID
	content messaging.MessageContent
}

func (session *recordingSession) SendMessage(_ context.Context, roomID ref.RoomID, content messaging.MessageContent) (ref.EventID, error) {
	session.messages = append(session.messages, sentMessage{
		roomID:  roomID,
		content: content,
	})
	return ref.EventID{}, nil
}

func testMentionDispatcher(session *recordingSession) *MentionDispatcher {
	return &MentionDispatcher{
		session: session,
		logger:  slog.Default(),
	}
}

func testRoomID() ref.RoomID {
	roomID, _ := ref.ParseRoomID("!test:bureau.local")
	return roomID
}

func mentionRoom(botUsername string, minAssociation forge.AuthorAssociation) forgesub.RoomMatch {
	return forgesub.RoomMatch{
		RoomID: testRoomID(),
		Config: &forge.ForgeConfig{
			Version:  1,
			Provider: "github",
			Repo:     "acme/widgets",
			MentionDispatch: &forge.MentionDispatchConfig{
				BotUsername:    botUsername,
				MinAssociation: minAssociation,
			},
		},
	}
}

func commentEvent(author, body, association string) *forge.Event {
	return &forge.Event{
		Type: forge.EventCategoryComment,
		Comment: &forge.CommentEvent{
			Provider:          "github",
			Repo:              "acme/widgets",
			EntityType:        forge.EntityTypeIssue,
			EntityNumber:      42,
			EntityTitle:       "Fix flaky test in CI",
			Author:            author,
			Body:              body,
			Summary:           "[acme/widgets] " + author + " commented on #42",
			URL:               "https://github.com/acme/widgets/issues/42#issuecomment-123",
			AuthorAssociation: association,
		},
	}
}

// --- Tests ---

func TestMentionDispatchBasic(t *testing.T) {
	t.Parallel()

	session := &recordingSession{}
	dispatcher := testMentionDispatcher(session)

	event := commentEvent("alice", "Hey @bureau-bot please fix this", "COLLABORATOR")
	rooms := []forgesub.RoomMatch{mentionRoom("bureau-bot", "")}

	dispatcher.Dispatch(context.Background(), event, rooms)

	if len(session.messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(session.messages))
	}

	message := session.messages[0]
	if message.roomID != testRoomID() {
		t.Errorf("room ID = %v, want %v", message.roomID, testRoomID())
	}
	body := message.content.Body
	if !strings.Contains(body, "alice") {
		t.Errorf("message should contain author name, got: %s", body)
	}
	if !strings.Contains(body, "@bureau-bot") {
		t.Errorf("message should contain bot username, got: %s", body)
	}
	if !strings.Contains(body, "acme/widgets#42") {
		t.Errorf("message should contain repo#number, got: %s", body)
	}
	if !strings.Contains(body, "Fix flaky test in CI") {
		t.Errorf("message should contain entity title, got: %s", body)
	}
	if !strings.Contains(body, "please fix this") {
		t.Errorf("message should contain comment body, got: %s", body)
	}
	if !strings.Contains(body, "https://github.com/acme/widgets/issues/42#issuecomment-123") {
		t.Errorf("message should contain URL, got: %s", body)
	}
}

func TestMentionDispatchNoMention(t *testing.T) {
	t.Parallel()

	session := &recordingSession{}
	dispatcher := testMentionDispatcher(session)

	// Comment does not mention the bot.
	event := commentEvent("alice", "This test is flaky, someone should fix it", "COLLABORATOR")
	rooms := []forgesub.RoomMatch{mentionRoom("bureau-bot", "")}

	dispatcher.Dispatch(context.Background(), event, rooms)

	if len(session.messages) != 0 {
		t.Fatalf("expected 0 messages (no mention), got %d", len(session.messages))
	}
}

func TestMentionDispatchWrongBot(t *testing.T) {
	t.Parallel()

	session := &recordingSession{}
	dispatcher := testMentionDispatcher(session)

	// Comment mentions a different bot.
	event := commentEvent("alice", "Hey @other-bot please fix this", "COLLABORATOR")
	rooms := []forgesub.RoomMatch{mentionRoom("bureau-bot", "")}

	dispatcher.Dispatch(context.Background(), event, rooms)

	if len(session.messages) != 0 {
		t.Fatalf("expected 0 messages (wrong bot), got %d", len(session.messages))
	}
}

func TestMentionDispatchCaseInsensitive(t *testing.T) {
	t.Parallel()

	session := &recordingSession{}
	dispatcher := testMentionDispatcher(session)

	// Mention uses different case than the configured bot username.
	event := commentEvent("alice", "Hey @Bureau-Bot please fix this", "COLLABORATOR")
	rooms := []forgesub.RoomMatch{mentionRoom("bureau-bot", "")}

	dispatcher.Dispatch(context.Background(), event, rooms)

	if len(session.messages) != 1 {
		t.Fatalf("expected 1 message (case-insensitive match), got %d", len(session.messages))
	}
}

func TestMentionDispatchAssociationGating(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		association string
		minimum     forge.AuthorAssociation
		wantSent    bool
	}{
		{"owner_passes_collaborator", "OWNER", forge.AssociationCollaborator, true},
		{"member_passes_collaborator", "MEMBER", forge.AssociationCollaborator, true},
		{"collaborator_passes_collaborator", "COLLABORATOR", forge.AssociationCollaborator, true},
		{"contributor_fails_collaborator", "CONTRIBUTOR", forge.AssociationCollaborator, false},
		{"none_fails_collaborator", "NONE", forge.AssociationCollaborator, false},
		{"default_minimum_is_collaborator", "COLLABORATOR", "", true},
		{"default_minimum_rejects_contributor", "CONTRIBUTOR", "", false},
		{"member_minimum_accepts_owner", "OWNER", forge.AssociationMember, true},
		{"member_minimum_rejects_collaborator", "COLLABORATOR", forge.AssociationMember, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			session := &recordingSession{}
			dispatcher := testMentionDispatcher(session)

			event := commentEvent("alice", "Hey @bureau-bot fix this", tt.association)
			rooms := []forgesub.RoomMatch{mentionRoom("bureau-bot", tt.minimum)}

			dispatcher.Dispatch(context.Background(), event, rooms)

			if tt.wantSent && len(session.messages) != 1 {
				t.Errorf("expected message to be sent, got %d messages", len(session.messages))
			}
			if !tt.wantSent && len(session.messages) != 0 {
				t.Errorf("expected message to be suppressed, got %d messages", len(session.messages))
			}
		})
	}
}

func TestMentionDispatchSelfMentionSuppression(t *testing.T) {
	t.Parallel()

	session := &recordingSession{}
	dispatcher := testMentionDispatcher(session)

	// The bot mentions itself (e.g., when posting a comment back to
	// GitHub that includes its own username).
	event := commentEvent("bureau-bot", "I've investigated @bureau-bot and found the issue", "COLLABORATOR")
	rooms := []forgesub.RoomMatch{mentionRoom("bureau-bot", "")}

	dispatcher.Dispatch(context.Background(), event, rooms)

	if len(session.messages) != 0 {
		t.Fatalf("expected 0 messages (self-mention), got %d", len(session.messages))
	}
}

func TestMentionDispatchNoConfig(t *testing.T) {
	t.Parallel()

	session := &recordingSession{}
	dispatcher := testMentionDispatcher(session)

	event := commentEvent("alice", "Hey @bureau-bot fix this", "COLLABORATOR")
	rooms := []forgesub.RoomMatch{
		{
			RoomID: testRoomID(),
			Config: &forge.ForgeConfig{
				Version:  1,
				Provider: "github",
				Repo:     "acme/widgets",
				// No MentionDispatch configured.
			},
		},
	}

	dispatcher.Dispatch(context.Background(), event, rooms)

	if len(session.messages) != 0 {
		t.Fatalf("expected 0 messages (no config), got %d", len(session.messages))
	}
}

func TestMentionDispatchNilConfig(t *testing.T) {
	t.Parallel()

	session := &recordingSession{}
	dispatcher := testMentionDispatcher(session)

	event := commentEvent("alice", "Hey @bureau-bot fix this", "COLLABORATOR")
	rooms := []forgesub.RoomMatch{
		{
			RoomID: testRoomID(),
			Config: nil,
		},
	}

	dispatcher.Dispatch(context.Background(), event, rooms)

	if len(session.messages) != 0 {
		t.Fatalf("expected 0 messages (nil config), got %d", len(session.messages))
	}
}

func TestMentionDispatchNonCommentEvent(t *testing.T) {
	t.Parallel()

	session := &recordingSession{}
	dispatcher := testMentionDispatcher(session)

	// A push event, not a comment.
	event := &forge.Event{
		Type: forge.EventCategoryPush,
		Push: &forge.PushEvent{
			Provider: "github",
			Repo:     "acme/widgets",
		},
	}
	rooms := []forgesub.RoomMatch{mentionRoom("bureau-bot", "")}

	dispatcher.Dispatch(context.Background(), event, rooms)

	if len(session.messages) != 0 {
		t.Fatalf("expected 0 messages (not a comment), got %d", len(session.messages))
	}
}

func TestMentionDispatchMultipleRooms(t *testing.T) {
	t.Parallel()

	session := &recordingSession{}
	dispatcher := testMentionDispatcher(session)

	event := commentEvent("alice", "Hey @bureau-bot fix this", "COLLABORATOR")

	secondRoomID, _ := ref.ParseRoomID("!second:bureau.local")
	rooms := []forgesub.RoomMatch{
		mentionRoom("bureau-bot", ""),
		{
			RoomID: secondRoomID,
			Config: &forge.ForgeConfig{
				Version:  1,
				Provider: "github",
				Repo:     "acme/widgets",
				MentionDispatch: &forge.MentionDispatchConfig{
					BotUsername: "bureau-bot",
				},
			},
		},
	}

	dispatcher.Dispatch(context.Background(), event, rooms)

	if len(session.messages) != 2 {
		t.Fatalf("expected 2 messages (one per room), got %d", len(session.messages))
	}
}

func TestMentionDispatchPRComment(t *testing.T) {
	t.Parallel()

	session := &recordingSession{}
	dispatcher := testMentionDispatcher(session)

	event := &forge.Event{
		Type: forge.EventCategoryComment,
		Comment: &forge.CommentEvent{
			Provider:          "github",
			Repo:              "acme/widgets",
			EntityType:        forge.EntityTypePullRequest,
			EntityNumber:      99,
			EntityTitle:       "Add widget caching",
			Author:            "alice",
			Body:              "@bureau-bot please review",
			Summary:           "[acme/widgets] alice commented on #99",
			URL:               "https://github.com/acme/widgets/pull/99#issuecomment-456",
			AuthorAssociation: "MEMBER",
		},
	}
	rooms := []forgesub.RoomMatch{mentionRoom("bureau-bot", "")}

	dispatcher.Dispatch(context.Background(), event, rooms)

	if len(session.messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(session.messages))
	}

	body := session.messages[0].content.Body
	if !strings.Contains(body, "PR") {
		t.Errorf("message for PR comment should contain 'PR', got: %s", body)
	}
}

func TestMentionDispatchEnabledCheck(t *testing.T) {
	t.Parallel()

	t.Run("enabled", func(t *testing.T) {
		rooms := []forgesub.RoomMatch{mentionRoom("bureau-bot", "")}
		if !mentionDispatchEnabled(rooms) {
			t.Error("expected mentionDispatchEnabled = true")
		}
	})

	t.Run("disabled_no_config", func(t *testing.T) {
		rooms := []forgesub.RoomMatch{
			{
				RoomID: testRoomID(),
				Config: &forge.ForgeConfig{
					Version:  1,
					Provider: "github",
					Repo:     "acme/widgets",
				},
			},
		}
		if mentionDispatchEnabled(rooms) {
			t.Error("expected mentionDispatchEnabled = false")
		}
	})

	t.Run("disabled_nil_config", func(t *testing.T) {
		rooms := []forgesub.RoomMatch{
			{RoomID: testRoomID(), Config: nil},
		}
		if mentionDispatchEnabled(rooms) {
			t.Error("expected mentionDispatchEnabled = false")
		}
	})

	t.Run("mixed", func(t *testing.T) {
		rooms := []forgesub.RoomMatch{
			{RoomID: testRoomID(), Config: nil},
			mentionRoom("bureau-bot", ""),
		}
		if !mentionDispatchEnabled(rooms) {
			t.Error("expected mentionDispatchEnabled = true (one room has config)")
		}
	})
}

// --- Message formatting ---

func TestFormatMentionMessage(t *testing.T) {
	t.Parallel()

	comment := &forge.CommentEvent{
		Provider:          "github",
		Repo:              "acme/widgets",
		EntityType:        forge.EntityTypeIssue,
		EntityNumber:      42,
		EntityTitle:       "Fix flaky test",
		Author:            "alice",
		Body:              "Can you look into this?",
		URL:               "https://github.com/acme/widgets/issues/42#issuecomment-123",
		AuthorAssociation: "COLLABORATOR",
	}

	message := formatMentionMessage(comment, "bureau-bot")

	expected := []string{
		"[GitHub]",
		"alice",
		"@bureau-bot",
		"acme/widgets#42",
		"Fix flaky test",
		"Can you look into this?",
		"https://github.com/acme/widgets/issues/42#issuecomment-123",
	}

	for _, fragment := range expected {
		if !strings.Contains(message, fragment) {
			t.Errorf("message missing %q:\n%s", fragment, message)
		}
	}
}

func TestFormatMentionMessagePR(t *testing.T) {
	t.Parallel()

	comment := &forge.CommentEvent{
		Provider:     "github",
		Repo:         "acme/widgets",
		EntityType:   forge.EntityTypePullRequest,
		EntityNumber: 99,
		EntityTitle:  "Add caching",
		Author:       "bob",
		Body:         "Please review",
		URL:          "https://github.com/acme/widgets/pull/99#issuecomment-456",
	}

	message := formatMentionMessage(comment, "bureau-bot")

	if !strings.Contains(message, "PR") {
		t.Errorf("PR comment message should contain 'PR':\n%s", message)
	}
}

func TestFormatMentionMessageNoTitle(t *testing.T) {
	t.Parallel()

	comment := &forge.CommentEvent{
		Provider:     "github",
		Repo:         "acme/widgets",
		EntityType:   forge.EntityTypeIssue,
		EntityNumber: 42,
		Author:       "alice",
		Body:         "Fix it please",
		URL:          "https://github.com/acme/widgets/issues/42#issuecomment-123",
	}

	message := formatMentionMessage(comment, "bureau-bot")

	// Should not have empty parentheses when title is missing.
	if strings.Contains(message, "()") {
		t.Errorf("message should not contain empty parens:\n%s", message)
	}
}
