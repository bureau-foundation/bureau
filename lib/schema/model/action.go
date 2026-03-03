// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package model

// Authorization action constants for the model service. These are
// matched against grant patterns in service tokens. The daemon
// filters the principal's grants to include only model/* actions
// when minting a model service token.
const (
	// Mutating actions — require explicit grants.
	ActionComplete = "model/complete"
	ActionEmbed    = "model/embed"

	// Read-only query actions.
	ActionList   = "model/list"
	ActionStatus = "model/status"

	// Sync publishes alias state events from catalog discovery.
	// Used by the CLI sync command to create, update, and remove
	// aliases based on provider catalog data and selection rules.
	ActionSync = "model/sync"

	// Wildcard — matches all model service actions.
	ActionAll = "model/**"
)
