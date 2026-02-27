// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import "github.com/bureau-foundation/bureau/lib/ref"

// Bureau event type constants for machine infrastructure, service
// discovery, and transport. Content types for machine events live in
// events_machine.go; content types for templates live in
// events_template.go.
//
// Event type constants for domains with dedicated sub-packages live
// alongside their content types:
//   - Agent:       lib/schema/agent/       (EventTypeAgentSession, etc.)
//   - Artifact:    lib/schema/artifact/    (EventTypeArtifactScope)
//   - Fleet:       events_fleet.go         (EventTypeFleetService, etc.)
//   - Log:         lib/schema/log/         (EventTypeLog)
//   - Pipeline:    lib/schema/pipeline/    (EventTypePipelineConfig, EventTypePipelineResult)
//   - Auth:        authorization.go        (EventTypeAuthorization, etc.)
//   - Revocation:  events_revocation.go    (EventTypeCredentialRevocation)
const (
	// EventTypeMachineKey is published to #bureau/machine when a machine
	// first boots. Contains the machine's age public key for credential
	// encryption.
	//
	// State key: machine localpart (e.g., "machine/workstation")
	// Room: #bureau/machine:<server>
	EventTypeMachineKey ref.EventType = "m.bureau.machine_key"

	// EventTypeMachineInfo is published to #bureau/machine when a
	// machine's daemon starts (or when hardware configuration changes).
	// Contains static system inventory: CPU topology, total memory,
	// GPU hardware, board identity. Unlike MachineStatus (periodic
	// heartbeat with changing values), MachineInfo is published once
	// and only updated if the hardware inventory changes.
	//
	// State key: machine localpart (e.g., "machine/workstation")
	// Room: #bureau/machine:<server>
	EventTypeMachineInfo ref.EventType = "m.bureau.machine_info"

	// EventTypeMachineStatus is published to #bureau/machine by each
	// machine's daemon as a periodic heartbeat with resource stats.
	//
	// State key: machine localpart (e.g., "machine/workstation")
	// Room: #bureau/machine:<server>
	EventTypeMachineStatus ref.EventType = "m.bureau.machine_status"

	// EventTypeMachineConfig is published to a per-machine config room
	// and defines which principals should run on that machine.
	//
	// State key: machine localpart (e.g., "machine/workstation")
	// Room: #<machine-localpart>:<server>
	EventTypeMachineConfig ref.EventType = "m.bureau.machine_config"

	// EventTypeCredentials is published to a per-machine config room
	// and contains age-encrypted credential bundles for a specific
	// principal on that machine.
	//
	// State key: principal localpart (e.g., "iree/amdgpu/pm")
	// Room: #<machine-localpart>:<server>
	EventTypeCredentials ref.EventType = "m.bureau.credentials"
)

// Service discovery and binding event types. The service
// infrastructure is cross-cutting — all Bureau services (tickets,
// artifacts, pipelines, fleet) use these to register and bind.
const (
	// EventTypeService is published to #bureau/service when a principal
	// starts providing a service. Used for service discovery.
	//
	// State key: principal localpart (e.g., "service/stt/whisper")
	// Room: #bureau/service:<server>
	EventTypeService ref.EventType = "m.bureau.service"

	// EventTypeServiceReady is a timeline event sent by a service after
	// it joins a room and finishes processing the room's configuration.
	// Consumers use this as a deterministic readiness signal — the
	// service is guaranteed to accept requests for this room after
	// this event is sent. Not a state event: message events are
	// append-only, so a rogue service cannot overwrite another
	// service's readiness signal. The sender field (set by the
	// homeserver) authenticates the source.
	//
	// Room: any room the service has joined and is tracking
	EventTypeServiceReady ref.EventType = "m.bureau.service_ready"

	// EventTypeServiceBinding declares which service principal handles a
	// given service role in a room. This is a general mechanism for
	// room-scoped service binding — any service type (tickets, CI, code
	// review, RAG) uses the same event type with different state keys.
	//
	// The daemon reads these events at sandbox creation time to resolve
	// a template's RequiredServices to concrete service principals and
	// their socket paths.
	//
	// State key: service role name (e.g., "ticket", "rag", "ci")
	// Room: any room that uses the service
	EventTypeServiceBinding ref.EventType = "m.bureau.service_binding"
)

// Transport event types for cross-machine daemon connectivity.
const (
	// EventTypeWebRTCOffer is published to #bureau/machine when a daemon
	// wants to establish a WebRTC PeerConnection to another daemon. The
	// offerer gathers all ICE candidates (vanilla ICE), generates a
	// complete SDP offer, and publishes it as a state event. The target
	// daemon polls for offers directed at it and responds with an answer.
	//
	// State key: "<offerer-localpart>|<target-localpart>"
	// The pipe character is not valid in Matrix localparts, so it
	// unambiguously separates the two machine identities.
	//
	// Room: #bureau/machine:<server>
	EventTypeWebRTCOffer ref.EventType = "m.bureau.webrtc_offer"

	// EventTypeWebRTCAnswer is published by the target daemon in response
	// to a WebRTC offer. Uses the same state key format as the offer so
	// the offerer can poll for answers to its outstanding offers.
	//
	// State key: "<offerer-localpart>|<target-localpart>"
	// Room: #bureau/machine:<server>
	EventTypeWebRTCAnswer ref.EventType = "m.bureau.webrtc_answer"
)

// Template event type. Content types in events_template.go.
const (
	// EventTypeTemplate defines a sandbox template — the complete
	// specification for how to create a sandboxed environment. Templates
	// are stored as state events in template rooms (e.g.,
	// #bureau/template for built-ins, #iree/template for project
	// templates). Room power levels control who can edit templates.
	//
	// State key: template name (e.g., "base", "llm-agent", "amdgpu-developer")
	// Room: template room (e.g., #bureau/template:<server>, #iree/template:<server>)
	//
	// Templates can inherit from other templates across rooms. The daemon
	// resolves the full inheritance chain and merges fields before sending
	// a fully-resolved SandboxSpec to the launcher via IPC.
	EventTypeTemplate ref.EventType = "m.bureau.template"
)

// Observation event type. Content types in lib/schema/observation/.
// This constant remains here because ConfigRoomPowerLevels and
// WorkspaceRoomPowerLevels reference it for room permission setup.
const (
	// EventTypeLayout describes the tmux session structure for an
	// observation target. Published to the room associated with the
	// target: a principal's room, a channel room, or a machine's config
	// room.
	//
	// For principal layouts, the state key is the principal's localpart
	// (e.g., "iree/amdgpu/pm"). For channel/room-level layouts, the
	// state key is empty ("").
	//
	// The daemon reads these events to create and reconcile tmux sessions.
	// Changes to the live tmux session are synced back as state event
	// updates. See observation.md for the full bidirectional sync design.
	EventTypeLayout ref.EventType = "m.bureau.layout"
)

// Workspace event types. Content types in lib/schema/workspace/.
// These constants remain here because WorkspaceRoomPowerLevels
// references them for room permission setup.
const (
	// EventTypeProject declares a workspace project — the top-level
	// organizational unit in /var/bureau/workspace/. Contains the git
	// repository URL (if git-backed), worktree definitions, and directory
	// structure. The daemon reads this during reconciliation and compares
	// it to the host filesystem to determine what setup work is needed.
	//
	// State key: project name (e.g., "iree", "lore", "bureau")
	// Room: the project's workspace room
	EventTypeProject ref.EventType = "m.bureau.project"

	// EventTypeWorkspace tracks the lifecycle of a workspace. Published
	// to the workspace room with an empty state key (one workspace per
	// room). The status field progresses through the lifecycle:
	// pending → active → teardown → archived | removed.
	//
	// Principals gate on this via StartCondition with ContentMatch.
	// Agent principals match {"status": "active"} so they start when
	// setup completes and stop when teardown begins. A teardown
	// principal matches {"status": "teardown"} so it starts only when
	// the workspace is being torn down. This is the continuous
	// enforcement mechanism: the daemon re-evaluates conditions every
	// reconcile cycle, so status transitions drive principal lifecycle
	// automatically.
	//
	// State key: "" (singleton per room)
	// Room: the workspace room
	EventTypeWorkspace ref.EventType = "m.bureau.workspace"

	// EventTypeWorktree tracks individual git worktree lifecycle within
	// a workspace. Published to the workspace room with state key = the
	// worktree path relative to the project root (e.g., "feature/amdgpu").
	//
	// Lifecycle:
	//   creating → active    (init pipeline success)
	//   creating → failed    (init pipeline failure)
	//   active   → removing  (remove requested)
	//   removing → archived  (archive mode success)
	//   removing → removed   (delete mode success)
	//   removing → failed    (deinit pipeline failure)
	//   failed   → creating  (retry)
	//
	// Principals gate on this via StartCondition with ContentMatch,
	// the same mechanism as workspace status. An agent principal that
	// needs its worktree ready matches {"status": "active"} on the
	// worktree event.
	//
	// State key: worktree path relative to project root
	// Room: the workspace room
	EventTypeWorktree ref.EventType = "m.bureau.worktree"
)

// Pipeline definition event type. Content types in lib/schema/pipeline/.
// EventTypePipelineConfig and EventTypePipelineResult have been moved
// to lib/schema/pipeline/ alongside their content types.
// EventTypePipeline remains here because PipelineRoomPowerLevels
// references it for room permission setup.
const (
	// EventTypePipeline defines a pipeline — a structured sequence of
	// steps that run inside a Bureau sandbox. Pipelines are stored as
	// state events in pipeline rooms (e.g., #bureau/pipeline for
	// built-ins, #iree/pipeline for project pipelines). Room power
	// levels control who can edit pipelines.
	//
	// State key: pipeline name (e.g., "dev-workspace-init", "dev-worktree-teardown")
	// Room: pipeline room (e.g., #bureau/pipeline:<server>)
	EventTypePipeline ref.EventType = "m.bureau.pipeline"
)

// Ticket event types. Content types in lib/schema/ticket/.
// These constants remain here because lib/schema/ticket/ imports this
// package (for ContentMatch). Moving the constants there would require
// this package to import lib/schema/ticket/ in return, creating a
// circular dependency.
const (
	// EventTypeTicket is a work item tracked in a room by the ticket
	// service. Each ticket is a state event whose state key is the
	// ticket ID (e.g., "tkt-a3f9"). The ticket service maintains an
	// indexed cache of these events for fast queries. Historical
	// versions are preserved in the room timeline.
	//
	// State key: ticket ID (e.g., "tkt-a3f9")
	// Room: any room with ticket management enabled (has EventTypeTicketConfig)
	EventTypeTicket ref.EventType = "m.bureau.ticket"

	// EventTypeTicketConfig enables and configures ticket management
	// for a room. Rooms without this event do not accept ticket
	// operations from the ticket service. Published by the admin
	// (via "bureau ticket enable") alongside the service binding
	// and service invitation.
	//
	// State key: "" (singleton per room)
	// Room: any room that wants ticket management
	EventTypeTicketConfig ref.EventType = "m.bureau.ticket_config"
)

// Stewardship event type. Content types in lib/schema/stewardship/.
// This constant remains here because WorkspaceRoomPowerLevels
// references it for room permission setup.
const (
	// EventTypeStewardship declares resource governance in a room.
	// Each stewardship declaration maps resource patterns to
	// responsible principals with tiered review escalation. The
	// ticket service resolves these declarations against tickets'
	// affects fields to auto-configure review gates or queue
	// notifications.
	//
	// State key: resource identifier (e.g., "fleet/gpu", "workspace/lib/schema")
	// Room: any room the ticket service is a member of
	EventTypeStewardship ref.EventType = "m.bureau.stewardship"
)

// Standard Matrix event type constants. These are Matrix spec types
// (not Bureau-specific) that Bureau code references frequently. Defined
// here so that callers avoid hardcoding matrix protocol strings.
const (
	MatrixEventTypeMessage           ref.EventType = "m.room.message"
	MatrixEventTypePowerLevels       ref.EventType = "m.room.power_levels"
	MatrixEventTypeJoinRules         ref.EventType = "m.room.join_rules"
	MatrixEventTypeRoomName          ref.EventType = "m.room.name"
	MatrixEventTypeRoomTopic         ref.EventType = "m.room.topic"
	MatrixEventTypeSpaceChild        ref.EventType = "m.space.child"
	MatrixEventTypeCanonicalAlias    ref.EventType = "m.room.canonical_alias"
	MatrixEventTypeEncryption        ref.EventType = "m.room.encryption"
	MatrixEventTypeServerACL         ref.EventType = "m.room.server_acl"
	MatrixEventTypeTombstone         ref.EventType = "m.room.tombstone"
	MatrixEventTypeRoomAvatar        ref.EventType = "m.room.avatar"
	MatrixEventTypeHistoryVisibility ref.EventType = "m.room.history_visibility"
	MatrixEventTypeRoomMember        ref.EventType = "m.room.member"
)

// Power level tiers for command authorization. The daemon checks the
// sender's power level in the room before executing any command.
// These tiers align with ConfigRoomPowerLevels conventions.
const (
	// PowerLevelReadOnly allows read-only queries: workspace.status,
	// workspace.list, workspace.du. Any room member can run these.
	PowerLevelReadOnly = 0

	// PowerLevelOperator allows mutating but non-destructive operations:
	// workspace.fetch, workspace.worktree.list. Requires operator status
	// in the room (PL 50).
	PowerLevelOperator = 50

	// PowerLevelAdmin allows full lifecycle control: workspace.setup,
	// workspace.restore, principal.spawn. Requires admin status in the
	// room (PL 100).
	PowerLevelAdmin = 100
)

// AdminProtectedEvents returns the set of standard Matrix room-level events
// that Bureau restricts to admin power level (100) in all rooms. Every
// power level function uses this as its base events map, adding
// Bureau-specific overrides on top. Returns a fresh map each call so
// callers can safely merge their own entries.
func AdminProtectedEvents() map[ref.EventType]any {
	return map[ref.EventType]any{
		MatrixEventTypeRoomAvatar:        100,
		MatrixEventTypeCanonicalAlias:    100,
		MatrixEventTypeEncryption:        100,
		MatrixEventTypeHistoryVisibility: 100,
		MatrixEventTypeJoinRules:         100,
		MatrixEventTypeRoomName:          100,
		MatrixEventTypePowerLevels:       100,
		MatrixEventTypeServerACL:         100,
		MatrixEventTypeTombstone:         100,
		MatrixEventTypeRoomTopic:         100,
		MatrixEventTypeSpaceChild:        100,
	}
}

// powerLevelSpec describes the variable parts of a Bureau room's power level
// structure. All Bureau rooms share the same skeleton (ban/kick/redact at
// PL 100, room notification at PL 100, users_default at 0). The spec
// captures only the fields that vary between room types.
type powerLevelSpec struct {
	users         map[string]any
	events        map[ref.EventType]any
	eventsDefault int
	stateDefault  int
	invite        int
}

// roomPowerLevels constructs a Matrix m.room.power_levels content object
// from a spec. Every Bureau room type function calls AdminProtectedEvents(),
// adds domain-specific event overrides, and passes the result here.
func roomPowerLevels(spec powerLevelSpec) map[string]any {
	return map[string]any{
		"users":          spec.users,
		"users_default":  0,
		"events":         spec.events,
		"events_default": spec.eventsDefault,
		"state_default":  spec.stateDefault,
		"ban":            100,
		"kick":           100,
		"invite":         spec.invite,
		"redact":         100,
		"notifications": map[string]any{
			"room": 100,
		},
	}
}

// adminMachineUsers returns a users power level map with admin at PL 100
// and machine at PL 50. If machineUserID is zero or matches adminUserID,
// only the admin entry is included.
func adminMachineUsers(adminUserID, machineUserID ref.UserID) map[string]any {
	users := map[string]any{
		adminUserID.String(): 100,
	}
	if !machineUserID.IsZero() && machineUserID != adminUserID {
		users[machineUserID.String()] = 50
	}
	return users
}

// WorkspaceRoomPowerLevels returns the power level structure for workspace
// collaboration rooms. Unlike config rooms (events_default: 100, admin-only),
// workspace rooms are collaboration spaces where agents send messages freely.
//
// Three tiers:
//   - Admin (100): project config, room metadata, join rules, power levels
//   - Machine/daemon (50): invite principals into workspace rooms
//   - Default (0): messages, workspace state, layout, read state
//
// state_default is 0: any room member can publish any state event type not
// explicitly listed. Authorization relies on room membership — the room is
// invite-only, and only the daemon (PL 50) or admin can invite. This means
// new Bureau state event types (tickets, artifacts, etc.) work in workspace
// rooms without updating this function. AdminProtectedEvents locks all
// Matrix room metadata (m.room.*, m.space.*) at PL 100 regardless.
//
// The explicit PL 0 entries for workspace/worktree/layout are documentation
// of the expected state event types, not load-bearing — they'd fall through
// to state_default: 0 anyway.
//
// The admin creates workspace rooms via "bureau workspace create", so using
// this as PowerLevelContentOverride in CreateRoom is safe (the creator
// already has PL 100 from the preset).
func WorkspaceRoomPowerLevels(adminUserID, machineUserID ref.UserID) map[string]any {
	events := AdminProtectedEvents()
	events[EventTypeProject] = 100
	events[EventTypeStewardship] = 100
	events[EventTypeWorkspace] = 0
	events[EventTypeWorktree] = 0
	events[EventTypeLayout] = 0

	return roomPowerLevels(powerLevelSpec{
		users:         adminMachineUsers(adminUserID, machineUserID),
		events:        events,
		eventsDefault: 0,
		stateDefault:  0,
		invite:        50,
	})
}

// ConfigRoomPowerLevels returns power level content for a per-machine config
// room. Two tiers:
//
//   - Admin (100): credential writes, room metadata, power level management,
//     kicking machines during decommission. The admin is the only user at
//     PL 100, keeping the config room's trust boundary tight.
//   - Machine + fleet controllers (50): MachineConfig writes (HA hosting,
//     service placement), layout publishes, invites, and message sends.
//     PL 50 is sufficient for operational writes but cannot modify
//     credentials (PL 100), power levels (PL 100), or room metadata (PL 100).
//   - Default (0): read-only (principals, other members).
//
// The admin creates config rooms via "bureau machine provision". The admin
// gets PL 100 from the private_chat preset, then applies this power level
// structure as a state event. The machine joins via invite and operates at
// PL 50.
func ConfigRoomPowerLevels(adminUserID, machineUserID ref.UserID) map[string]any {
	events := AdminProtectedEvents()
	events[EventTypeMachineConfig] = 50 // machine and fleet controllers (PL 50) write placements
	events[EventTypeCredentials] = 100  // admin only
	events[EventTypeLayout] = 0         // daemon publishes layout state
	events[MatrixEventTypeMessage] = 0  // daemon posts command results and pipeline results

	return roomPowerLevels(powerLevelSpec{
		users:         adminMachineUsers(adminUserID, machineUserID),
		events:        events,
		eventsDefault: 100,
		stateDefault:  0,
		invite:        50,
	})
}

// PipelineRoomPowerLevels returns the power level structure for pipeline
// content rooms (e.g., #bureau/pipeline). Pipeline rooms are content
// repositories: only the admin publishes pipeline definitions, everyone
// else reads.
//
// Unlike workspace rooms (events_default: 0 for collaboration) or config
// rooms (machine at PL 50 for layouts), pipeline rooms are admin-only
// for writes. No machine tier is needed — machines read pipelines via
// their proxy, they don't write them.
func PipelineRoomPowerLevels(adminUserID ref.UserID) map[string]any {
	events := AdminProtectedEvents()
	events[EventTypePipeline] = 100

	return roomPowerLevels(powerLevelSpec{
		users: map[string]any{
			adminUserID.String(): 100,
		},
		events:        events,
		eventsDefault: 100,
		stateDefault:  100,
		invite:        100,
	})
}
