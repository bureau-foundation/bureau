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

// Observation operations. Enforced by the daemon (observe_auth.go)
// when an observer requests terminal access to a principal.
//
// ActionObserve gates read-only streaming observation. ActionObserveReadWrite
// gates interactive terminal access (input injection, resize). The
// daemon checks ActionObserve first, then ActionObserveReadWrite for
// write-capable sessions. ActionObserveReadWrite is a sensitive action
// that triggers audit logging on both allow and deny.
const (
	ActionObserve          = "observe"
	ActionObserveReadWrite = "observe/read-write"
	ActionObserveInput     = "observe/input"
	ActionObserveResize    = "observe/resize"
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

// Ticket service operations. Enforced by the ticket service
// (cmd/bureau-ticket-service/) via service token grant verification.
const (
	ActionTicketCreate             = "ticket/create"
	ActionTicketUpdate             = "ticket/update"
	ActionTicketClose              = "ticket/close"
	ActionTicketReopen             = "ticket/reopen"
	ActionTicketList               = "ticket/list"
	ActionTicketShow               = "ticket/show"
	ActionTicketReady              = "ticket/ready"
	ActionTicketBlocked            = "ticket/blocked"
	ActionTicketRanked             = "ticket/ranked"
	ActionTicketStats              = "ticket/stats"
	ActionTicketGrep               = "ticket/grep"
	ActionTicketSearch             = "ticket/search"
	ActionTicketDeps               = "ticket/deps"
	ActionTicketChildren           = "ticket/children"
	ActionTicketEpicHealth         = "ticket/epic-health"
	ActionTicketInfo               = "ticket/info"
	ActionTicketSubscribe          = "ticket/subscribe"
	ActionTicketImport             = "ticket/import"
	ActionTicketReview             = "ticket/review"
	ActionTicketUpcomingGates      = "ticket/upcoming-gates"
	ActionTicketListRooms          = "ticket/list-rooms"
	ActionTicketListMembers        = "ticket/list-members"
	ActionTicketStewardshipList    = "ticket/stewardship-list"
	ActionTicketStewardshipResolve = "ticket/stewardship-resolve"
	ActionTicketStewardshipSet     = "ticket/stewardship-set"
)

// Artifact service operations. Enforced by the artifact service
// (cmd/bureau-artifact-service/) via service token grant verification.
const (
	ActionArtifactStore = "artifact/store"
	ActionArtifactFetch = "artifact/fetch"
	ActionArtifactList  = "artifact/list"
	ActionArtifactTag   = "artifact/tag"
	ActionArtifactPin   = "artifact/pin"
	ActionArtifactGC    = "artifact/gc"
)

// Fleet controller operations. Enforced by the fleet controller
// (cmd/bureau-fleet-controller/) via service token grant verification.
const (
	ActionFleetInfo          = "fleet/info"
	ActionFleetListMachines  = "fleet/list-machines"
	ActionFleetListServices  = "fleet/list-services"
	ActionFleetShowMachine   = "fleet/show-machine"
	ActionFleetShowService   = "fleet/show-service"
	ActionFleetPlace         = "fleet/place"
	ActionFleetUnplace       = "fleet/unplace"
	ActionFleetPlan          = "fleet/plan"
	ActionFleetMachineHealth = "fleet/machine-health"
	ActionFleetAssign        = "fleet/assign"
	ActionFleetProvision     = "fleet/provision"
)

// Wildcard action patterns used in grant construction. These are
// authorization patterns with ** glob syntax, not exact action names.
// They match all actions in their respective namespace.
const (
	ActionObserveAll         = "observe/**"
	ActionInterruptAll       = "interrupt/**"
	ActionServiceAll         = "service/**"
	ActionTicketAll          = "ticket/**"
	ActionArtifactAll        = "artifact/**"
	ActionFleetAll           = "fleet/**"
	ActionCommandAll         = "command/**"
	ActionCommandTicketAll   = "command/ticket/**"
	ActionCommandArtifactAll = "command/artifact/**"
)
