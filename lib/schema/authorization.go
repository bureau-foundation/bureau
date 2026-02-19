// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

// Bureau authorization event type constants. These are state event types
// used by the authorization framework to store grants, allowances, and
// token signing keys in Matrix rooms.
const (
	// EventTypeAuthorization is a room-level authorization policy that
	// grants actions to all members of a room, optionally scoped by
	// power level. The daemon reads these during /sync and merges them
	// with per-principal policies in the authorization index.
	//
	// State key: "" (singleton per room)
	// Room: any room (workspace rooms, project rooms, etc.)
	EventTypeAuthorization = "m.bureau.authorization"

	// EventTypeTemporalGrant is a time-bounded grant added at runtime,
	// typically through a ticket-backed access request workflow. The
	// daemon merges temporal grants into the principal's resolved
	// policy on /sync and removes them on expiry or tombstone.
	//
	// State key: ticket reference or grant ID (e.g., "tkt-c4d1")
	// Room: the principal's room
	EventTypeTemporalGrant = "m.bureau.temporal_grant"

	// EventTypeTokenSigningKey publishes the daemon's Ed25519 public
	// key so that services can discover it for token verification. The
	// daemon publishes this at startup if the key has changed.
	//
	// State key: machine localpart (e.g., "machine/workstation")
	// Room: #bureau/system
	EventTypeTokenSigningKey = "m.bureau.token_signing_key"

	// EventTypeAudit is a timeline event recording an authorization
	// decision. Posted by the daemon to the per-machine config room
	// for denials and sensitive grants. Timeline event (not state) —
	// each decision is a distinct occurrence, not a replacement.
	//
	// Room: #<machine-localpart>:<server>
	EventTypeAudit = "m.bureau.audit"
)

// Authorization source constants identify where a policy entry came from
// in the merge hierarchy. The daemon stamps these on each entry during
// authorization index rebuild. They appear in the Source field of Grant,
// Denial, Allowance, and AllowanceDenial.
const (
	// SourceMachineDefault marks entries from MachineConfig.DefaultPolicy.
	SourceMachineDefault = "machine-default"

	// SourcePrincipal marks entries from a principal's own
	// PrincipalAssignment.Authorization policy.
	SourcePrincipal = "principal"

	// SourceTemporal marks entries from temporal grant state events.
	// The grant's Ticket, GrantedBy, and GrantedAt fields provide
	// further provenance.
	SourceTemporal = "temporal"
)

// SourceRoom returns the source string for a room-level authorization
// policy. The roomID is the Matrix room ID where the m.bureau.authorization
// state event was found.
func SourceRoom(roomID string) string {
	return "room:" + roomID
}

// Grant gives a principal permission to perform actions on targets.
// Grants are subject-side: they describe what the principal can do.
//
// For self-service actions (joining rooms, discovering services, running
// CLI commands), Targets is empty — the principal acts on infrastructure,
// not on another principal.
//
// For cross-principal actions (observing, interrupting, provisioning
// credentials), Targets contains localpart patterns identifying which
// principals or resources the grant applies to.
type Grant struct {
	// Actions is a list of action patterns (glob syntax). The principal
	// can perform any action matching any pattern in this list.
	Actions []string `json:"actions" cbor:"1,keyasint"`

	// Targets is a list of localpart patterns (glob syntax) identifying
	// which principals or resources this grant applies to. Empty means
	// the grant applies to non-targeted actions only (self-service ops
	// like matrix/join, service/discover, command/*).
	Targets []string `json:"targets,omitempty" cbor:"2,keyasint,omitempty"`

	// ExpiresAt is an optional RFC 3339 timestamp. After this time,
	// the grant is no longer effective and is ignored during evaluation.
	// The daemon garbage-collects expired grants on the next /sync cycle.
	// Omit for permanent grants (the common case for template-defined
	// and room-level grants).
	ExpiresAt string `json:"expires_at,omitempty" cbor:"3,keyasint,omitempty"`

	// Ticket is an optional ticket reference (e.g., "tkt-a3f9") linking
	// this grant to the ticket that authorized it. Provides audit trail
	// for temporary and escalated grants. Omit for grants that don't
	// originate from a ticket workflow.
	Ticket string `json:"ticket,omitempty" cbor:"4,keyasint,omitempty"`

	// GrantedBy is the Matrix user ID of the principal that created this
	// grant. Set automatically by the daemon when processing grant
	// requests. Omit for static grants (template-defined, room-level).
	GrantedBy string `json:"granted_by,omitempty" cbor:"5,keyasint,omitempty"`

	// GrantedAt is an RFC 3339 timestamp of when this grant was created.
	// Set automatically by the daemon. Omit for static grants.
	GrantedAt string `json:"granted_at,omitempty" cbor:"6,keyasint,omitempty"`

	// Source identifies where this grant came from in the policy merge
	// hierarchy. Set by the daemon during authorization index rebuild,
	// not by users in config files. Values: "machine-default" (from
	// MachineConfig.DefaultPolicy), "principal" (from the principal's
	// own PrincipalAssignment.Authorization), "temporal" (from a
	// temporal grant state event), "room:<room_id>" (from a room-level
	// authorization policy).
	Source string `json:"source,omitempty" cbor:"7,keyasint,omitempty"`
}

// Denial explicitly blocks actions. Evaluated after grants — a matching
// denial overrides any matching grant. Use sparingly: prefer narrow
// grants over broad grants + denials.
type Denial struct {
	// Actions is a list of action patterns to deny.
	Actions []string `json:"actions" cbor:"1,keyasint"`

	// Targets is a list of localpart patterns to deny the action
	// against. Empty means deny self-service actions.
	Targets []string `json:"targets,omitempty" cbor:"2,keyasint,omitempty"`

	// Source identifies where this denial came from. See Grant.Source.
	Source string `json:"source,omitempty" cbor:"3,keyasint,omitempty"`
}

// Allowance permits specific actors to perform specific actions on a
// principal. This is the target-side counterpart to grants: grants say
// what A can do, allowances say who can act on B.
type Allowance struct {
	// Actions is a list of action patterns (glob syntax). Actors
	// matching the Actors field can perform these actions on this
	// principal.
	Actions []string `json:"actions" cbor:"1,keyasint"`

	// Actors is a list of localpart patterns (glob syntax) identifying
	// who can perform the allowed actions. The acting principal's
	// localpart must match at least one pattern.
	Actors []string `json:"actors" cbor:"2,keyasint"`

	// Source identifies where this allowance came from. See Grant.Source.
	Source string `json:"source,omitempty" cbor:"3,keyasint,omitempty"`
}

// AllowanceDenial explicitly blocks specific actors from specific
// actions, overriding any matching allowance.
type AllowanceDenial struct {
	// Actions is a list of action patterns to deny.
	Actions []string `json:"actions" cbor:"1,keyasint"`

	// Actors is a list of localpart patterns to deny.
	Actors []string `json:"actors" cbor:"2,keyasint"`

	// Source identifies where this allowance denial came from. See
	// Grant.Source.
	Source string `json:"source,omitempty" cbor:"3,keyasint,omitempty"`
}

// AuthorizationPolicy defines what a principal can do (grants) and
// what others can do to it (allowances). Stored in PrincipalAssignment
// and resolved during template inheritance.
//
// During template inheritance, all four lists are appended — child adds
// to parent, never removes. This means base templates can establish
// security invariants (via Denials) that child templates cannot override.
type AuthorizationPolicy struct {
	// Grants define what this principal can do.
	Grants []Grant `json:"grants,omitempty" cbor:"1,keyasint,omitempty"`

	// Denials explicitly block actions that would otherwise be granted.
	// Evaluated after grants: a matching denial overrides any matching
	// grant.
	Denials []Denial `json:"denials,omitempty" cbor:"2,keyasint,omitempty"`

	// Allowances define what others can do to this principal.
	Allowances []Allowance `json:"allowances,omitempty" cbor:"3,keyasint,omitempty"`

	// AllowanceDenials explicitly block actors that would otherwise
	// be allowed. Evaluated after allowances: a matching denial
	// overrides any matching allowance.
	AllowanceDenials []AllowanceDenial `json:"allowance_denials,omitempty" cbor:"4,keyasint,omitempty"`
}

// RoomAuthorizationPolicy is the content of an EventTypeAuthorization
// state event. It grants actions to all members of a room, optionally
// differentiated by power level.
//
// The daemon reads these during /sync and merges them with per-principal
// policies. A principal's effective grants are the union of their
// per-principal grants, all room-level MemberGrants from rooms they
// belong to, and any PowerLevelGrants they qualify for.
type RoomAuthorizationPolicy struct {
	// MemberGrants are grants automatically given to every member
	// of this room. Merged with per-principal grants.
	MemberGrants []Grant `json:"member_grants,omitempty"`

	// PowerLevelGrants map Matrix power levels to additional grants.
	// A principal with power level >= the key gets the associated
	// grants in addition to MemberGrants. Keys are strings because
	// JSON object keys must be strings; values are parsed as integers.
	PowerLevelGrants map[string][]Grant `json:"power_level_grants,omitempty"`
}

// TemporalGrantContent is the content of an EventTypeTemporalGrant
// state event. It represents a time-bounded grant added at runtime
// through a ticket-backed access request workflow.
//
// Publishing an event with empty content (tombstone) for the same
// state key revokes the temporal grant.
type TemporalGrantContent struct {
	// Grant is the time-bounded grant. The ExpiresAt field should be
	// set to bound the grant's lifetime.
	Grant Grant `json:"grant"`

	// Principal is the localpart of the principal receiving this grant.
	// The daemon uses this to merge the grant into the correct
	// principal's resolved policy.
	Principal string `json:"principal"`
}

// TokenSigningKeyContent is the content of an EventTypeTokenSigningKey
// state event. Published by the daemon at startup so services can
// discover the public key for token verification.
type TokenSigningKeyContent struct {
	// PublicKey is the Ed25519 public key bytes (32 bytes), hex-encoded.
	PublicKey string `json:"public_key"`

	// Machine is the machine localpart that owns this signing key.
	Machine string `json:"machine"`
}

// AuditDecision identifies what happened in an audit event.
type AuditDecision string

const (
	// AuditAllow means the action was permitted.
	AuditAllow AuditDecision = "allow"

	// AuditDeny means the action was denied.
	AuditDeny AuditDecision = "deny"

	// AuditGrantCreated means a temporal grant was created.
	AuditGrantCreated AuditDecision = "grant_created"

	// AuditGrantRevoked means a temporal grant was revoked.
	AuditGrantRevoked AuditDecision = "grant_revoked"

	// AuditGrantExpired means a temporal grant expired.
	AuditGrantExpired AuditDecision = "grant_expired"
)

// AuditEventContent is the content of an EventTypeAudit timeline event.
// Posted by the daemon to the per-machine config room for authorization
// denials and sensitive grants.
type AuditEventContent struct {
	// Decision is what happened: allow, deny, or a grant lifecycle event.
	Decision AuditDecision `json:"decision"`

	// Actor is the localpart of the principal that attempted the action.
	Actor string `json:"actor"`

	// Action is the authorization action that was checked (e.g.,
	// "observe/read-write", "credential/provision/key/FORGEJO_TOKEN").
	Action string `json:"action"`

	// Target is the localpart of the principal or resource being acted
	// on. Empty for self-service actions.
	Target string `json:"target,omitempty"`

	// Reason describes why the check was denied. Empty for allow
	// decisions and grant lifecycle events.
	Reason string `json:"reason,omitempty"`

	// EnforcementPoint identifies where the check happened (e.g.,
	// "daemon/observe", "daemon/temporal_grant").
	EnforcementPoint string `json:"enforcement_point"`

	// Machine is the machine localpart where the check occurred.
	Machine string `json:"machine"`

	// MatchedGrant is the grant that matched, if any.
	MatchedGrant *Grant `json:"matched_grant,omitempty"`

	// MatchedDenial is the denial that fired, if any.
	MatchedDenial *Denial `json:"matched_denial,omitempty"`

	// MatchedAllowance is the allowance that matched, if any.
	MatchedAllowance *Allowance `json:"matched_allowance,omitempty"`

	// MatchedAllowanceDenial is the allowance denial that fired, if any.
	MatchedAllowanceDenial *AllowanceDenial `json:"matched_allowance_denial,omitempty"`

	// Ticket is the ticket reference for grant lifecycle events.
	Ticket string `json:"ticket,omitempty"`

	// ExpiresAt is the grant expiry for grant lifecycle events.
	ExpiresAt string `json:"expires_at,omitempty"`
}
