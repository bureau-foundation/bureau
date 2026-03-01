// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package forge

import "fmt"

// ForgeIdentityVersion is the current schema version.
const ForgeIdentityVersion = 1

// ForgeIdentity is the content of an EventTypeForgeIdentity state
// event. It maps a Bureau entity to a forge user account. For
// per-principal forges (Forgejo), the forge connector creates a
// dedicated user account and provisions credentials. For
// shared-account forges (GitHub App), this records the association
// between the Bureau entity and its attribution metadata for
// auto-subscribe and provenance tracking.
//
// State key: "<provider>/<matrix_localpart>".
type ForgeIdentity struct {
	Version  int    `json:"version"`
	Provider string `json:"provider"`
	// MatrixUser is the full Matrix user ID of the Bureau entity.
	MatrixUser string `json:"matrix_user"`

	// ForgeUser is the forge username. For per-principal forges
	// (Forgejo), this is the connector-created account. For
	// shared-account forges (GitHub), this is empty — all agents
	// share the App bot identity.
	ForgeUser string `json:"forge_user,omitempty"`

	// ForgeUserID is the forge-internal user ID. Per-principal
	// forges only.
	ForgeUserID int64 `json:"forge_user_id,omitempty"`

	// TokenProvisioned indicates whether API credentials have been
	// provisioned for this entity on the forge. Per-principal forges
	// only.
	TokenProvisioned bool `json:"token_provisioned,omitempty"`

	// SigningKeyRegistered indicates whether the entity's SSH signing
	// key has been registered on the forge for commit verification.
	// Per-principal forges only.
	SigningKeyRegistered bool `json:"signing_key_registered,omitempty"`

	// LastSynced is the RFC3339 timestamp of the last identity sync
	// between Bureau and the forge.
	LastSynced string `json:"last_synced,omitempty"`
}

// Validate checks that all required fields are present.
func (fi *ForgeIdentity) Validate() error {
	if fi.Version < 1 {
		return fmt.Errorf("forge identity: version must be >= 1, got %d", fi.Version)
	}
	if fi.Provider == "" {
		return fmt.Errorf("forge identity: provider is required")
	}
	if !Provider(fi.Provider).IsKnown() {
		return fmt.Errorf("forge identity: unknown provider %q", fi.Provider)
	}
	if fi.MatrixUser == "" {
		return fmt.Errorf("forge identity: matrix_user is required")
	}
	return nil
}

// ForgeAutoSubscribeRulesVersion is the current schema version.
const ForgeAutoSubscribeRulesVersion = 1

// ForgeAutoSubscribeRules is the content of an
// EventTypeForgeAutoSubscribe state event. It configures when the
// connector automatically creates entity subscriptions for an agent
// in response to webhook events. Default: all triggers enabled.
//
// State key: Bureau entity localpart.
type ForgeAutoSubscribeRules struct {
	Version int `json:"version"`

	// OnAuthor subscribes the agent to entities it creates (PRs,
	// issues).
	OnAuthor bool `json:"on_author"`

	// OnAssign subscribes the agent to entities assigned to it.
	OnAssign bool `json:"on_assign"`

	// OnMention subscribes the agent when it is @mentioned in a
	// comment or description.
	OnMention bool `json:"on_mention"`

	// OnReviewRequest subscribes the agent when it is requested as
	// a reviewer on a pull request.
	OnReviewRequest bool `json:"on_review_request"`
}

// Validate checks that all required fields are present.
func (r *ForgeAutoSubscribeRules) Validate() error {
	if r.Version < 1 {
		return fmt.Errorf("forge auto-subscribe rules: version must be >= 1, got %d", r.Version)
	}
	return nil
}

// DefaultAutoSubscribeRules returns auto-subscribe rules with all
// triggers enabled.
func DefaultAutoSubscribeRules() ForgeAutoSubscribeRules {
	return ForgeAutoSubscribeRules{
		Version:         ForgeAutoSubscribeRulesVersion,
		OnAuthor:        true,
		OnAssign:        true,
		OnMention:       true,
		OnReviewRequest: true,
	}
}
