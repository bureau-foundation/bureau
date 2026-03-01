// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package forge

import "github.com/bureau-foundation/bureau/lib/ref"

// Matrix state event types for forge connector configuration and
// identity. State keys use the pattern "<provider>/<owner>/<repo>"
// for repo-scoped events and "<provider>/<matrix_localpart>" for
// identity-scoped events.
const (
	// EventTypeRepository binds a forge repository to a Bureau room.
	// State key: "<provider>/<owner>/<repo>".
	EventTypeRepository ref.EventType = "m.bureau.repository"

	// EventTypeForgeConfig configures per-room, per-repo forge
	// connector behavior: which events to process, issue sync mode,
	// review ticket creation, CI monitoring, triage filters.
	// State key: "<provider>/<owner>/<repo>".
	EventTypeForgeConfig ref.EventType = "m.bureau.forge_config"

	// EventTypeForgeIdentity maps a Bureau entity to a forge username.
	// Used for webhook attribution (reverse lookup: forge user → Bureau
	// entity) and auto-subscribe. For per-principal forges (Forgejo),
	// includes credential provisioning status. For shared-account forges
	// (GitHub), records the association without a forge account.
	// State key: "<provider>/<matrix_localpart>".
	EventTypeForgeIdentity ref.EventType = "m.bureau.forge_identity"

	// EventTypeForgeAutoSubscribe stores per-agent auto-subscribe rules.
	// Controls when the connector automatically creates entity
	// subscriptions in response to webhook events.
	// State key: Bureau entity localpart.
	EventTypeForgeAutoSubscribe ref.EventType = "m.bureau.forge_auto_subscribe"

	// EventTypeForgeWorkIdentity configures the external git identity
	// used for autonomous agent commits. Decoupled from principal
	// identity because principals are ephemeral. Resolution chain:
	// per-repo → room default → namespace default.
	// State key: "<provider>/<owner>/<repo>" or "" for room default.
	EventTypeForgeWorkIdentity ref.EventType = "m.bureau.forge_work_identity"
)
