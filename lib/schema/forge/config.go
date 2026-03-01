// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package forge

import "fmt"

// RepositoryBindingVersion is the current schema version.
const RepositoryBindingVersion = 1

// RepositoryBinding is the content of an EventTypeRepository state
// event. It binds a forge repository to a Bureau room. The state key
// encodes provider/owner/repo so a room can bind to repos on multiple
// forges simultaneously.
type RepositoryBinding struct {
	Version    int    `json:"version"`
	Provider   string `json:"provider"`            // "github", "forgejo", "gitlab"
	Owner      string `json:"owner"`               // repository owner or organization
	Repo       string `json:"repo"`                // repository name (without owner)
	URL        string `json:"url"`                 // web URL to the repository
	CloneHTTPS string `json:"clone_https"`         // HTTPS clone URL
	CloneSSH   string `json:"clone_ssh,omitempty"` // SSH clone URL (empty for GitHub App)
}

// Validate checks that all required fields are present.
func (r *RepositoryBinding) Validate() error {
	if r.Version < 1 {
		return fmt.Errorf("repository binding: version must be >= 1, got %d", r.Version)
	}
	if r.Provider == "" {
		return fmt.Errorf("repository binding: provider is required")
	}
	if !Provider(r.Provider).IsKnown() {
		return fmt.Errorf("repository binding: unknown provider %q", r.Provider)
	}
	if r.Owner == "" {
		return fmt.Errorf("repository binding: owner is required")
	}
	if r.Repo == "" {
		return fmt.Errorf("repository binding: repo is required")
	}
	if r.URL == "" {
		return fmt.Errorf("repository binding: url is required")
	}
	if r.CloneHTTPS == "" {
		return fmt.Errorf("repository binding: clone_https is required")
	}
	return nil
}

// StateKey returns the state key for this binding:
// "<provider>/<owner>/<repo>".
func (r *RepositoryBinding) StateKey() string {
	return r.Provider + "/" + r.Owner + "/" + r.Repo
}

// IssueSyncMode controls how forge issues map to Bureau tickets.
type IssueSyncMode string

const (
	// IssueSyncNone disables issue-to-ticket synchronization.
	IssueSyncNone IssueSyncMode = "none"

	// IssueSyncImport creates Bureau tickets from forge issues
	// (forge → Bureau only).
	IssueSyncImport IssueSyncMode = "import"

	// IssueSyncBidirectional synchronizes in both directions: forge
	// issues create Bureau tickets, and Bureau ticket changes push
	// back to the forge.
	IssueSyncBidirectional IssueSyncMode = "bidirectional"
)

// IsKnown reports whether the mode is a recognized value.
func (m IssueSyncMode) IsKnown() bool {
	switch m {
	case IssueSyncNone, IssueSyncImport, IssueSyncBidirectional:
		return true
	}
	return false
}

// ForgeConfigVersion is the current schema version.
const ForgeConfigVersion = 1

// ForgeConfig is the content of an EventTypeForgeConfig state event.
// It configures per-room, per-repo forge connector behavior. The state
// key matches the RepositoryBinding state key.
type ForgeConfig struct {
	Version  int    `json:"version"`
	Provider string `json:"provider"`
	Repo     string `json:"repo"` // "owner/repo"

	// Events lists which forge event categories to process for this
	// room. Events not listed are received by the connector but not
	// dispatched to subscribers in this room. Uses common category
	// names: "push", "pull_request", "issues", "review", "comment",
	// "ci_status".
	Events []string `json:"events"`

	// IssueSync controls issue-to-ticket synchronization.
	IssueSync IssueSyncMode `json:"issue_sync"`

	// PRReviewTickets controls whether opening a PR creates a Bureau
	// review ticket with CI and review gates.
	PRReviewTickets bool `json:"pr_review_tickets"`

	// CIMonitor controls whether the connector publishes
	// m.bureau.pipeline_result state events for CI/CD runs on this repo.
	CIMonitor bool `json:"ci_monitor"`

	// TriageFilter restricts which events are delivered to room
	// subscriptions. When empty, all events for the configured
	// categories are delivered.
	TriageFilter *TriageFilter `json:"triage_filter,omitempty"`

	// AutoSubscribe enables webhook-driven auto-subscribe for this
	// repo's events.
	AutoSubscribe bool `json:"auto_subscribe"`
}

// TriageFilter restricts the events delivered to room-level
// subscriptions.
type TriageFilter struct {
	// Labels restricts issue/PR events to those with at least one
	// matching label.
	Labels []string `json:"labels,omitempty"`

	// EventTypes restricts which event actions are delivered (e.g.,
	// "issue_opened", "pr_opened").
	EventTypes []string `json:"event_types,omitempty"`
}

// Validate checks that all required fields are present.
func (c *ForgeConfig) Validate() error {
	if c.Version < 1 {
		return fmt.Errorf("forge config: version must be >= 1, got %d", c.Version)
	}
	if c.Provider == "" {
		return fmt.Errorf("forge config: provider is required")
	}
	if !Provider(c.Provider).IsKnown() {
		return fmt.Errorf("forge config: unknown provider %q", c.Provider)
	}
	if c.Repo == "" {
		return fmt.Errorf("forge config: repo is required")
	}
	if c.IssueSync != "" && !c.IssueSync.IsKnown() {
		return fmt.Errorf("forge config: unknown issue_sync mode %q", c.IssueSync)
	}
	return nil
}

// ForgeWorkIdentityVersion is the current schema version.
const ForgeWorkIdentityVersion = 1

// ForgeWorkIdentity is the content of an EventTypeForgeWorkIdentity
// state event. It configures the external git identity used in
// Co-authored-by trailers for autonomous agent commits. The state key
// is "<provider>/<owner>/<repo>" for per-repo identity, or "" for the
// room default. Resolution chain: per-repo → room default → namespace
// default.
type ForgeWorkIdentity struct {
	Version     int    `json:"version"`
	DisplayName string `json:"display_name"` // e.g., "IREE Team"
	Email       string `json:"email"`        // e.g., "iree-team@agents.bureau.foundation"
}

// Validate checks that all required fields are present.
func (w *ForgeWorkIdentity) Validate() error {
	if w.Version < 1 {
		return fmt.Errorf("forge work identity: version must be >= 1, got %d", w.Version)
	}
	if w.DisplayName == "" {
		return fmt.Errorf("forge work identity: display_name is required")
	}
	if w.Email == "" {
		return fmt.Errorf("forge work identity: email is required")
	}
	return nil
}

// Provider identifies a forge provider.
type Provider string

const (
	ProviderGitHub  Provider = "github"
	ProviderForgejo Provider = "forgejo"
	ProviderGitLab  Provider = "gitlab"
)

// IsKnown reports whether the provider is a recognized value.
func (p Provider) IsKnown() bool {
	switch p {
	case ProviderGitHub, ProviderForgejo, ProviderGitLab:
		return true
	}
	return false
}
