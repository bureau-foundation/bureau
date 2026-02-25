// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package artifact

// Authorization action constants for the artifact service. Enforced
// by the artifact service (cmd/bureau-artifact-service/) via service
// token grant verification. See lib/schema/action.go for the action
// system overview and non-artifact-specific actions.
const (
	ActionStore = "artifact/store"
	ActionFetch = "artifact/fetch"
	ActionList  = "artifact/list"
	ActionTag   = "artifact/tag"
	ActionPin   = "artifact/pin"
	ActionGC    = "artifact/gc"
)

// ActionAll is the wildcard pattern matching all artifact operations.
const ActionAll = "artifact/**"
