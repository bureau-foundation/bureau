// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package schema defines the Matrix state event types and content structures
// used by Bureau. These are the "Bureau protocol" — the set of Matrix events
// that Bureau components read and write to coordinate machines, principals,
// credentials, and services.
//
// Event type constants are the Matrix event types. Go structs define the JSON
// content of each event. State keys are always the principal's localpart
// (e.g., "machine/workstation", "iree/amdgpu/pm", "service/stt/whisper").
//
// # Room layout
//
// Shared rooms (created by bureau-matrix-setup):
//
//   - #bureau/machines:<server>  — MachineKey, MachineStatus events for all machines
//   - #bureau/services:<server>  — Service registration events
//
// Per-machine rooms (created by the launcher at registration):
//
//   - #bureau/config/<machine-localpart>:<server>  — MachineConfig, Credentials for one machine
//
// The per-machine config room ensures that credential ciphertext is only
// visible to the machine that can decrypt it (plus the admin account).
package schema

// Matrix state event type constants. These are the "type" field in Matrix
// state events. The state_key for each is the principal's localpart.
const (
	// EventTypeMachineKey is published to #bureau/machines when a machine
	// first boots. Contains the machine's age public key for credential
	// encryption.
	//
	// State key: machine localpart (e.g., "machine/workstation")
	// Room: #bureau/machines:<server>
	EventTypeMachineKey = "m.bureau.machine_key"

	// EventTypeMachineStatus is published to #bureau/machines by each
	// machine's daemon as a periodic heartbeat with resource stats.
	//
	// State key: machine localpart (e.g., "machine/workstation")
	// Room: #bureau/machines:<server>
	EventTypeMachineStatus = "m.bureau.machine_status"

	// EventTypeMachineConfig is published to a per-machine config room
	// and defines which principals should run on that machine.
	//
	// State key: machine localpart (e.g., "machine/workstation")
	// Room: #bureau/config/<machine-localpart>:<server>
	EventTypeMachineConfig = "m.bureau.machine_config"

	// EventTypeCredentials is published to a per-machine config room
	// and contains age-encrypted credential bundles for a specific
	// principal on that machine.
	//
	// State key: principal localpart (e.g., "iree/amdgpu/pm")
	// Room: #bureau/config/<machine-localpart>:<server>
	EventTypeCredentials = "m.bureau.credentials"

	// EventTypeService is published to #bureau/services when a principal
	// starts providing a service. Used for service discovery.
	//
	// State key: principal localpart (e.g., "service/stt/whisper")
	// Room: #bureau/services:<server>
	EventTypeService = "m.bureau.service"

	// EventTypeWebRTCOffer is published to #bureau/machines when a daemon
	// wants to establish a WebRTC PeerConnection to another daemon. The
	// offerer gathers all ICE candidates (vanilla ICE), generates a
	// complete SDP offer, and publishes it as a state event. The target
	// daemon polls for offers directed at it and responds with an answer.
	//
	// State key: "<offerer-localpart>|<target-localpart>"
	// The pipe character is not valid in Matrix localparts, so it
	// unambiguously separates the two machine identities.
	//
	// Room: #bureau/machines:<server>
	EventTypeWebRTCOffer = "m.bureau.webrtc_offer"

	// EventTypeWebRTCAnswer is published by the target daemon in response
	// to a WebRTC offer. Uses the same state key format as the offer so
	// the offerer can poll for answers to its outstanding offers.
	//
	// State key: "<offerer-localpart>|<target-localpart>"
	// Room: #bureau/machines:<server>
	EventTypeWebRTCAnswer = "m.bureau.webrtc_answer"

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
	// updates. See OBSERVATION.md for the full bidirectional sync design.
	EventTypeLayout = "m.bureau.layout"
)

// MachineKey is the content of an EventTypeMachineKey state event.
// Published by the launcher at first boot after generating the machine's
// age keypair.
type MachineKey struct {
	// Algorithm identifies the key type. Always "age-x25519" for now.
	Algorithm string `json:"algorithm"`

	// PublicKey is the age public key (e.g., "age1xyz..."). Other machines
	// and the operator use this to encrypt credential bundles for this
	// machine.
	PublicKey string `json:"public_key"`
}

// MachineStatus is the content of an EventTypeMachineStatus state event.
// Published periodically by the daemon as a heartbeat with resource stats.
type MachineStatus struct {
	// Principal is the full Matrix user ID of the machine
	// (e.g., "@machine/workstation:bureau.local").
	Principal string `json:"principal"`

	// CPUPercent is the current CPU utilization (0-100, may exceed 100 on
	// multi-core for per-core reporting).
	CPUPercent float64 `json:"cpu_percent"`

	// MemoryUsedGB is the current memory usage in gigabytes.
	MemoryUsedGB float64 `json:"memory_used_gb"`

	// GPUUtilizationPercent is the GPU utilization (0-100). Zero if no GPU
	// or GPU monitoring is unavailable.
	GPUUtilizationPercent float64 `json:"gpu_utilization_percent,omitempty"`

	// Sandboxes reports counts of sandbox states on this machine.
	Sandboxes SandboxCounts `json:"sandboxes"`

	// UptimeSeconds is the machine uptime in seconds.
	UptimeSeconds int64 `json:"uptime_seconds"`

	// LastActivityAt is an ISO 8601 timestamp of the last meaningful daemon
	// activity — sandbox creation, destruction, credential rotation, config
	// reconciliation, etc. Consumers compute idle duration as now minus this
	// timestamp. Empty on the first heartbeat before any activity occurs.
	LastActivityAt string `json:"last_activity_at,omitempty"`

	// TransportAddress identifies this machine for daemon-to-daemon
	// transport. With WebRTC, this is the machine's Matrix user ID
	// (e.g., "@machine/workstation:bureau.local") — peer daemons use it
	// as the signaling target for WebRTC connection establishment.
	// Empty if the daemon is not accepting transport connections.
	TransportAddress string `json:"transport_address,omitempty"`
}

// SandboxCounts reports how many sandboxes are in each state.
type SandboxCounts struct {
	Running int `json:"running"`
	Idle    int `json:"idle"`
}

// MachineConfig is the content of an EventTypeMachineConfig state event.
// Defines which principals should run on a machine. The daemon reads this
// at startup and on change to reconcile running sandboxes.
type MachineConfig struct {
	// Principals is the list of principals assigned to this machine.
	Principals []PrincipalAssignment `json:"principals"`

	// DefaultObservePolicy is the fallback observation policy for
	// principals on this machine that do not specify their own
	// ObservePolicy in PrincipalAssignment. When nil, observation is
	// denied for any principal without an explicit per-principal policy.
	// This provides a convenient machine-wide default while allowing
	// per-principal overrides.
	DefaultObservePolicy *ObservePolicy `json:"default_observe_policy,omitempty"`
}

// PrincipalAssignment defines a single principal that should run on a machine.
type PrincipalAssignment struct {
	// Localpart is the principal's localpart (e.g., "iree/amdgpu/pm").
	// Must pass principal.ValidateLocalpart.
	Localpart string `json:"localpart"`

	// Template is the name of the template to instantiate (e.g., "llm-agent",
	// "whisper-stt"). Templates are defined as state events in
	// #bureau/templates or as checked-in YAML files.
	Template string `json:"template"`

	// AutoStart controls whether the daemon should start this principal's
	// sandbox immediately at boot, or wait for a wake event (message,
	// service request, etc.).
	AutoStart bool `json:"auto_start"`

	// MatrixPolicy controls which self-service Matrix operations this
	// principal's proxy allows. The homeserver enforces room membership
	// and power levels; this policy controls whether the agent can change
	// its own membership (join rooms, invite others, create rooms).
	// When nil or zero-valued, all self-service membership operations are
	// blocked — the agent can only interact with rooms the admin placed
	// it in.
	MatrixPolicy *MatrixPolicy `json:"matrix_policy,omitempty"`

	// ObservePolicy controls who may observe this principal's terminal
	// session and at what access level. When nil, the principal cannot
	// be observed by anyone — default-deny. If set, the daemon checks
	// each observation request against these rules before forking a
	// relay. Overrides MachineConfig.DefaultObservePolicy for this
	// principal.
	ObservePolicy *ObservePolicy `json:"observe_policy,omitempty"`
}

// MatrixPolicy controls which self-service Matrix operations an agent can
// perform through its proxy. The homeserver enforces room membership and
// power levels; this is a defense-in-depth layer that prevents agents from
// expanding their own access.
//
// Default (all false): the agent can only interact with rooms it was
// explicitly placed in by the admin. It cannot join new rooms, accept
// invitations, invite others, or create rooms.
type MatrixPolicy struct {
	// AllowJoin permits the agent to join public rooms and accept room
	// invitations from other agents. When false, the proxy blocks all
	// POST /join requests. Enables general-purpose agents that discover
	// rooms via space membership and accept invitations from coordinator
	// agents.
	AllowJoin bool `json:"allow_join,omitempty"`

	// AllowInvite permits the agent to invite other agents to rooms it
	// is a member of. Enables coordinator/secretary agents that assemble
	// teams and page in other agents.
	AllowInvite bool `json:"allow_invite,omitempty"`

	// AllowRoomCreate permits the agent to create new rooms (e.g.,
	// backchannel rooms for ad-hoc collaboration between agents).
	AllowRoomCreate bool `json:"allow_room_create,omitempty"`
}

// ObservePolicy controls who may observe a principal's terminal session
// and at what access level. Default-deny: a nil or empty policy means no
// observation is permitted. The daemon evaluates this policy on every
// observation request before forking a relay process.
//
// Patterns use the same glob syntax as Bureau's namespace hierarchy:
// "bureau-admin" matches exactly that localpart, "iree/**" matches any
// localpart under the iree/ prefix, and "*" matches any single-segment
// localpart. See principal.MatchPattern for the matching semantics.
type ObservePolicy struct {
	// AllowedObservers lists glob patterns matching observer localparts
	// (the localpart portion of the observer's Matrix user ID) that may
	// observe this principal. An empty list means no one may observe.
	//
	// Examples:
	//   - "bureau-admin" — only the admin account
	//   - "iree/**" — any principal under the iree/ namespace
	//   - "**" — any authenticated identity (use with caution)
	AllowedObservers []string `json:"allowed_observers,omitempty"`

	// ReadWriteObservers lists glob patterns matching observer localparts
	// that may observe with read-write (interactive) access. Observers
	// matching AllowedObservers but not ReadWriteObservers are
	// downgraded to read-only mode regardless of what they request.
	// An empty list means all observers get read-only access.
	//
	// Read-only observation is enforced at the relay level: the relay
	// attaches to tmux with the -r flag and does not forward input to
	// the PTY.
	ReadWriteObservers []string `json:"readwrite_observers,omitempty"`
}

// Credentials is the content of an EventTypeCredentials state event.
// Contains an age-encrypted credential bundle for a specific principal
// on a specific machine. See CREDENTIALS.md for the full lifecycle.
type Credentials struct {
	// Version is the schema version. Currently 1.
	Version int `json:"version"`

	// Principal is the full Matrix user ID of the principal these
	// credentials are for (e.g., "@iree/amdgpu/pm:bureau.local").
	Principal string `json:"principal"`

	// EncryptedFor lists the Matrix user IDs (machines) and key
	// identifiers (e.g., "yubikey:operator-escrow") that can decrypt
	// this bundle. This is informational — the actual recipients are
	// encoded in the age header.
	EncryptedFor []string `json:"encrypted_for"`

	// Keys lists the credential names (not values) contained in the
	// encrypted bundle. Allows auditing without decryption.
	Keys []string `json:"keys"`

	// Ciphertext is the base64-encoded age-encrypted blob. The plaintext
	// is a JSON object mapping credential names to values.
	Ciphertext string `json:"ciphertext"`

	// ProvisionedBy is the Matrix user ID of whoever provisioned these
	// credentials (e.g., "@bureau/operator:bureau.local").
	ProvisionedBy string `json:"provisioned_by"`

	// ProvisionedAt is the ISO 8601 timestamp of when the credentials
	// were provisioned.
	ProvisionedAt string `json:"provisioned_at"`

	// Signature is an ed25519 signature from the provisioner's signing
	// key (typically the operator's YubiKey). The launcher verifies this
	// before trusting the bundle.
	Signature string `json:"signature"`

	// ExpiresAt is an optional ISO 8601 timestamp for credential expiry.
	// A monitoring principal can poll for upcoming expirations.
	ExpiresAt string `json:"expires_at,omitempty"`
}

// Service is the content of an EventTypeService state event.
// Published to #bureau/services when a principal provides a service
// that other principals can consume.
type Service struct {
	// Principal is the full Matrix user ID of the service provider
	// (e.g., "@service/stt/whisper:bureau.local").
	Principal string `json:"principal"`

	// Machine is the full Matrix user ID of the machine running this
	// service instance (e.g., "@machine/cloud-gpu-1:bureau.local").
	Machine string `json:"machine"`

	// Capabilities lists what this service instance supports. The
	// daemon uses these for service selection when a sandbox requests
	// a capability (e.g., ["streaming", "speaker-diarization"]).
	Capabilities []string `json:"capabilities,omitempty"`

	// Protocol is the wire protocol spoken on the service socket
	// (e.g., "http", "grpc", "raw-frames").
	Protocol string `json:"protocol"`

	// Description is a human-readable description of the service.
	Description string `json:"description,omitempty"`

	// Metadata holds service-specific key-value pairs that don't fit
	// into the fixed fields (e.g., supported languages, model version,
	// max batch size). Consumers can filter on these.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// WebRTCSignal is the content of both EventTypeWebRTCOffer and
// EventTypeWebRTCAnswer state events. Contains a complete SDP with all
// ICE candidates gathered (vanilla ICE — no trickle). The same struct
// is used for both offers and answers; the event type distinguishes them.
type WebRTCSignal struct {
	// SDP is the complete Session Description Protocol string including
	// all gathered ICE candidates. For offers this is type "offer"; for
	// answers this is type "answer".
	SDP string `json:"sdp"`

	// Timestamp is an ISO 8601 timestamp of when the signal was created.
	// Used to detect stale signals: if the receiver has already processed
	// a newer signal for this peer pair, this one is ignored.
	Timestamp string `json:"timestamp"`
}

// LayoutContent is the content of an EventTypeLayout state event. It describes
// the tmux session structure for an observation target: windows, panes, and
// what each pane shows.
//
// This is the Matrix wire-format representation. The observe package defines a
// parallel runtime type (observe.Layout). The two have the same shape but are
// separate types: the schema package defines persistence, the observe package
// defines behavior. Conversion between them happens in the daemon.
type LayoutContent struct {
	// Prefix is the tmux prefix key for this session (e.g., "C-a").
	// Empty means use the Bureau default from deploy/tmux/bureau.conf.
	Prefix string `json:"prefix,omitempty"`

	// Windows is the ordered list of tmux windows in the session.
	Windows []LayoutWindow `json:"windows"`

	// SourceMachine is the full Matrix user ID of the machine that
	// published this layout event (e.g., "@machine/workstation:bureau.local").
	// Used for loop prevention: when a daemon receives a layout event
	// that it published itself, it skips applying it to avoid an
	// infinite sync loop.
	SourceMachine string `json:"source_machine,omitempty"`

	// SealedMetadata is reserved for future use. When populated, it
	// will contain an age-encrypted blob with sensitive runtime state
	// (environment variables, checkpoint references, etc.) that should
	// not be stored in plaintext. The structural layout above remains
	// in plaintext for routing and display.
	SealedMetadata string `json:"sealed_metadata,omitempty"`
}

// LayoutWindow describes a single tmux window containing one or more panes.
type LayoutWindow struct {
	// Name is the tmux window name (displayed in the status bar).
	Name string `json:"name"`

	// Panes is the ordered list of panes in this window. The first pane
	// is the root; subsequent panes are splits from it.
	Panes []LayoutPane `json:"panes"`
}

// LayoutPane describes a single pane within a tmux window. Exactly one of
// Observe, Command, Role, or ObserveMembers should be set to determine what
// the pane shows.
type LayoutPane struct {
	// Observe is the principal localpart to observe in this pane
	// (e.g., "iree/amdgpu/pm"). When set, the pane runs bureau-observe
	// connected to this principal. Used in channel/room layouts for
	// composite views.
	Observe string `json:"observe,omitempty"`

	// Command is a shell command to run in this pane. Used for local
	// tooling (beads-tui, dashboards, shells) in composite layouts.
	Command string `json:"command,omitempty"`

	// Role identifies the pane's purpose in a principal's own layout
	// (e.g., "agent", "shell"). The launcher resolves roles to concrete
	// commands based on the agent template. Used in principal layouts.
	Role string `json:"role,omitempty"`

	// ObserveMembers dynamically populates panes from room membership.
	// When set, the daemon creates one pane per room member matching
	// the filter. Used in channel layouts for auto-scaling views.
	ObserveMembers *LayoutMemberFilter `json:"observe_members,omitempty"`

	// Split is the split direction from the previous pane: "horizontal"
	// or "vertical". Empty for the first pane in a window (which is not
	// a split).
	Split string `json:"split,omitempty"`

	// Size is the pane size as a percentage of the available space
	// (1-99). Zero means tmux divides evenly.
	Size int `json:"size,omitempty"`
}

// LayoutMemberFilter selects room members for dynamic pane creation in
// channel layouts with ObserveMembers.
type LayoutMemberFilter struct {
	// Role filters members by their Bureau role (from the principal's
	// identity metadata). Empty means all members.
	Role string `json:"role,omitempty"`
}

// ConfigRoomPowerLevels returns power level overrides for a per-machine config
// room. The admin has power level 100 (can set config and credentials). The
// machine (room creator or invited member) has default power level 0 — it
// reads state events but doesn't write them. Only the admin decides what
// runs on the machine.
func ConfigRoomPowerLevels(adminUserID string) map[string]any {
	return map[string]any{
		"users": map[string]any{
			adminUserID: 100,
		},
		"users_default": 0,
		"events": map[string]any{
			EventTypeMachineConfig: 100,
			EventTypeCredentials:   100,
			EventTypeLayout:        0, // daemon publishes layout state
			"m.room.name":              100,
			"m.room.topic":             100,
			"m.room.avatar":            100,
			"m.room.canonical_alias":   100,
			"m.room.history_visibility": 100,
			"m.room.power_levels":      100,
			"m.room.join_rules":        100,
		},
		"events_default":          100,
		"state_default":           100,
		"ban":                     100,
		"kick":                    100,
		"invite":                  100,
		"redact":                  100,
		"notifications": map[string]any{
			"room": 100,
		},
	}
}
