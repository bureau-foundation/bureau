// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

// Authorization action constants for the ticket service. Enforced by
// the ticket service (cmd/bureau-ticket-service/) via service token
// grant verification. See lib/schema/action.go for the action system
// overview and non-ticket-specific actions.

// Mutation operations.
const (
	ActionCreate = "ticket/create"
	ActionUpdate = "ticket/update"
	ActionClose  = "ticket/close"
	ActionReopen = "ticket/reopen"
	ActionImport = "ticket/import"
	ActionReview = "ticket/review"
	ActionAttach = "ticket/attach"
)

// Query operations.
const (
	ActionList          = "ticket/list"
	ActionShow          = "ticket/show"
	ActionReady         = "ticket/ready"
	ActionBlocked       = "ticket/blocked"
	ActionRanked        = "ticket/ranked"
	ActionStats         = "ticket/stats"
	ActionGrep          = "ticket/grep"
	ActionSearch        = "ticket/search"
	ActionDeps          = "ticket/deps"
	ActionChildren      = "ticket/children"
	ActionEpicHealth    = "ticket/epic-health"
	ActionInfo          = "ticket/info"
	ActionUpcomingGates = "ticket/upcoming-gates"
	ActionListRooms     = "ticket/list-rooms"
	ActionListMembers   = "ticket/list-members"
)

// Subscription and stewardship operations.
const (
	ActionSubscribe          = "ticket/subscribe"
	ActionStewardshipList    = "ticket/stewardship-list"
	ActionStewardshipResolve = "ticket/stewardship-resolve"
	ActionStewardshipSet     = "ticket/stewardship-set"
)

// ActionAll is the wildcard pattern matching all ticket operations.
const ActionAll = "ticket/**"
