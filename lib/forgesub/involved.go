// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package forgesub

// InvolvementRole identifies how a user is involved with a forge
// event. Used by auto-subscribe to match against per-agent rules.
type InvolvementRole string

const (
	// RoleAuthor means the user created the entity (opened a PR,
	// filed an issue, pushed commits).
	RoleAuthor InvolvementRole = "author"

	// RoleAssignee means the user was assigned to the entity.
	RoleAssignee InvolvementRole = "assignee"

	// RoleReviewRequested means the user was requested as a reviewer
	// on a pull request.
	RoleReviewRequested InvolvementRole = "review_requested"

	// RoleMentioned means the user was @mentioned in the text of an
	// issue body, PR description, or comment.
	RoleMentioned InvolvementRole = "mentioned"
)

// InvolvedUser pairs a forge username with their role in an event.
// The forge connector extracts these from provider-specific webhook
// payloads and passes them to the Manager for auto-subscribe
// evaluation.
type InvolvedUser struct {
	ForgeUsername string
	Role          InvolvementRole
}
