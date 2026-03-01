// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package forge

// Authorization action suffixes for forge connector services. Each
// connector prefixes these with the provider name to form the full
// grant string (e.g., "github/subscribe", "forgejo/create-issue").
// See authorization.md for the grant model.

// Subscription operations.
const (
	ActionSubscribe           = "subscribe"
	ActionUnsubscribe         = "unsubscribe"
	ActionListSubscriptions   = "list-subscriptions"
	ActionAutoSubscribeConfig = "auto-subscribe-config"
)

// Entity mutation operations.
const (
	ActionCreateIssue     = "create-issue"
	ActionCreateComment   = "create-comment"
	ActionCreateReview    = "create-review"
	ActionMergePR         = "merge-pr"
	ActionTriggerWorkflow = "trigger-workflow"
	ActionGetWorkflowLogs = "get-workflow-logs"
	ActionReportStatus    = "report-status"
)

// Repository query operations.
const (
	ActionListRepos = "list-repos"
	ActionRepoInfo  = "repo-info"
)

// ProviderAction constructs a full grant action string from a
// provider and action suffix: "github" + "subscribe" → "github/subscribe".
func ProviderAction(provider Provider, action string) string {
	return string(provider) + "/" + action
}
