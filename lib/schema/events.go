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
//   - #bureau/templates:<server> — Built-in sandbox templates (base, base-networked)
//
// Per-project/org template rooms:
//
//   - #iree/templates:<server>   — Project-specific templates
//
// Workspace rooms (created by bureau workspace create):
//
//   - #iree/amdgpu/inference:<server> — Per-workspace: ProjectConfig, WorkspaceReady, WorkspaceTeardown
//
// Per-machine rooms (created by the launcher at registration):
//
//   - #bureau/config/<machine-localpart>:<server>  — MachineConfig, Credentials for one machine
//
// The per-machine config room ensures that credential ciphertext is only
// visible to the machine that can decrypt it (plus the admin account).
// Template rooms use standard Matrix power levels for edit control.
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

	// EventTypeTemplate defines a sandbox template — the complete
	// specification for how to create a sandboxed environment. Templates
	// are stored as state events in template rooms (e.g.,
	// #bureau/templates for built-ins, #iree/templates for project
	// templates). Room power levels control who can edit templates.
	//
	// State key: template name (e.g., "base", "llm-agent", "amdgpu-developer")
	// Room: template room (e.g., #bureau/templates:<server>, #iree/templates:<server>)
	//
	// Templates can inherit from other templates across rooms. The daemon
	// resolves the full inheritance chain and merges fields before sending
	// a fully-resolved SandboxSpec to the launcher via IPC.
	EventTypeTemplate = "m.bureau.template"

	// EventTypeProject declares a workspace project — the top-level
	// organizational unit in /var/bureau/workspace/. Contains the git
	// repository URL (if git-backed), worktree definitions, and directory
	// structure. The daemon reads this during reconciliation and compares
	// it to the host filesystem to determine what setup work is needed.
	//
	// State key: project name (e.g., "iree", "lore", "bureau")
	// Room: the project's workspace room
	EventTypeProject = "m.bureau.project"

	// EventTypeWorkspaceReady is published by the setup principal after
	// it finishes creating the workspace (clone, worktrees, config files).
	// Other principals use this as a gate via StartCondition — they don't
	// start until this event exists in the workspace room.
	//
	// State key: "" (singleton per room)
	// Room: the workspace room
	EventTypeWorkspaceReady = "m.bureau.workspace.ready"

	// EventTypeWorkspaceTeardown triggers workspace teardown. When the
	// daemon sees this event, it spawns a teardown principal (if
	// configured via lifecycle hooks) to archive and clean up the
	// workspace. Teardown is separate from Matrix room archival — a room
	// can be tombstoned while its workspace persists, or vice versa.
	//
	// State key: "" (singleton per room)
	// Room: the workspace room
	EventTypeWorkspaceTeardown = "m.bureau.workspace.teardown"
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

	// Template is a template reference identifying which template to
	// instantiate for this principal. The format is:
	//
	//   <room-alias-localpart>:<template-name>
	//
	// Examples:
	//   - "bureau/templates:base" — built-in base template
	//   - "bureau/templates:llm-agent" — built-in agent template
	//   - "iree/templates:amdgpu-developer" — project-specific template
	//
	// For federated deployments, an optional @server suffix on the room
	// reference specifies the homeserver:
	//
	//   "iree/templates@other.example:amdgpu-developer"
	//
	// The daemon resolves this reference to a room alias and state event,
	// walks the inheritance chain, and produces a fully-resolved SandboxSpec.
	// See ParseTemplateRef for parsing semantics.
	Template string `json:"template"`

	// AutoStart controls whether the daemon should start this principal's
	// sandbox immediately at boot, or wait for a wake event (message,
	// service request, etc.).
	AutoStart bool `json:"auto_start"`

	// Labels are free-form key-value metadata for organizational purposes:
	// observation layout filtering, dashboard grouping, UI categorization.
	// Labels do not affect access control — that's handled by ObservePolicy,
	// MatrixPolicy, and localpart glob patterns. Changing labels does not
	// require a sandbox restart; the daemon picks up label changes on the
	// next config sync.
	//
	// Common conventions:
	//   - "role": "agent", "service", "coordinator" — principal function
	//   - "team": "iree", "infra" — organizational grouping
	//   - "tier": "gpu", "cpu" — resource classification
	Labels map[string]string `json:"labels,omitempty"`

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

	// ServiceVisibility is a list of glob patterns that control which
	// services this principal can discover via GET /v1/services. Patterns
	// match against service localparts using Bureau's hierarchical glob
	// syntax (same as ObservePolicy patterns):
	//   - "service/stt/*" — all STT services
	//   - "service/**" — all services under service/
	//   - "**" — all services (use with caution)
	//
	// An empty or nil list means the principal cannot see any services
	// (default-deny). The daemon pushes these patterns to the principal's
	// proxy, which filters the directory before returning results.
	ServiceVisibility []string `json:"service_visibility,omitempty"`

	// CommandOverride replaces the template's Command for this specific
	// principal instance. Use this when the same template should run a
	// different entrypoint on different machines or for different
	// principals (e.g., a template defines the environment but each
	// principal runs a different model). When nil, the template's Command
	// is used unchanged.
	CommandOverride []string `json:"command_override,omitempty"`

	// EnvironmentOverride replaces the template's Environment (Nix store
	// path) for this principal. When empty, the template's Environment is
	// used. This enables pinning a specific principal to a different Nix
	// closure than the template default — useful for canary deployments
	// or per-principal version pinning.
	EnvironmentOverride string `json:"environment_override,omitempty"`

	// ExtraEnvironmentVariables are merged into the template's
	// EnvironmentVariables map for this principal instance. These are
	// applied after template inheritance resolution, so they override
	// any variable set by the template or its parents. Use this for
	// per-instance configuration that doesn't warrant a separate
	// template (e.g., MODEL_NAME, BATCH_SIZE).
	ExtraEnvironmentVariables map[string]string `json:"extra_environment_variables,omitempty"`

	// Payload is instance-specific data passed to the agent at startup
	// and available for hot-reload via SIGHUP. The daemon merges this
	// over the template's DefaultPayload (Payload values win on
	// conflict) and writes the result to /run/bureau/payload.json
	// inside the sandbox. Use this for per-instance agent configuration:
	// project context, task assignments, model parameters, etc.
	Payload map[string]any `json:"payload,omitempty"`

	// StartCondition gates this principal's launch on the existence of a
	// specific state event in a specific room. When set, the daemon's
	// reconciliation loop checks whether the referenced event exists
	// before creating the sandbox. If the event is absent, the principal
	// is deferred until a subsequent /sync delivers it.
	//
	// The primary use case is workspace lifecycle sequencing: agent
	// principals gate on EventTypeWorkspaceReady so they don't start
	// until the setup principal has finished creating the workspace.
	// Other uses include service dependencies, staged rollouts, and
	// manual approval gates.
	//
	// When nil, the principal starts normally (subject to AutoStart).
	StartCondition *StartCondition `json:"start_condition,omitempty"`
}

// StartCondition specifies a state event that must exist before a principal
// can launch. The daemon checks this during reconciliation: resolve the room
// alias, fetch the state event, and proceed only if it exists.
type StartCondition struct {
	// EventType is the Matrix state event type to check for (e.g.,
	// "m.bureau.workspace.ready"). Must be a valid Matrix event type.
	EventType string `json:"event_type"`

	// StateKey is the state key to look up. Empty string is valid and
	// common — it means the singleton instance of that event type.
	StateKey string `json:"state_key"`

	// RoomAlias is the full Matrix room alias where the event should
	// exist (e.g., "#iree/amdgpu/inference:bureau.local"). The daemon
	// resolves this to a room ID (cached) before fetching the event.
	// When empty, the daemon checks the principal's own config room.
	RoomAlias string `json:"room_alias,omitempty"`
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

// TemplateContent is the content of an EventTypeTemplate state event. It
// defines the complete sandbox specification for a class of principals:
// filesystem mounts, namespace isolation, resource limits, security settings,
// entrypoint command, environment variables, and agent roles.
//
// This is the Matrix wire-format representation. The sandbox package defines
// parallel runtime types (sandbox.Profile, sandbox.Mount, etc.) with YAML
// tags for local file authoring. The daemon converts between the two when
// resolving templates. The same pattern is used for LayoutContent vs
// observe.Layout.
//
// Templates can inherit from other templates via the Inherits field. The
// daemon walks the inheritance chain, merging fields at each step:
// base template -> parent (via Inherits) -> child -> instance overrides.
// Slices (Filesystem, CreateDirs) are appended; maps (EnvironmentVariables,
// Roles, DefaultPayload) are merged with child values winning; scalars
// (Command, Environment) are replaced if non-zero in the child.
type TemplateContent struct {
	// Description is a human-readable summary of what this template
	// provides (e.g., "GPU-accelerated LLM agent with IREE runtime").
	Description string `json:"description,omitempty"`

	// Inherits is a template reference identifying the parent template.
	// The daemon resolves the full inheritance chain before producing a
	// SandboxSpec. Cycles are detected and rejected. Format is the same
	// as PrincipalAssignment.Template: "room-alias-localpart:template-name".
	// Empty means no inheritance (this template is self-contained).
	Inherits string `json:"inherits,omitempty"`

	// Command is the entrypoint command and arguments to run inside the
	// sandbox. The first element is the executable path; subsequent
	// elements are arguments. Empty inherits from the parent template.
	Command []string `json:"command,omitempty"`

	// Environment is the Nix store path providing the sandbox's /usr/local
	// toolchain (e.g., "/nix/store/abc123-bureau-agent-env"). Resolved at
	// template publish time, not at runtime. The daemon prefetches missing
	// store paths from the Attic binary cache before creating the sandbox.
	// Empty means no Nix environment (use host /usr/bin via bind mounts).
	Environment string `json:"environment,omitempty"`

	// EnvironmentVariables are environment variables set inside the sandbox.
	// During inheritance, child values override parent values for the same
	// key. Values may contain ${VARIABLE} references expanded at sandbox
	// creation time (e.g., "${WORKTREE}" for the host worktree path).
	EnvironmentVariables map[string]string `json:"environment_variables,omitempty"`

	// Filesystem is the list of mount points for the sandbox. During
	// inheritance, child mounts are appended after parent mounts.
	// The sandbox package's MergeProfiles handles deduplication by Dest.
	Filesystem []TemplateMount `json:"filesystem,omitempty"`

	// Namespaces controls which Linux namespaces are unshared for the
	// sandbox. When nil, inherits from the parent template.
	Namespaces *TemplateNamespaces `json:"namespaces,omitempty"`

	// Resources sets resource limits (cgroup constraints) for the sandbox.
	// When nil, inherits from the parent template.
	Resources *TemplateResources `json:"resources,omitempty"`

	// Security configures sandbox security options (setsid, PR_SET_NO_NEW_PRIVS,
	// etc.). When nil, inherits from the parent template.
	Security *TemplateSecurity `json:"security,omitempty"`

	// CreateDirs lists directories to create inside the sandbox before
	// starting the command. During inheritance, child dirs are appended
	// after parent dirs (duplicates are removed).
	CreateDirs []string `json:"create_dirs,omitempty"`

	// Roles maps role names to their commands. The observation layout
	// system uses roles to determine what each pane shows (e.g., "agent"
	// runs the main agent process, "shell" runs a debug shell). Each
	// role's value is a command array (same format as Command).
	Roles map[string][]string `json:"roles,omitempty"`

	// RequiredCredentials lists the credential names that must be present
	// in the principal's encrypted credential bundle before the sandbox
	// can start. The daemon checks this against the Credentials state
	// event and refuses to start the sandbox if any are missing.
	RequiredCredentials []string `json:"required_credentials,omitempty"`

	// DefaultPayload is the default agent payload merged under
	// PrincipalAssignment.Payload at resolution time (instance values
	// win on conflict). Written to /run/bureau/payload.json inside the
	// sandbox. Use this for template-level defaults that individual
	// instances can override.
	DefaultPayload map[string]any `json:"default_payload,omitempty"`
}

// TemplateMount describes a filesystem mount point in a template. This is
// the JSON wire-format counterpart to sandbox.Mount (which uses YAML tags).
// The daemon converts between the two during template resolution.
type TemplateMount struct {
	// Source is the host path to mount. Supports ${VARIABLE} expansion
	// (e.g., "${WORKTREE}" for the principal's worktree directory).
	// Empty when Type is "tmpfs" or other virtual filesystem.
	Source string `json:"source,omitempty"`

	// Dest is the mount destination path inside the sandbox. Required.
	Dest string `json:"dest"`

	// Type is the filesystem type. Empty means bind mount (the common
	// case). Other values: "tmpfs" for a temporary filesystem.
	Type string `json:"type,omitempty"`

	// Mode is the access mode: "ro" for read-only, "rw" for read-write.
	// Empty defaults to "ro" in the sandbox package.
	Mode string `json:"mode,omitempty"`

	// Options are mount-specific options (e.g., "size=64M" for tmpfs).
	Options string `json:"options,omitempty"`

	// Optional marks this mount as non-fatal if the source does not
	// exist. When true, the sandbox skips this mount silently rather
	// than failing. Use for paths that exist on some machines but not
	// others (e.g., /lib64 on non-multilib systems, /nix on non-Nix
	// machines).
	Optional bool `json:"optional,omitempty"`
}

// TemplateNamespaces controls which Linux namespaces are unshared for the
// sandbox. Each field corresponds to a bwrap --unshare-* flag.
type TemplateNamespaces struct {
	// PID unshares the PID namespace (--unshare-pid). Processes inside
	// the sandbox see their own PID 1.
	PID bool `json:"pid,omitempty"`

	// Net unshares the network namespace (--unshare-net). The sandbox
	// gets an isolated network stack with only loopback. External
	// network access goes through the bridge/proxy.
	Net bool `json:"net,omitempty"`

	// IPC unshares the IPC namespace (--unshare-ipc). Isolates System V
	// IPC and POSIX message queues.
	IPC bool `json:"ipc,omitempty"`

	// UTS unshares the UTS namespace (--unshare-uts). The sandbox gets
	// its own hostname.
	UTS bool `json:"uts,omitempty"`
}

// TemplateResources defines resource limits for the sandbox, enforced via
// cgroup constraints when systemd user scopes are available.
type TemplateResources struct {
	// CPUShares is the relative CPU weight (cgroup cpu.shares). Higher
	// values get proportionally more CPU time under contention. Zero
	// means no limit (use cgroup default).
	CPUShares int `json:"cpu_shares,omitempty"`

	// MemoryLimitMB is the maximum memory in megabytes (cgroup
	// memory.max). Zero means no limit.
	MemoryLimitMB int `json:"memory_limit_mb,omitempty"`

	// PidsLimit is the maximum number of processes (cgroup pids.max).
	// Zero means no limit.
	PidsLimit int `json:"pids_limit,omitempty"`
}

// TemplateSecurity configures sandbox security options passed to bwrap.
type TemplateSecurity struct {
	// NewSession calls setsid(2) to create a new session for the sandbox
	// process, detaching it from the controlling terminal. This prevents
	// the sandboxed process from sending signals to the parent process
	// group.
	NewSession bool `json:"new_session,omitempty"`

	// DieWithParent sets PR_SET_PDEATHSIG so the sandbox process receives
	// SIGKILL when its parent dies. Prevents orphaned sandbox processes.
	DieWithParent bool `json:"die_with_parent,omitempty"`

	// NoNewPrivs sets PR_SET_NO_NEW_PRIVS, preventing the sandbox process
	// (and its children) from gaining privileges through execve of setuid
	// binaries.
	NoNewPrivs bool `json:"no_new_privs,omitempty"`
}

// ProjectConfig is the content of an EventTypeProject state event. It
// declares a workspace project: the top-level unit in
// /var/bureau/workspace/<project>/. For git-backed projects, it specifies
// the repository URL and worktree layout. For non-git projects (creative
// writing, ML training, etc.), it specifies directory structure.
//
// The daemon reads this during reconciliation and compares it to the host
// filesystem. If the workspace doesn't exist or is missing worktrees, the
// daemon spawns a setup principal to create them.
//
// The room alias maps mechanically to the filesystem path:
// #iree/amdgpu/inference:bureau.local → /var/bureau/workspace/iree/amdgpu/inference/
type ProjectConfig struct {
	// Repository is the git clone URL for git-backed projects (e.g.,
	// "https://github.com/iree-org/iree.git"). Empty for non-git
	// workspaces. When set, the setup principal clones this as a bare
	// repo into .bare/ under the project root.
	Repository string `json:"repository,omitempty"`

	// WorkspacePath is the project's directory name under
	// /var/bureau/workspace/ (e.g., "iree", "lore", "bureau"). This is
	// always the first segment of the room alias and is redundant with
	// the state key, but included explicitly for clarity and to
	// decouple the struct from its storage context.
	WorkspacePath string `json:"workspace_path"`

	// DefaultBranch is the primary branch name for git-backed projects
	// (e.g., "main", "master"). The setup principal creates a worktree
	// named "main/" tracking this branch. Empty for non-git workspaces.
	DefaultBranch string `json:"default_branch,omitempty"`

	// Worktrees maps worktree path suffixes to their configuration for
	// git-backed projects. The key is the path relative to the project
	// root (e.g., "amdgpu/inference", "remoting"). The setup principal
	// creates each worktree via git worktree add.
	//
	// Empty or nil for non-git workspaces.
	Worktrees map[string]WorktreeConfig `json:"worktrees,omitempty"`

	// Directories maps directory path suffixes to their configuration
	// for non-git workspaces. The key is the path relative to the
	// project root (e.g., "novel4", "training-data"). The setup
	// principal creates each directory.
	//
	// Empty or nil for git-backed workspaces.
	Directories map[string]DirectoryConfig `json:"directories,omitempty"`
}

// WorktreeConfig describes a single git worktree within a project.
type WorktreeConfig struct {
	// Branch is the git branch to check out in this worktree (e.g.,
	// "feature/amdgpu-inference", "main"). The setup principal runs
	// git worktree add with this branch.
	Branch string `json:"branch"`

	// Description is a human-readable description of what this worktree
	// is for (e.g., "AMDGPU inference pipeline development").
	Description string `json:"description,omitempty"`
}

// DirectoryConfig describes a single directory within a non-git workspace.
type DirectoryConfig struct {
	// Description is a human-readable description of what this directory
	// is for (e.g., "Fourth novel workspace", "Training dataset").
	Description string `json:"description,omitempty"`
}

// WorkspaceReady is the content of an EventTypeWorkspaceReady state event.
// Published by the setup principal after the workspace is fully initialized.
// Principals with StartCondition referencing this event will start only
// after it appears.
type WorkspaceReady struct {
	// SetupPrincipal is the localpart of the setup principal that
	// completed the workspace initialization (e.g., "iree/setup").
	SetupPrincipal string `json:"setup_principal"`

	// CompletedAt is an ISO 8601 timestamp of when setup finished.
	CompletedAt string `json:"completed_at"`
}

// WorkspaceTeardown is the content of an EventTypeWorkspaceTeardown state
// event. Triggers workspace teardown when published to a workspace room.
type WorkspaceTeardown struct {
	// RequestedBy is the Matrix user ID of whoever initiated the
	// teardown (e.g., "@bureau-admin:bureau.local").
	RequestedBy string `json:"requested_by"`

	// RequestedAt is an ISO 8601 timestamp of when teardown was
	// requested.
	RequestedAt string `json:"requested_at"`

	// Action specifies what to do with the workspace data: "delete"
	// removes it permanently, "archive" moves it to cold storage.
	Action string `json:"action"`

	// ArchivePath is the destination for archived data when Action is
	// "archive" (e.g., "/var/bureau/archive/iree/amdgpu/inference/").
	// Empty when Action is "delete".
	ArchivePath string `json:"archive_path,omitempty"`
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
	// Labels filters members whose PrincipalAssignment labels contain all
	// specified key-value pairs (subset match). An empty or nil map means
	// all members pass the filter. Example: {"role": "agent"} matches any
	// principal labeled role=agent; {"role": "agent", "team": "iree"}
	// matches only principals with both labels set to those values.
	Labels map[string]string `json:"labels,omitempty"`
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
			EventTypeMachineConfig:      100,
			EventTypeCredentials:        100,
			EventTypeLayout:             0, // daemon publishes layout state
			"m.room.name":               100,
			"m.room.topic":              100,
			"m.room.avatar":             100,
			"m.room.canonical_alias":    100,
			"m.room.history_visibility": 100,
			"m.room.power_levels":       100,
			"m.room.join_rules":         100,
		},
		"events_default": 100,
		"state_default":  100,
		"ban":            100,
		"kick":           100,
		"invite":         100,
		"redact":         100,
		"notifications": map[string]any{
			"room": 100,
		},
	}
}
