// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import "github.com/bureau-foundation/bureau/lib/ref"

// Credential revocation event type constants. These are state event types
// used by the emergency revocation workflow to record that a machine's
// credentials have been revoked.
const (
	// EventTypeCredentialRevocation is published to #bureau/machine when
	// an administrator revokes a machine's credentials via
	// "bureau machine revoke". It records which principals and credential
	// keys were affected, whether the Matrix account was deactivated, and
	// who initiated the revocation.
	//
	// Other machines and future connectors watch for this event to
	// invalidate cached tokens and revoke external API keys provisioned
	// through the affected machine.
	//
	// State key: machine localpart (e.g., "machine/workstation")
	// Room: #bureau/machine:<server>
	EventTypeCredentialRevocation ref.EventType = "m.bureau.credential_revocation"
)

// CredentialRevocationContent is the content of an
// EventTypeCredentialRevocation state event.
type CredentialRevocationContent struct {
	// Machine is the localpart of the revoked machine
	// (e.g., "machine/workstation").
	Machine string `json:"machine"`

	// MachineUserID is the full Matrix user ID of the revoked machine
	// (e.g., "@machine/workstation:bureau.local").
	MachineUserID ref.UserID `json:"machine_user_id"`

	// Principals lists the localparts of principals that had credentials
	// on this machine at revocation time.
	Principals []string `json:"principals,omitempty"`

	// CredentialKeys lists the state keys of credential state events
	// that were tombstoned during revocation.
	CredentialKeys []string `json:"credential_keys,omitempty"`

	// InitiatedBy is the Matrix user ID of the admin who initiated
	// the revocation.
	InitiatedBy ref.UserID `json:"initiated_by"`

	// InitiatedAt is the RFC 3339 timestamp of when the revocation
	// was initiated.
	InitiatedAt string `json:"initiated_at"`

	// Reason is an optional human-readable explanation for the
	// revocation (e.g., "machine compromised", "employee departure").
	Reason string `json:"reason,omitempty"`

	// AccountDeactivated is true if the machine's Matrix account was
	// successfully deactivated. If false, account deactivation failed
	// (but credential state events were still cleared and the machine
	// was kicked from rooms).
	AccountDeactivated bool `json:"account_deactivated"`
}
