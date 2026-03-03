// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"fmt"

	"github.com/bureau-foundation/bureau/lib/ref"
)

// EventTypeForgeAttribution is a timeline event published by the
// proxy when an agent creates a forge entity (PR, issue, commit)
// through a shared-account forge like GitHub. The proxy knows the
// agent's identity (from the credential payload) and the entity
// identifier (from the API response). The connector reads these
// events via /sync to correlate webhooks with the real agent.
const EventTypeForgeAttribution ref.EventType = "m.bureau.forge_attribution"

// ForgeAttribution is the content of an EventTypeForgeAttribution
// timeline event. Published to the repo's room so operators and
// other agents see what was created and by whom.
type ForgeAttribution struct {
	// Agent is the full Matrix user ID of the agent that created
	// the entity (e.g., "@bureau/fleet/prod/agent/coder:bureau.local").
	Agent string `json:"agent"`

	// Provider is the forge provider ("github", "forgejo", "gitlab").
	Provider string `json:"provider"`

	// Repo is the "owner/repo" string.
	Repo string `json:"repo"`

	// EntityType is the kind of entity created ("pull_request",
	// "issue", "commit").
	EntityType string `json:"entity_type"`

	// EntityNumber is the issue/PR number. Zero for commits.
	EntityNumber int `json:"entity_number,omitempty"`

	// EntitySHA is the commit SHA. Empty for issues and PRs.
	EntitySHA string `json:"entity_sha,omitempty"`

	// EntityURL is the web URL to the created entity.
	EntityURL string `json:"entity_url,omitempty"`

	// Title is the entity's title (PR title, issue title, commit
	// message first line). For human-readable display.
	Title string `json:"title,omitempty"`

	// Body is a plain-text summary for Matrix clients that don't
	// render formatted_body.
	Body string `json:"body"`

	// FormattedBody is an HTML summary with links for Matrix clients
	// that support formatted messages.
	FormattedBody string `json:"formatted_body,omitempty"`
}

// Validate checks that all required fields are present.
func (a *ForgeAttribution) Validate() error {
	if a.Agent == "" {
		return fmt.Errorf("forge attribution: agent is required")
	}
	if a.Provider == "" {
		return fmt.Errorf("forge attribution: provider is required")
	}
	if !Provider(a.Provider).IsKnown() {
		return fmt.Errorf("forge attribution: unknown provider %q", a.Provider)
	}
	if a.Repo == "" {
		return fmt.Errorf("forge attribution: repo is required")
	}
	if a.EntityType == "" {
		return fmt.Errorf("forge attribution: entity_type is required")
	}
	if a.EntityNumber == 0 && a.EntitySHA == "" {
		return fmt.Errorf("forge attribution: one of entity_number or entity_sha is required")
	}
	return nil
}
