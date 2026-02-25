// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

// Authorization action constants. These are the known action strings
// used in Grant.Actions, Denial.Actions, Allowance.Actions, and
// AllowanceDenial.Actions fields across the Bureau authorization system.
//
// Actions use a hierarchical namespace separated by "/" and support
// glob matching (*, ** wildcards) during authorization evaluation.
// These constants define the leaf actions checked at enforcement
// points. They are plain strings, not a named type, because the
// action vocabulary is open-ended (services can define new actions)
// and patterns in grants use glob syntax to match them.
//
// Domain-specific actions live in their respective schema sub-packages:
//   - lib/schema/ticket — ticket service operations
//   - lib/schema/fleet — fleet controller operations
//   - lib/schema/artifact — artifact service operations
//   - lib/schema/observation — observation/terminal access operations
//
// This file defines actions that have no natural sub-package home:
// Matrix client API, interrupts, service discovery, credential
// provisioning, grant approval, and command wildcards.
//
// For the matching semantics, see lib/authorization/match.go.

// Matrix client API operations. Enforced by the proxy
// (proxy/handler.go, proxy/matrix_api.go) when a sandboxed agent
// makes Matrix API requests through the proxy.
const (
	ActionMatrixJoin       = "matrix/join"
	ActionMatrixInvite     = "matrix/invite"
	ActionMatrixCreateRoom = "matrix/create-room"
	ActionMatrixRawAPI     = "matrix/raw-api"
)

// Interrupt operations. Enforced by the daemon when a principal sends
// signals to another principal's process. Both are sensitive actions
// that trigger audit logging on allow and deny.
const (
	ActionInterrupt          = "interrupt"
	ActionInterruptTerminate = "interrupt/terminate"
)

// Service operations. ActionServiceDiscover is enforced by the proxy
// (handler.go HandleServiceDirectory) with localpart-level target
// matching. ActionServiceRegister is used in room-level authorization
// policies for service rooms.
const (
	ActionServiceDiscover = "service/discover"
	ActionServiceRegister = "service/register"
)

// Credential provisioning. Enforced by the daemon (admin.go) when a
// principal requests credential injection into another principal's
// sandbox. The full action is ActionCredentialProvisionKeyPrefix + key
// name (e.g., "credential/provision/key/GITHUB_TOKEN"). Sensitive
// action — all decisions are audit-logged.
const ActionCredentialProvisionKeyPrefix = "credential/provision/key/"

// Grant approval. Enforced by the daemon when a principal approves a
// temporal grant for another principal. The full action is
// ActionGrantApprovePrefix + the action being approved (e.g.,
// "grant/approve/observe"). Sensitive action.
const ActionGrantApprovePrefix = "grant/approve/"

// Wildcard action patterns used in grant construction. These are
// authorization patterns with ** glob syntax, not exact action names.
// They match all actions in their respective namespace. Domain-specific
// wildcards (ticket, fleet, artifact, observation) are defined in
// their respective sub-packages as ActionAll.
const (
	ActionInterruptAll       = "interrupt/**"
	ActionServiceAll         = "service/**"
	ActionCommandAll         = "command/**"
	ActionCommandTicketAll   = "command/ticket/**"
	ActionCommandArtifactAll = "command/artifact/**"
)
