// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Matrix state event type constants. These are the "type" field in Matrix
// state events. The state_key for each is the principal's localpart.
const (
	// EventTypeMachineKey is published to #bureau/machine when a machine
	// first boots. Contains the machine's age public key for credential
	// encryption.
	//
	// State key: machine localpart (e.g., "machine/workstation")
	// Room: #bureau/machine:<server>
	EventTypeMachineKey = "m.bureau.machine_key"

	// EventTypeMachineInfo is published to #bureau/machine when a
	// machine's daemon starts (or when hardware configuration changes).
	// Contains static system inventory: CPU topology, total memory,
	// GPU hardware, board identity. Unlike MachineStatus (periodic
	// heartbeat with changing values), MachineInfo is published once
	// and only updated if the hardware inventory changes.
	//
	// State key: machine localpart (e.g., "machine/workstation")
	// Room: #bureau/machine:<server>
	EventTypeMachineInfo = "m.bureau.machine_info"

	// EventTypeMachineStatus is published to #bureau/machine by each
	// machine's daemon as a periodic heartbeat with resource stats.
	//
	// State key: machine localpart (e.g., "machine/workstation")
	// Room: #bureau/machine:<server>
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

	// EventTypeService is published to #bureau/service when a principal
	// starts providing a service. Used for service discovery.
	//
	// State key: principal localpart (e.g., "service/stt/whisper")
	// Room: #bureau/service:<server>
	EventTypeService = "m.bureau.service"

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
	EventTypeWebRTCOffer = "m.bureau.webrtc_offer"

	// EventTypeWebRTCAnswer is published by the target daemon in response
	// to a WebRTC offer. Uses the same state key format as the offer so
	// the offerer can poll for answers to its outstanding offers.
	//
	// State key: "<offerer-localpart>|<target-localpart>"
	// Room: #bureau/machine:<server>
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
	// #bureau/template for built-ins, #iree/template for project
	// templates). Room power levels control who can edit templates.
	//
	// State key: template name (e.g., "base", "llm-agent", "amdgpu-developer")
	// Room: template room (e.g., #bureau/template:<server>, #iree/template:<server>)
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
	EventTypeWorkspace = "m.bureau.workspace"

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
	EventTypeWorktree = "m.bureau.worktree"

	// EventTypePipeline defines a pipeline — a structured sequence of
	// steps that run inside a Bureau sandbox. Pipelines are stored as
	// state events in pipeline rooms (e.g., #bureau/pipeline for
	// built-ins, #iree/pipeline for project pipelines). Room power
	// levels control who can edit pipelines.
	//
	// State key: pipeline name (e.g., "dev-workspace-init", "dev-worktree-teardown")
	// Room: pipeline room (e.g., #bureau/pipeline:<server>)
	EventTypePipeline = "m.bureau.pipeline"

	// EventTypePipelineResult is published by the pipeline executor when
	// a pipeline completes (successfully, with failure, or by abort). The
	// ticket service watches these events to evaluate pipeline gates —
	// a TicketGate with type "pipeline" matches the PipelineRef and
	// Conclusion fields from this event.
	//
	// Published to the pipeline's log room (PipelineContent.Log.Room).
	// Pipelines without a log room do not publish result events; the
	// JSONL result log (BUREAU_RESULT_PATH) covers daemon-internal needs.
	//
	// State key: pipeline name/ref (e.g., "dev-workspace-init")
	// Room: the pipeline's log room
	EventTypePipelineResult = "m.bureau.pipeline_result"

	// EventTypeRoomService declares which service principal handles a
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
	EventTypeRoomService = "m.bureau.room_service"

	// EventTypeTicket is a work item tracked in a room by the ticket
	// service. Each ticket is a state event whose state key is the
	// ticket ID (e.g., "tkt-a3f9"). The ticket service maintains an
	// indexed cache of these events for fast queries. Historical
	// versions are preserved in the room timeline.
	//
	// State key: ticket ID (e.g., "tkt-a3f9")
	// Room: any room with ticket management enabled (has EventTypeTicketConfig)
	EventTypeTicket = "m.bureau.ticket"

	// EventTypeTicketConfig enables and configures ticket management
	// for a room. Rooms without this event do not accept ticket
	// operations from the ticket service. Published by the admin
	// (via "bureau ticket enable") alongside the room service binding
	// and service invitation.
	//
	// State key: "" (singleton per room)
	// Room: any room that wants ticket management
	EventTypeTicketConfig = "m.bureau.ticket_config"

	// EventTypeArtifactScope configures artifact integration for a
	// room. Declares which artifact service principal manages this
	// room's artifacts and which tag patterns the room subscribes to
	// for notifications. Per-artifact metadata and per-tag mappings
	// live in the artifact service's own persistent store, not in
	// Matrix state events.
	//
	// State key: "" (singleton per room)
	// Room: any room that works with artifacts
	EventTypeArtifactScope = "m.bureau.artifact_scope"
)

// Matrix m.room.message msgtype constants for Bureau command messages.
// Commands are regular m.room.message events with custom msgtypes (not
// state events). The daemon receives them via /sync timeline events and
// posts threaded replies with the result.
const (
	// MsgTypeCommand is the msgtype for command request messages.
	// Commands are m.room.message events with structured fields for
	// the daemon to parse: command name, workspace target, parameters.
	// The human-readable body field allows the command to display
	// legibly in any Matrix client.
	MsgTypeCommand = "m.bureau.command"

	// MsgTypeCommandResult is the msgtype for command result messages.
	// Posted as threaded replies to the original command message.
	// Contains structured result data alongside a human-readable body
	// for display in standard Matrix clients.
	MsgTypeCommandResult = "m.bureau.command_result"
)

// Matrix m.room.message msgtype constants for Bureau daemon notification
// messages. Notifications are m.room.message events with custom msgtypes
// posted by the daemon to config rooms. Each carries structured fields
// alongside a human-readable body for display in standard Matrix clients.
// Tests match on typed fields, never on body text.
const (
	// MsgTypeServiceDirectoryUpdated is posted after the daemon syncs
	// service room state changes and pushes the updated directory to
	// running proxies.
	MsgTypeServiceDirectoryUpdated = "m.bureau.service_directory_updated"

	// MsgTypeGrantsUpdated is posted after the daemon hot-reloads
	// authorization grants for a running principal's proxy.
	MsgTypeGrantsUpdated = "m.bureau.grants_updated"

	// MsgTypePayloadUpdated is posted after the daemon writes an
	// updated payload file for a running principal.
	MsgTypePayloadUpdated = "m.bureau.payload_updated"

	// MsgTypePrincipalAdopted is posted when the daemon discovers and
	// adopts a running sandbox from a previous daemon instance.
	MsgTypePrincipalAdopted = "m.bureau.principal_adopted"

	// MsgTypeSandboxExited is posted when a sandbox process terminates.
	// Includes exit code and captured terminal output for diagnosis.
	MsgTypeSandboxExited = "m.bureau.sandbox_exited"

	// MsgTypeCredentialsRotated is posted during credential rotation
	// lifecycle: when rotation starts, completes, or fails.
	MsgTypeCredentialsRotated = "m.bureau.credentials_rotated"

	// MsgTypeProxyCrash is posted when a proxy process exits
	// unexpectedly and the daemon attempts recovery.
	MsgTypeProxyCrash = "m.bureau.proxy_crash"

	// MsgTypeHealthCheck is posted when a health check fails and the
	// daemon takes corrective action (rollback or destroy).
	MsgTypeHealthCheck = "m.bureau.health_check"

	// MsgTypeDaemonSelfUpdate is posted during daemon binary
	// self-update lifecycle: in-progress, succeeded, or failed.
	MsgTypeDaemonSelfUpdate = "m.bureau.daemon_self_update"

	// MsgTypeNixPrefetchFailed is posted when the daemon fails to
	// prefetch a principal's Nix environment closure. The prefetch
	// retries automatically on the next reconciliation cycle.
	MsgTypeNixPrefetchFailed = "m.bureau.nix_prefetch_failed"

	// MsgTypePrincipalStartFailed is posted when the daemon cannot
	// start a principal (service resolution failure, token minting
	// failure, or other sandbox creation error).
	MsgTypePrincipalStartFailed = "m.bureau.principal_start_failed"

	// MsgTypePrincipalRestarted is posted when the daemon destroys
	// and recreates a principal because its sandbox configuration
	// changed (template update, environment change).
	MsgTypePrincipalRestarted = "m.bureau.principal_restarted"

	// MsgTypeBureauVersionUpdate is posted when the daemon reconciles
	// a BureauVersion state event — either reporting a prefetch failure
	// or summarizing what binary updates were applied.
	MsgTypeBureauVersionUpdate = "m.bureau.bureau_version_update"
)

// Standard Matrix event type constants. These are Matrix spec types
// (not Bureau-specific) that Bureau code references frequently. Defined
// here so that callers avoid hardcoding matrix protocol strings.
const (
	MatrixEventTypeMessage           = "m.room.message"
	MatrixEventTypePowerLevels       = "m.room.power_levels"
	MatrixEventTypeJoinRules         = "m.room.join_rules"
	MatrixEventTypeRoomName          = "m.room.name"
	MatrixEventTypeRoomTopic         = "m.room.topic"
	MatrixEventTypeSpaceChild        = "m.space.child"
	MatrixEventTypeCanonicalAlias    = "m.room.canonical_alias"
	MatrixEventTypeEncryption        = "m.room.encryption"
	MatrixEventTypeServerACL         = "m.room.server_acl"
	MatrixEventTypeTombstone         = "m.room.tombstone"
	MatrixEventTypeRoomAvatar        = "m.room.avatar"
	MatrixEventTypeHistoryVisibility = "m.room.history_visibility"
	MatrixEventTypeRoomMember        = "m.room.member"
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

// Schema version constants. CanModify methods compare the event's Version
// field against these constants to prevent silent data loss during rolling
// upgrades where different service instances run different code versions.
const (
	// ArtifactScopeVersion is the current schema version for
	// ArtifactScope events. Increment when adding fields that
	// existing code must not silently drop during read-modify-write.
	ArtifactScopeVersion = 1

	// CredentialsVersion is the current schema version for
	// Credentials events.
	CredentialsVersion = 1

	// TicketContentVersion is the current schema version for
	// TicketContent events. Increment when adding fields that
	// existing code must not silently drop during read-modify-write.
	TicketContentVersion = 1

	// TicketConfigVersion is the current schema version for
	// TicketConfigContent events.
	TicketConfigVersion = 1

	// PipelineResultContentVersion is the current schema version for
	// PipelineResultContent events. Increment when adding fields that
	// existing code must not silently drop during read-modify-write.
	PipelineResultContentVersion = 1

	// AgentSessionVersion is the current schema version for
	// AgentSessionContent events. Increment when adding fields that
	// existing code must not silently drop during read-modify-write.
	AgentSessionVersion = 1

	// AgentContextVersion is the current schema version for
	// AgentContextContent events. Increment when adding fields that
	// existing code must not silently drop during read-modify-write.
	AgentContextVersion = 1

	// AgentMetricsVersion is the current schema version for
	// AgentMetricsContent events. Increment when adding fields that
	// existing code must not silently drop during read-modify-write.
	AgentMetricsVersion = 1
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

	// CPUPercent is the current CPU utilization as an integer (0-100, may
	// exceed 100 on multi-core for per-core reporting). Truncated from the
	// actual float value. Integer because Matrix canonical JSON forbids
	// fractional numbers.
	CPUPercent int `json:"cpu_percent"`

	// MemoryUsedMB is the current memory usage in megabytes. Integer
	// megabytes give adequate precision for monitoring while complying
	// with Matrix canonical JSON (which forbids fractional numbers).
	MemoryUsedMB int `json:"memory_used_mb"`

	// GPUStats reports per-GPU dynamic metrics for each GPU on the
	// machine. Each entry is identified by its PCI slot (matching
	// GPUInfo.PCISlot in MachineInfo) for correlation between static
	// inventory and dynamic stats. Empty when no GPUs are present or
	// GPU monitoring is unavailable.
	GPUStats []GPUStatus `json:"gpu_stats,omitempty"`

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

	// Principals reports per-sandbox resource usage derived from cgroup
	// v2 statistics. Keyed by principal localpart. Populated by daemons
	// that support enriched heartbeats. Omitted by older daemons —
	// consumers must handle its absence gracefully.
	Principals map[string]PrincipalResourceUsage `json:"principals,omitempty"`
}

// SandboxCounts reports how many sandboxes are in each state.
type SandboxCounts struct {
	Running int `json:"running"`
	Idle    int `json:"idle"`
}

// GPUStatus reports dynamic metrics for a single GPU in a MachineStatus
// heartbeat. The PCISlot field correlates with GPUInfo.PCISlot in the
// MachineInfo event for this machine, linking dynamic stats to static
// inventory.
type GPUStatus struct {
	// PCISlot is the PCI slot address (e.g., "0000:c3:00.0"). Matches
	// GPUInfo.PCISlot in the MachineInfo event for device correlation.
	PCISlot string `json:"pci_slot"`

	// UtilizationPercent is the GPU compute utilization (0-100).
	// Sourced from AMDGPU_INFO_SENSOR_GPU_LOAD on AMD or NVML on
	// NVIDIA. Zero when monitoring is unavailable.
	UtilizationPercent int `json:"utilization_percent"`

	// VRAMUsedBytes is the current VRAM consumption in bytes.
	// Sourced from sysfs mem_info_vram_used on AMD.
	VRAMUsedBytes int64 `json:"vram_used_bytes"`

	// TemperatureMillidegrees is the GPU junction temperature in
	// millidegrees Celsius (e.g., 55000 = 55.0C). Junction
	// temperature is the hottest point on die — the most relevant
	// metric for thermal throttling. Integer millidegrees comply
	// with Matrix canonical JSON while giving 0.001C precision.
	TemperatureMillidegrees int `json:"temperature_millidegrees"`

	// PowerDrawWatts is the average GPU package power draw in whole
	// watts. Sourced from AMDGPU_INFO_SENSOR_GPU_AVG_POWER on AMD.
	PowerDrawWatts int `json:"power_draw_watts"`

	// GraphicsClockMHz is the current graphics (shader) clock
	// frequency in MHz. Sourced from AMDGPU_INFO_SENSOR_GFX_SCLK.
	GraphicsClockMHz int `json:"graphics_clock_mhz"`

	// MemoryClockMHz is the current memory clock frequency in MHz.
	// Sourced from AMDGPU_INFO_SENSOR_GFX_MCLK.
	MemoryClockMHz int `json:"memory_clock_mhz"`
}

// MachineInfo is the content of an EventTypeMachineInfo state event.
// Published once at daemon startup to #bureau/machine. Contains static
// system inventory that does not change between heartbeats: CPU topology,
// total memory, GPU hardware, board identity. Consumers use this to
// understand the fleet's hardware composition without requiring ssh access
// or out-of-band inventory systems.
type MachineInfo struct {
	// Principal is the full Matrix user ID of the machine
	// (e.g., "@machine/workstation:bureau.local").
	Principal string `json:"principal"`

	// Hostname is the machine's hostname from os.Hostname().
	Hostname string `json:"hostname"`

	// KernelVersion is the running kernel version string
	// (e.g., "6.14.0-37-generic"). From syscall.Uname.
	KernelVersion string `json:"kernel_version"`

	// BoardVendor is the motherboard/system vendor from
	// /sys/class/dmi/id/sys_vendor (e.g., "ASUS"). Empty in
	// environments where DMI is unavailable (VMs, containers).
	BoardVendor string `json:"board_vendor,omitempty"`

	// BoardName is the motherboard model from
	// /sys/class/dmi/id/board_name (e.g., "Pro WS WRX90E-SAGE SE").
	// Empty in environments where DMI is unavailable.
	BoardName string `json:"board_name,omitempty"`

	// CPU describes the machine's CPU topology.
	CPU CPUInfo `json:"cpu"`

	// MemoryTotalMB is the total physical RAM in megabytes.
	// From syscall.Sysinfo.
	MemoryTotalMB int `json:"memory_total_mb"`

	// SwapTotalMB is the total swap space in megabytes.
	// From syscall.Sysinfo. Zero if no swap is configured.
	SwapTotalMB int `json:"swap_total_mb,omitempty"`

	// NUMANodes is the number of NUMA nodes on this machine.
	// From counting /sys/devices/system/node/node* directories.
	// Zero if NUMA information is unavailable.
	NUMANodes int `json:"numa_nodes,omitempty"`

	// GPUs lists the GPU hardware present on this machine. Each
	// entry describes a single GPU. Empty if no GPUs are detected.
	GPUs []GPUInfo `json:"gpus,omitempty"`

	// Labels are operator-assigned key-value tags describing the machine's
	// capabilities and organizational role. The fleet controller uses labels
	// for placement constraint matching (PlacementConstraints.Requires).
	// Examples: "gpu=rtx4090", "persistent=true", "tier=production".
	// Published by the daemon from configuration; updated via
	// "bureau machine label" CLI.
	Labels map[string]string `json:"labels,omitempty"`

	// DaemonVersion is the bureau-daemon version string
	// (from lib/version at build time).
	DaemonVersion string `json:"daemon_version"`
}

// CPUInfo describes the CPU topology of a machine.
type CPUInfo struct {
	// Model is the CPU model string from /proc/cpuinfo
	// (e.g., "AMD Ryzen Threadripper PRO 7995WX 96-Cores").
	Model string `json:"model"`

	// Sockets is the number of physical CPU packages.
	// From counting unique physical_package_id values in sysfs.
	Sockets int `json:"sockets"`

	// CoresPerSocket is the number of physical cores per socket.
	// From counting unique core_id values per physical package.
	CoresPerSocket int `json:"cores_per_socket"`

	// ThreadsPerCore is the number of hardware threads per core
	// (1 for no SMT, 2 for standard SMT/HyperThreading).
	ThreadsPerCore int `json:"threads_per_core"`

	// L3CacheKB is the L3 cache size per socket in kilobytes.
	// From /sys/devices/system/cpu/cpu0/cache/index3/size.
	// Zero if L3 cache information is unavailable.
	L3CacheKB int `json:"l3_cache_kb,omitempty"`
}

// GPUInfo describes a single GPU's static hardware identity. Published
// as part of MachineInfo. The PCISlot field serves as the join key with
// GPUStatus entries in MachineStatus heartbeats.
type GPUInfo struct {
	// Vendor is the GPU vendor name: "AMD", "NVIDIA", or "Intel".
	// Mapped from PCI vendor ID (0x1002, 0x10de, 0x8086).
	Vendor string `json:"vendor"`

	// ModelName is the human-readable GPU model name (e.g., "NVIDIA
	// GeForce RTX 4090", "NVIDIA A100-SXM4-80GB"). On NVIDIA, read
	// from /proc/driver/nvidia/gpus/<slot>/information. Empty when
	// the driver does not provide a marketing name.
	ModelName string `json:"model_name,omitempty"`

	// PCIDeviceID is the PCI device ID (e.g., "0x744a" for MI300X).
	// Combined with vendor ID, this uniquely identifies the GPU model.
	PCIDeviceID string `json:"pci_device_id"`

	// PCISlot is the PCI slot address (e.g., "0000:c3:00.0"). Unique
	// per machine and stable across reboots. Used as the join key
	// between GPUInfo and GPUStatus.
	PCISlot string `json:"pci_slot"`

	// VRAMTotalBytes is the total GPU video memory in bytes
	// (e.g., 48301604864 for 48 GB). From sysfs mem_info_vram_total
	// on AMD.
	VRAMTotalBytes int64 `json:"vram_total_bytes"`

	// UniqueID is a vendor-specific hardware serial number. From
	// sysfs unique_id on AMD, GPU UUID on NVIDIA. Empty if the
	// driver does not expose a unique identifier.
	UniqueID string `json:"unique_id,omitempty"`

	// VBIOSVersion is the GPU firmware/VBIOS version string
	// (e.g., "113-APM7489-DS2-100"). From sysfs vbios_version.
	VBIOSVersion string `json:"vbios_version,omitempty"`

	// VRAMVendor is the VRAM chip manufacturer (e.g., "samsung",
	// "micron", "sk_hynix"). From sysfs mem_info_vram_vendor on AMD.
	// Empty if the driver does not expose this information.
	VRAMVendor string `json:"vram_vendor,omitempty"`

	// PCIeLinkWidth is the negotiated PCIe link width (e.g., 16 for
	// x16). From sysfs current_link_width. Zero if unavailable.
	PCIeLinkWidth int `json:"pcie_link_width,omitempty"`

	// ThermalLimitCriticalMillidegrees is the critical temperature
	// threshold in millidegrees Celsius. The GPU throttles at this
	// temperature. From hwmon temp*_crit where the label is "edge".
	// Zero if unavailable.
	ThermalLimitCriticalMillidegrees int `json:"thermal_limit_critical_millidegrees,omitempty"`

	// ThermalLimitEmergencyMillidegrees is the emergency shutdown
	// temperature in millidegrees Celsius. The GPU powers off at
	// this temperature. From hwmon temp*_emergency. Zero if
	// unavailable.
	ThermalLimitEmergencyMillidegrees int `json:"thermal_limit_emergency_millidegrees,omitempty"`

	// Driver is the kernel driver name (e.g., "amdgpu", "nvidia",
	// "nouveau", "i915", "xe"). From the driver symlink in sysfs.
	Driver string `json:"driver"`
}

// MachineConfig is the content of an EventTypeMachineConfig state event.
// Defines which principals should run on a machine. The daemon reads this
// at startup and on change to reconcile running sandboxes.
type MachineConfig struct {
	// Principals is the list of principals assigned to this machine.
	Principals []PrincipalAssignment `json:"principals"`

	// DefaultPolicy is the machine-wide baseline authorization policy.
	// Its grants and allowances apply to every principal on this
	// machine unless overridden. Per-principal policy is additive on
	// top of machine defaults — it adds grants and allowances, it
	// does not remove them. A principal cannot have fewer permissions
	// than the machine default.
	DefaultPolicy *AuthorizationPolicy `json:"default_policy,omitempty"`

	// BureauVersion specifies the desired versions of Bureau core
	// binaries (daemon, launcher, proxy) on this machine. When the
	// daemon detects a change (via binary content hash comparison, not
	// store path), it orchestrates the update: prefetching from attic,
	// exec()'ing itself for daemon updates, signaling the launcher for
	// launcher updates, and passing the new proxy path to the launcher
	// for future sandbox creation.
	//
	// When nil, no version management is performed — the machine runs
	// whatever binaries were installed at bootstrap time.
	BureauVersion *BureauVersion `json:"bureau_version,omitempty"`
}

// BureauVersion identifies the desired versions of Bureau core binaries
// on a machine. The daemon compares binary content hashes (SHA256 of the
// actual file, not the Nix store path) against the currently running
// versions to determine what needs restarting. This avoids unnecessary
// restarts when an unrelated dependency change produces a new store path
// but a byte-identical binary.
//
// Only the three persistent/recurring process types are tracked here:
// daemon (runs continuously), launcher (runs continuously), and proxy
// (spawned per-sandbox by the launcher). Other Bureau binaries (bridge,
// sandbox, credentials, proxy-call, observe-relay) are short-lived
// utilities resolved from PATH or the Nix environment at invocation time.
type BureauVersion struct {
	// DaemonStorePath is the Nix store path containing the bureau-daemon
	// binary (e.g., "/nix/store/abc123-bureau-daemon/bin/bureau-daemon").
	// The daemon prefetches this path from attic, hashes the binary,
	// compares against its own running binary, and exec()'s the new
	// version if the hashes differ.
	DaemonStorePath string `json:"daemon_store_path"`

	// LauncherStorePath is the Nix store path containing the
	// bureau-launcher binary. The daemon prefetches this path, hashes
	// the binary, compares against the launcher's reported hash (via
	// the "status" IPC action), and signals the launcher to exec() the
	// new version if the hashes differ.
	LauncherStorePath string `json:"launcher_store_path"`

	// ProxyStorePath is the Nix store path containing the bureau-proxy
	// binary. The launcher uses this path when spawning new proxy
	// processes for sandbox creation. Existing proxies continue running
	// their current binary until their sandbox is recycled.
	ProxyStorePath string `json:"proxy_store_path"`
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
	//   - "bureau/template:base" — built-in base template
	//   - "bureau/template:llm-agent" — built-in agent template
	//   - "iree/template:amdgpu-developer" — project-specific template
	//
	// For federated deployments, an optional @server suffix on the room
	// reference specifies the homeserver:
	//
	//   "iree/template@other.example:amdgpu-developer"
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
	// Labels do not affect access control — that's handled by the
	// authorization policy and localpart glob patterns. Changing labels does not
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

	// ServiceVisibility is a list of glob patterns that control which
	// services this principal can discover via GET /v1/services. Patterns
	// match against service localparts using Bureau's hierarchical glob
	// syntax:
	//   - "service/stt/*" — all STT services
	//   - "service/**" — all services under service/
	//   - "**" — all services (use with caution)
	//
	// An empty or nil list means the principal cannot see any services
	// (default-deny). The daemon pushes these patterns to the principal's
	// proxy, which filters the directory before returning results.
	ServiceVisibility []string `json:"service_visibility,omitempty"`

	// Authorization is the full authorization policy for this principal.
	// Defines what this principal can do (grants, denials) and what
	// others can do to it (allowances, allowance denials).
	//
	// This is the advanced alternative to the shorthand fields above
	// (MatrixPolicy, ServiceVisibility). When set, the daemon uses
	// it for all authorization decisions and ignores the shorthand
	// fields. When absent, the daemon synthesizes grants from the
	// shorthand fields.
	Authorization *AuthorizationPolicy `json:"authorization,omitempty"`

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
	// principals gate on EventTypeWorkspace (with ContentMatch
	// {"status": "active"}) so they don't start until the setup
	// principal has finished creating the workspace. Other uses
	// include service dependencies, staged rollouts, and manual
	// approval gates.
	//
	// When nil, the principal starts normally (subject to AutoStart).
	StartCondition *StartCondition `json:"start_condition,omitempty"`
}

// StartCondition specifies a state event that must exist (and optionally
// match specific content) before a principal can launch. The daemon checks
// this during reconciliation: resolve the room alias, fetch the state event,
// and proceed only if the event exists and all ContentMatch entries are
// satisfied.
type StartCondition struct {
	// EventType is the Matrix state event type to check for (e.g.,
	// "m.bureau.workspace"). Must be a valid Matrix event type.
	EventType string `json:"event_type"`

	// StateKey is the state key to look up. Empty string is valid and
	// common — it means the singleton instance of that event type.
	StateKey string `json:"state_key"`

	// RoomAlias is the full Matrix room alias where the event should
	// exist (e.g., "#iree/amdgpu/inference:bureau.local"). The daemon
	// resolves this to a room ID (cached) before fetching the event.
	// When empty, the daemon checks the principal's own config room.
	RoomAlias string `json:"room_alias,omitempty"`

	// ContentMatch specifies criteria that must all be satisfied in the
	// event content for the condition to pass. When nil or empty, only
	// event existence is checked.
	//
	// Each key names a field in the event content. Each value is either
	// a bare scalar (shorthand for equality) or a $-prefixed operator
	// object supporting comparisons and set membership. Multiple
	// operators on the same field AND together (enabling ranges).
	//
	// Bare values (backward compatible):
	//   {"status": "active"}            — string equality
	//   {"priority": 2}                 — numeric equality
	//
	// Operators:
	//   {"priority": {"$lte": 2}}       — comparison
	//   {"priority": {"$gte": 1, "$lte": 3}} — range
	//   {"hour": {"$in": [9, 10, 11]}}  — set membership
	//
	// For array targets, all operators evaluate per-element: a match
	// succeeds if any element in the array satisfies the criterion.
	ContentMatch ContentMatch `json:"content_match,omitempty"`
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
// Templates can inherit from multiple parents via the Inherits field. The
// daemon resolves each parent independently and merges them left-to-right:
// later parents override earlier parents for conflicting scalars and map
// keys, slices are concatenated, and the child template is applied last
// (overrides everything). Slices (Filesystem, CreateDirs) are appended;
// maps (EnvironmentVariables, Roles, DefaultPayload) are merged with child
// values winning; scalars (Command, Environment) are replaced if non-zero
// in the child.
type TemplateContent struct {
	// Description is a human-readable summary of what this template
	// provides (e.g., "GPU-accelerated LLM agent with IREE runtime").
	Description string `json:"description,omitempty"`

	// Inherits is an ordered list of parent template references. The
	// daemon resolves each parent independently and merges left-to-right:
	// later parents override earlier parents for conflicting scalar
	// fields and map keys. Slices are concatenated. The child template
	// is applied last (overrides everything).
	//
	// Single-parent inheritance is a one-element list. Empty means no
	// inheritance (self-contained template). Format for each entry is
	// the same as PrincipalAssignment.Template:
	// "room-alias-localpart:template-name". Cycles are detected and
	// rejected.
	Inherits []string `json:"inherits,omitempty"`

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
	// creation time. Available variables include ${WORKSPACE_ROOT},
	// ${PROJECT}, ${WORKTREE_PATH}, ${CACHE_ROOT}, ${PROXY_SOCKET},
	// ${TERM}, ${MACHINE_NAME}, and ${SERVER_NAME}. Workspace variables
	// (PROJECT, WORKTREE_PATH) are populated from the principal's payload.
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

	// RequiredServices lists service roles (e.g., "ticket", "rag")
	// that must be available when a sandbox is created from this
	// template. The daemon resolves each role to a concrete service
	// principal by reading m.bureau.room_service state events in the
	// principal's rooms, then bind-mounts the service's Unix socket
	// into the sandbox at /run/bureau/service/<role>.sock. If any
	// required service cannot be resolved, sandbox creation fails.
	//
	// During template inheritance, child RequiredServices are appended
	// to parent RequiredServices (duplicates removed).
	RequiredServices []string `json:"required_services,omitempty"`

	// DefaultPayload is the default agent payload merged under
	// PrincipalAssignment.Payload at resolution time (instance values
	// win on conflict). Written to /run/bureau/payload.json inside the
	// sandbox. Use this for template-level defaults that individual
	// instances can override.
	DefaultPayload map[string]any `json:"default_payload,omitempty"`

	// HealthCheck configures automated health monitoring for principals
	// created from this template. When set, the daemon polls the
	// configured HTTP endpoint through the proxy admin socket and
	// triggers rollback on sustained failure. During template
	// inheritance, child HealthCheck replaces parent HealthCheck
	// entirely (no field-level merge — the parameters form a coherent
	// unit). When nil, no health monitoring is performed.
	HealthCheck *HealthCheck `json:"health_check,omitempty"`

	// ProxyServices declares external HTTP API upstreams that the proxy
	// should be configured to forward. The daemon registers these on
	// the principal's proxy after sandbox creation, enabling credential
	// injection without exposing API keys to the sandboxed agent.
	//
	// During template inheritance, ProxyServices maps are merged with
	// child values winning on conflict (same semantics as
	// EnvironmentVariables and Roles).
	//
	// Example:
	//
	//   "proxy_services": {
	//     "anthropic": {
	//       "upstream": "https://api.anthropic.com",
	//       "inject_headers": {"x-api-key": "ANTHROPIC_API_KEY"},
	//       "strip_headers": ["x-api-key", "authorization"]
	//     }
	//   }
	//
	// Inside the sandbox, agents reach the service at
	// /http/<service-name>/... on the proxy socket (directly via Unix
	// socket clients, or via the bridge's TCP endpoint for HTTP clients
	// that require a URL).
	ProxyServices map[string]TemplateProxyService `json:"proxy_services,omitempty"`
}

// TemplateProxyService declares an external HTTP API upstream that the
// proxy should forward requests to with credential injection. The daemon
// registers each service on the principal's proxy via the admin API
// after sandbox creation.
type TemplateProxyService struct {
	// Upstream is the base URL of the external API
	// (e.g., "https://api.anthropic.com"). Required.
	Upstream string `json:"upstream"`

	// InjectHeaders maps HTTP header names to credential names. When
	// forwarding a request, the proxy reads each credential from the
	// principal's credential bundle and sets it as the specified header.
	// Example: {"x-api-key": "ANTHROPIC_API_KEY"} injects the
	// ANTHROPIC_API_KEY credential as the x-api-key header.
	InjectHeaders map[string]string `json:"inject_headers,omitempty"`

	// StripHeaders lists HTTP headers to remove from incoming requests
	// before forwarding. Use this to prevent agents from injecting
	// their own authentication headers.
	StripHeaders []string `json:"strip_headers,omitempty"`
}

// HealthCheck configures automated health monitoring for principals
// created from a template. The daemon polls the health endpoint through
// the principal's proxy admin socket. If consecutive failures exceed the
// threshold, the daemon triggers a rollback to the previously working
// sandbox configuration.
//
// Health checks are optional. Templates for services (databases, API
// servers, webhook relays) should define health checks; templates for
// interactive agents typically should not.
type HealthCheck struct {
	// Endpoint is the HTTP path to poll (e.g., "/health", "/healthz",
	// "/api/v1/status"). The daemon sends HTTP GET requests to this
	// path through the proxy admin socket.
	Endpoint string `json:"endpoint"`

	// IntervalSeconds is the polling interval in seconds. The daemon
	// sends one health check request per interval. Must be positive.
	IntervalSeconds int `json:"interval_seconds"`

	// TimeoutSeconds is the per-request timeout in seconds. A request
	// that does not receive HTTP 200 within this duration counts as a
	// failure. Zero defaults to 5 seconds at runtime.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`

	// FailureThreshold is the number of consecutive failures before
	// the daemon considers the principal unhealthy and triggers a
	// rollback to the previous working sandbox configuration. Zero
	// defaults to 3 at runtime.
	FailureThreshold int `json:"failure_threshold,omitempty"`

	// GracePeriodSeconds is the delay after sandbox creation before
	// the first health check. Gives the service time to initialize
	// (load models, warm caches, bind ports). Zero defaults to 30
	// seconds at runtime.
	GracePeriodSeconds int `json:"grace_period_seconds,omitempty"`
}

// TemplateMount describes a filesystem mount point in a template. This is
// the JSON wire-format counterpart to sandbox.Mount (which uses YAML tags).
// The daemon converts between the two during template resolution.
type TemplateMount struct {
	// Source is the host path to mount. Supports ${VARIABLE} expansion
	// (e.g., "${WORKSPACE_ROOT}/${PROJECT}" for a project workspace directory).
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

// WorkspaceState is the content of an EventTypeWorkspace state event.
// It tracks the full lifecycle of a workspace as a status field that
// progresses one-directionally:
//
//	pending → active → teardown → archived | removed
//
// The teardown status triggers continuous enforcement: agent principals
// gated on "active" stop, and the teardown principal gated on "teardown"
// starts. The teardown principal performs cleanup (archive or delete)
// and publishes the final status ("archived" or "removed").
type WorkspaceState struct {
	// Status is the current lifecycle state. Valid values:
	//   - "pending": room created, setup not yet started or in progress.
	//   - "active": setup complete, workspace is usable.
	//   - "teardown": destroy requested, agents stopping, teardown running.
	//   - "archived": teardown completed in archive mode.
	//   - "removed": teardown completed in delete mode.
	Status string `json:"status"`

	// Project is the project name (first path segment of the workspace
	// alias). Matches the directory under /var/bureau/workspace/.
	Project string `json:"project"`

	// Machine is the machine localpart identifying which host the
	// workspace data lives on.
	Machine string `json:"machine"`

	// WorkspacePath is the absolute filesystem path of the workspace
	// on the host machine (e.g., "/var/bureau/workspace/iree"). Present
	// when the workspace is "active" — the setup pipeline sets it after
	// creating the project directory. Empty for "pending" (workspace not
	// yet created), "teardown", "archived", and "removed" (workspace no
	// longer exists at this path).
	WorkspacePath string `json:"workspace_path,omitempty"`

	// TeardownMode specifies how the teardown principal should handle
	// the workspace data. Set by "bureau workspace destroy --mode".
	// Valid values: "archive" (move to .archive/), "delete" (remove).
	// Empty for all statuses other than "teardown".
	TeardownMode string `json:"teardown_mode,omitempty"`

	// UpdatedAt is an ISO 8601 timestamp of the last status transition.
	UpdatedAt string `json:"updated_at"`

	// ArchivePath is the path under /workspace/.archive/ where the
	// project was moved when status is "archived". Empty for all other
	// statuses.
	ArchivePath string `json:"archive_path,omitempty"`
}

// WorktreeState is the content of an EventTypeWorktree state event.
// It tracks the lifecycle of an individual git worktree within a workspace.
// Each worktree has its own state event in the workspace room, keyed by
// the worktree path relative to the project root.
//
// The lifecycle parallels WorkspaceState but is simpler: worktrees don't
// have a "pending" state (the daemon publishes "creating" immediately
// when it accepts the add request) and don't have a "teardown" stage
// (removal is a single pipeline, not a multi-phase process).
type WorktreeState struct {
	// Status is the current lifecycle state. Valid values:
	//   - "creating": daemon accepted the add request, init pipeline running.
	//   - "active": init pipeline completed, worktree is ready for use.
	//   - "removing": daemon accepted the remove request, deinit pipeline running.
	//   - "archived": deinit completed in archive mode.
	//   - "removed": deinit completed in delete mode.
	//   - "failed": init or deinit pipeline failed.
	Status string `json:"status"`

	// Project is the project name (first path segment of the workspace alias).
	Project string `json:"project"`

	// WorktreePath is the worktree path relative to the project root
	// (e.g., "feature/amdgpu", "main"). Matches the state key of the event.
	WorktreePath string `json:"worktree_path"`

	// Branch is the git branch checked out in this worktree. Empty when
	// the worktree was created in detached HEAD mode.
	Branch string `json:"branch,omitempty"`

	// Machine is the machine localpart identifying which host the
	// worktree lives on.
	Machine string `json:"machine"`

	// UpdatedAt is an ISO 8601 timestamp of the last status transition.
	UpdatedAt string `json:"updated_at"`
}

// PipelineContent is the content of an EventTypePipeline state event.
// It defines a reusable automation sequence: a list of steps executed
// in order by the pipeline executor inside a sandbox. Steps can run
// shell commands, publish Matrix state events, or launch interactive
// sessions.
//
// Pipelines are the automation primitive for Bureau operations:
// workspace setup, service lifecycle, maintenance, deployment, and
// any structured task that benefits from observability, idempotency,
// and Matrix-native logging.
//
// Variable substitution (${NAME}) is applied to all string fields in
// steps before execution. Variables are resolved from step-level env,
// pipeline payload, Bureau runtime variables, and process environment.
type PipelineContent struct {
	// Description is a human-readable summary of what this pipeline
	// does (e.g., "Clone repository and prepare project workspace").
	Description string `json:"description,omitempty"`

	// Variables declares the variables this pipeline expects, with
	// optional defaults and required flags. The executor validates
	// required variables before starting execution. This is the
	// declaration — actual values come from payload, environment,
	// and step-level overrides at runtime.
	Variables map[string]PipelineVariable `json:"variables,omitempty"`

	// Steps is the ordered list of steps to execute. At least one
	// step is required. Steps run sequentially; the executor does
	// not support parallel steps (use shell backgrounding if needed).
	Steps []PipelineStep `json:"steps"`

	// OnFailure is a list of steps to execute when a non-optional step
	// fails. Typically used to publish a failure state event so that
	// observers can detect the failure and the resource doesn't get
	// stuck in a transitional state (e.g., "creating" forever).
	//
	// On_failure steps are inherently best-effort: if an on_failure
	// step itself fails, the failure is logged and the executor
	// continues with remaining on_failure steps. The original error
	// is preserved.
	//
	// On_failure steps do NOT run when a pipeline is aborted (an
	// assert_state step with on_mismatch "abort" triggers a clean
	// exit, not a failure).
	//
	// The variables FAILED_STEP (name of the step that failed) and
	// FAILED_ERROR (error message) are injected into the variable
	// context for on_failure steps.
	OnFailure []PipelineStep `json:"on_failure,omitempty"`

	// Outputs declares the pipeline's return values — which step outputs
	// are promoted to the pipeline result. Each entry maps an output name
	// to a PipelineOutput with a description and a value expression that
	// references step outputs via ${OUTPUT_<step>_<name>} variables.
	//
	// Pipeline outputs are resolved after all steps succeed. On failure
	// or abort, no pipeline-level outputs are produced.
	Outputs map[string]PipelineOutput `json:"outputs,omitempty"`

	// Log configures Matrix thread logging for pipeline executions.
	// When set, the executor creates a thread in the specified room
	// at pipeline start and posts step progress as thread replies.
	// When nil, the executor logs only to stdout (visible via
	// bureau observe).
	Log *PipelineLog `json:"log,omitempty"`
}

// PipelineVariable declares an expected variable for a pipeline.
// Variables are informational for documentation and validation —
// the executor resolves actual values from payload, environment,
// and step-level overrides.
type PipelineVariable struct {
	// Description explains what this variable is for (shown by
	// bureau pipeline show).
	Description string `json:"description,omitempty"`

	// Default is the fallback value when the variable is not
	// provided in any source. Empty string is a valid default.
	Default string `json:"default,omitempty"`

	// Required means the executor must fail if this variable has
	// no value from any source (including Default). A variable
	// with both Required and Default set uses the default only
	// when no explicit value is provided.
	Required bool `json:"required,omitempty"`
}

// PipelineOutput declares a pipeline-level output value. Pipeline outputs
// are the externally visible return values — they appear in
// PipelineResultContent and CommandResultMessage so initiators and
// observers can consume structured results.
//
// The Value field is a variable expression (e.g.,
// "${OUTPUT_clone_repository_head_sha}") that references a step output.
// It is expanded after all steps succeed, using the same variable
// substitution as step fields.
type PipelineOutput struct {
	// Description is a human-readable explanation of what this output
	// represents (e.g., "HEAD commit SHA of the cloned repository").
	// Used in bureau pipeline show and in result events for
	// observability.
	Description string `json:"description,omitempty"`

	// Value is a variable expression that resolves to the output value.
	// Typically references a step output: "${OUTPUT_clone_repo_head_sha}".
	// Expanded after all steps succeed.
	Value string `json:"value"`
}

// PipelineStep is a single step in a pipeline. Exactly one of Run,
// Publish, or AssertState must be set:
//   - Run: execute a shell command
//   - Publish: publish a Matrix state event
//   - AssertState: read a Matrix state event and check a condition
type PipelineStep struct {
	// Name is a human-readable identifier for this step, used in
	// log output and status messages (e.g., "clone-repository",
	// "publish-ready"). Required.
	Name string `json:"name"`

	// Run is a shell command executed via /bin/sh -c. Multi-line
	// strings are supported. Variable substitution (${NAME}) is
	// applied before execution. Mutually exclusive with Publish.
	Run string `json:"run,omitempty"`

	// Check is a post-step health check command. Runs after Run
	// succeeds; if Check exits non-zero, the step is treated as
	// failed. Catches cases where a command "succeeds" but
	// doesn't produce the expected result. Only valid with Run.
	Check string `json:"check,omitempty"`

	// When is a guard condition command. Runs before Run; if it
	// exits non-zero, the step is skipped (not failed). Use for
	// conditional steps: when: "test -n '${REPOSITORY}'" skips
	// clone for non-git workspaces.
	When string `json:"when,omitempty"`

	// Optional means step failure doesn't abort the pipeline.
	// The failure is logged but execution continues. Use for
	// best-effort steps like project-specific init scripts that
	// may not exist.
	Optional bool `json:"optional,omitempty"`

	// Publish sends a Matrix state event instead of running a
	// shell command. The executor connects to the proxy Unix
	// socket directly. Mutually exclusive with Run and AssertState.
	Publish *PipelinePublish `json:"publish,omitempty"`

	// AssertState reads a Matrix state event and checks a field
	// against an expected value. Used for precondition checks
	// (e.g., verifying a workspace is still in "teardown" before
	// proceeding with destructive operations) and advisory CAS
	// (e.g., verifying no one else is already removing a worktree).
	//
	// Mutually exclusive with Run and Publish.
	AssertState *PipelineAssertState `json:"assert_state,omitempty"`

	// Timeout is the maximum duration for this step (e.g., "5m",
	// "30s", "1h"). Parsed by time.ParseDuration. The executor
	// kills the step if it exceeds this duration. When empty,
	// defaults to 5 minutes at runtime.
	Timeout string `json:"timeout,omitempty"`

	// GracePeriod is the duration between SIGTERM and SIGKILL when
	// a step's timeout expires. When set, the executor sends SIGTERM
	// to the process group first, waits up to this duration for the
	// process to exit gracefully, then escalates to SIGKILL. When
	// empty, the executor sends SIGKILL immediately on timeout.
	//
	// Use this for steps that perform irreversible operations
	// (database writes, external API calls with side effects) where
	// abrupt termination could leave state inconsistent. Most sandbox
	// steps should use the default (immediate SIGKILL) since sandbox
	// processes are ephemeral and hold no durable state.
	//
	// Parsed by time.ParseDuration. Only valid on run steps.
	GracePeriod string `json:"grace_period,omitempty"`

	// Env sets additional environment variables for this step
	// only. Merged with pipeline-level variables; step values
	// take precedence on conflict.
	Env map[string]string `json:"env,omitempty"`

	// Outputs declares files to capture as named output values after
	// the step's run command (and check, if present) succeeds. Each
	// map entry is either a string (file path for inline capture) or
	// a JSON object (PipelineStepOutput with artifact mode and
	// metadata). Only valid on run steps.
	//
	// Inline outputs read the file and store its content as a string
	// value (64 KB limit, trailing whitespace trimmed). Artifact
	// outputs stream the file to the artifact service and store the
	// returned art-* reference as the value.
	//
	// Output values are injected as variables for subsequent steps:
	// OUTPUT_<step_name>_<output_name> (dashes in step names become
	// underscores).
	//
	// The value type is json.RawMessage to support both string and
	// object forms. Use ParseStepOutputs to resolve into typed
	// PipelineStepOutput structs.
	Outputs map[string]json.RawMessage `json:"outputs,omitempty"`

	// Interactive means this step expects terminal interaction.
	// The executor allocates a PTY and does not capture stdout.
	// The operator interacts via bureau observe (readwrite mode).
	// Only valid with Run.
	Interactive bool `json:"interactive,omitempty"`
}

// PipelineStepOutput declares how to capture a single output value
// from a file produced by a run step.
type PipelineStepOutput struct {
	// Path is the filesystem path to read after the step succeeds.
	// Supports ${VARIABLE} substitution (expanded before execution,
	// same as all other step string fields).
	Path string `json:"path"`

	// Artifact means the file should be stored in the artifact
	// service rather than read inline. The output value becomes the
	// art-* content-addressed reference returned by the artifact
	// service. Use this for large or binary outputs (model weights,
	// compiled binaries, log files) that should be durably stored.
	// When false (default), the file is read as an inline string
	// (64 KB limit, trailing whitespace trimmed).
	Artifact bool `json:"artifact,omitempty"`

	// ContentType is the MIME type hint for artifact storage (e.g.,
	// "text/plain", "application/gzip"). Only meaningful when
	// Artifact is true. When empty, the artifact service
	// auto-detects based on content.
	ContentType string `json:"content_type,omitempty"`

	// Description is a human-readable explanation of what this
	// output represents (e.g., "HEAD commit SHA after clone").
	// Used in bureau pipeline show and in result events for
	// observability.
	Description string `json:"description,omitempty"`
}

// ParseStepOutputs parses the raw output declarations from a PipelineStep
// into typed PipelineStepOutput structs. Each entry in the map is either:
//   - A JSON string: interpreted as an inline file path →
//     PipelineStepOutput{Path: <string>}
//   - A JSON object: unmarshaled directly into PipelineStepOutput
//
// Returns an error if any entry is neither a string nor a valid object.
func ParseStepOutputs(raw map[string]json.RawMessage) (map[string]PipelineStepOutput, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	result := make(map[string]PipelineStepOutput, len(raw))
	for name, rawValue := range raw {
		parsed, err := parseOneStepOutput(name, rawValue)
		if err != nil {
			return nil, err
		}
		result[name] = parsed
	}
	return result, nil
}

// parseOneStepOutput parses a single output declaration from its raw
// JSON representation (string or object form).
func parseOneStepOutput(name string, raw json.RawMessage) (PipelineStepOutput, error) {
	// Try string form first (most common).
	var path string
	if err := json.Unmarshal(raw, &path); err == nil {
		return PipelineStepOutput{Path: path}, nil
	}

	// Try object form.
	var output PipelineStepOutput
	if err := json.Unmarshal(raw, &output); err != nil {
		return PipelineStepOutput{}, fmt.Errorf("output %q: must be a string (file path) or object (PipelineStepOutput), got: %s", name, string(raw))
	}
	return output, nil
}

// PipelinePublish describes a Matrix state event to publish as a
// pipeline step. The executor connects to the proxy Unix socket
// and PUTs the event directly (same mechanism as bureau-proxy-call).
// All string fields support variable substitution (${NAME}).
type PipelinePublish struct {
	// EventType is the Matrix state event type (e.g.,
	// "m.bureau.workspace").
	EventType string `json:"event_type"`

	// Room is the target room alias or ID. Supports variable
	// substitution (e.g., "${WORKSPACE_ROOM_ID}").
	Room string `json:"room"`

	// StateKey is the state key for the event. Empty string is
	// valid (singleton events like m.bureau.workspace).
	StateKey string `json:"state_key,omitempty"`

	// Content is the event content as a JSON-compatible map.
	// String values support variable substitution.
	Content map[string]any `json:"content"`
}

// PipelineAssertState describes a state event assertion — a precondition
// check that reads a Matrix state event and verifies a field matches an
// expected value. Used in pipeline steps to guard against stale state
// (e.g., a deinit pipeline verifying the resource is still in "removing"
// status before proceeding with destructive operations).
//
// Exactly one condition field must be set: Equals, NotEquals, In, or NotIn.
//
// All string fields support variable substitution (${NAME}).
type PipelineAssertState struct {
	// Room is the Matrix room alias or ID containing the state event.
	Room string `json:"room"`

	// EventType is the Matrix state event type to read (e.g.,
	// "m.bureau.worktree", "m.bureau.workspace").
	EventType string `json:"event_type"`

	// StateKey is the state key for the event. Empty string is valid
	// (for singleton events like m.bureau.workspace).
	StateKey string `json:"state_key,omitempty"`

	// Field is the top-level JSON field name to extract from the event
	// content (e.g., "status"). The extracted value is stringified for
	// comparison.
	Field string `json:"field"`

	// Equals asserts the field value equals this string exactly.
	Equals string `json:"equals,omitempty"`

	// NotEquals asserts the field value does not equal this string.
	NotEquals string `json:"not_equals,omitempty"`

	// In asserts the field value is one of the listed strings.
	In []string `json:"in,omitempty"`

	// NotIn asserts the field value is not any of the listed strings.
	NotIn []string `json:"not_in,omitempty"`

	// OnMismatch controls behavior when the assertion fails:
	//   - "fail" (default): the step fails, the pipeline fails, and
	//     on_failure steps run. Use for error conditions.
	//   - "abort": the pipeline exits cleanly with exit code 0 and
	//     on_failure steps do NOT run. Use for benign precondition
	//     mismatches (e.g., "someone else is already handling this").
	OnMismatch string `json:"on_mismatch,omitempty"`

	// Message is a human-readable explanation logged when the assertion
	// fails (e.g., "workspace status is no longer 'teardown'").
	Message string `json:"message,omitempty"`
}

// PipelineLog configures Matrix thread logging for a pipeline.
// When present on a PipelineContent, the executor creates a thread
// in the specified room and logs step progress as thread replies.
type PipelineLog struct {
	// Room is the Matrix room alias or ID where execution threads
	// are created. Supports variable substitution (e.g.,
	// "${WORKSPACE_ROOM_ID}"). The executor resolves aliases via
	// the proxy.
	Room string `json:"room"`
}

// PipelineResultContent is the content of an EventTypePipelineResult state
// event. Published by the pipeline executor when execution finishes. This
// is the Matrix-native public format; the JSONL result log
// (BUREAU_RESULT_PATH) is a separate internal format for daemon tailing.
//
// The ticket service evaluates pipeline gates against these events:
// TicketGate.PipelineRef matches PipelineRef, TicketGate.Conclusion
// matches Conclusion. Gate evaluation happens via /sync — when the
// ticket service sees a PipelineResult state event change, it re-checks
// all pending pipeline gates in that room.
type PipelineResultContent struct {
	// Version is the schema version (see PipelineResultContentVersion).
	// Same semantics as TicketContent.Version — call CanModify() before
	// any read-modify-write cycle.
	Version int `json:"version"`

	// PipelineRef identifies which pipeline was executed (e.g.,
	// "dev-workspace-init"). This is the name/ref used when the
	// pipeline was resolved. TicketGate.PipelineRef matches against
	// this field.
	PipelineRef string `json:"pipeline_ref"`

	// Conclusion is the terminal outcome: "success", "failure", or
	// "aborted". TicketGate.Conclusion matches against this field.
	// An empty Conclusion in a gate means "any completed result".
	Conclusion string `json:"conclusion"`

	// StartedAt is an ISO 8601 timestamp of when execution began.
	StartedAt string `json:"started_at"`

	// CompletedAt is an ISO 8601 timestamp of when execution finished.
	CompletedAt string `json:"completed_at"`

	// DurationMS is the total execution wall-clock time in milliseconds.
	DurationMS int64 `json:"duration_ms"`

	// StepCount is the total number of steps in the pipeline definition.
	StepCount int `json:"step_count"`

	// StepResults records the outcome of each step that executed. Steps
	// that were never reached (due to earlier failure or abort) are not
	// included. Ordered by execution order.
	StepResults []PipelineStepResult `json:"step_results,omitempty"`

	// FailedStep is the name of the step that caused a failure. Empty
	// when Conclusion is "success".
	FailedStep string `json:"failed_step,omitempty"`

	// ErrorMessage is the error text from the failed or aborted step.
	// Empty when Conclusion is "success".
	ErrorMessage string `json:"error_message,omitempty"`

	// LogEventID is the Matrix event ID of the thread root message in
	// the log room. Provides a direct link from the result summary to
	// the detailed step-by-step execution log. Empty when the pipeline
	// has no log room configured (which should not happen since the
	// result event itself is published to the log room).
	LogEventID string `json:"log_event_id,omitempty"`

	// Outputs contains the pipeline's resolved output values. Each
	// entry maps an output name to its string value (either inline
	// file content or an art-* artifact reference). Only populated
	// when Conclusion is "success" — failed and aborted pipelines
	// produce no outputs.
	Outputs map[string]string `json:"outputs,omitempty"`

	// Extra is a documented extension namespace for experimental or
	// preview fields before promotion to top-level schema fields in
	// a version bump. Same semantics as TicketContent.Extra.
	Extra map[string]json.RawMessage `json:"extra,omitempty"`
}

// PipelineStepResult records the outcome of a single pipeline step.
type PipelineStepResult struct {
	// Name is the step's human-readable identifier from the pipeline
	// definition.
	Name string `json:"name"`

	// Status is the step outcome: "ok", "failed", "skipped", or
	// "aborted". "failed (optional)" is recorded when an optional
	// step fails but execution continues.
	Status string `json:"status"`

	// DurationMS is the step execution wall-clock time in milliseconds.
	DurationMS int64 `json:"duration_ms"`

	// Error is the error message when the step failed or aborted.
	// Empty for successful or skipped steps.
	Error string `json:"error,omitempty"`

	// Outputs contains the captured output values for this step.
	// Each entry maps an output name to its string value (inline
	// file content or art-* artifact reference). Only populated
	// for steps with status "ok" that declared outputs.
	Outputs map[string]string `json:"outputs,omitempty"`
}

// Validate checks that all required fields are present and well-formed.
// Returns an error describing the first invalid field found, or nil if
// the content is valid.
func (p *PipelineResultContent) Validate() error {
	if p.Version < 1 {
		return fmt.Errorf("pipeline result: version must be >= 1, got %d", p.Version)
	}
	if p.PipelineRef == "" {
		return errors.New("pipeline result: pipeline_ref is required")
	}
	switch p.Conclusion {
	case "success", "failure", "aborted":
		// Valid.
	case "":
		return errors.New("pipeline result: conclusion is required")
	default:
		return fmt.Errorf("pipeline result: unknown conclusion %q", p.Conclusion)
	}
	if p.StartedAt == "" {
		return errors.New("pipeline result: started_at is required")
	}
	if p.CompletedAt == "" {
		return errors.New("pipeline result: completed_at is required")
	}
	if p.StepCount < 1 {
		return fmt.Errorf("pipeline result: step_count must be >= 1, got %d", p.StepCount)
	}
	for i := range p.StepResults {
		if err := p.StepResults[i].Validate(); err != nil {
			return fmt.Errorf("pipeline result: step_results[%d]: %w", i, err)
		}
	}
	return nil
}

// CanModify checks whether this code version can safely perform a
// read-modify-write cycle on this event. Returns nil if safe, or an
// error explaining why modification would risk data loss.
//
// Pipeline result events are typically write-once (each execution
// overwrites the previous result), but CanModify is provided for
// consistency with the schema pattern and to protect against future
// use cases where results might be annotated.
func (p *PipelineResultContent) CanModify() error {
	if p.Version > PipelineResultContentVersion {
		return fmt.Errorf(
			"pipeline result version %d exceeds supported version %d: "+
				"modification would lose fields added in newer versions; "+
				"upgrade the pipeline executor before modifying this event",
			p.Version, PipelineResultContentVersion,
		)
	}
	return nil
}

// Validate checks that the step result has valid required fields.
func (s *PipelineStepResult) Validate() error {
	if s.Name == "" {
		return errors.New("step result: name is required")
	}
	switch s.Status {
	case "ok", "failed", "failed (optional)", "skipped", "aborted":
		// Valid.
	case "":
		return errors.New("step result: status is required")
	default:
		return fmt.Errorf("step result: unknown status %q", s.Status)
	}
	return nil
}

// Credentials is the content of an EventTypeCredentials state event.
// Contains an age-encrypted credential bundle for a specific principal
// on a specific machine. See CREDENTIALS.md for the full lifecycle.
type Credentials struct {
	// Version is the schema version (see CredentialsVersion).
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
// Published to #bureau/service when a principal provides a service
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

// RoomServiceContent is the content of an EventTypeRoomService state event.
// It binds a service role to a specific service principal in a room. The
// daemon reads these when resolving a template's RequiredServices to
// determine which service sockets to bind-mount into a sandbox.
//
// State key: the service role name (e.g., "ticket", "rag", "ci")
type RoomServiceContent struct {
	// Principal is the full Matrix user ID of the service instance
	// that handles this role in this room (e.g.,
	// "@service/ticket/iree:bureau.local").
	Principal string `json:"principal"`
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

// AdminProtectedEvents returns the set of Matrix room metadata event types
// that Bureau restricts to admin power level (100) in all rooms. Every
// power level function uses this as its base events map, adding
// Bureau-specific overrides on top. Returns a fresh map each call so
// callers can safely merge their own entries.
func AdminProtectedEvents() map[string]any {
	return map[string]any{
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
func WorkspaceRoomPowerLevels(adminUserID, machineUserID string) map[string]any {
	users := map[string]any{
		adminUserID: 100,
	}
	if machineUserID != "" && machineUserID != adminUserID {
		users[machineUserID] = 50
	}

	events := AdminProtectedEvents()
	events[EventTypeProject] = 100
	events[EventTypeWorkspace] = 0
	events[EventTypeWorktree] = 0
	events[EventTypeLayout] = 0

	return map[string]any{
		"users":          users,
		"users_default":  0,
		"events":         events,
		"events_default": 0,
		"state_default":  0,
		"ban":            100,
		"kick":           100,
		"invite":         50,
		"redact":         100,
		"notifications": map[string]any{
			"room": 100,
		},
	}
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
func ConfigRoomPowerLevels(adminUserID, machineUserID string) map[string]any {
	users := map[string]any{
		adminUserID: 100,
	}
	if machineUserID != "" && machineUserID != adminUserID {
		users[machineUserID] = 50
	}

	events := AdminProtectedEvents()
	events[EventTypeMachineConfig] = 50 // machine and fleet controllers (PL 50) write placements
	events[EventTypeCredentials] = 100  // admin only
	events[EventTypeLayout] = 0         // daemon publishes layout state
	events[MatrixEventTypeMessage] = 0  // daemon posts command results and pipeline results

	return map[string]any{
		"users":          users,
		"users_default":  0,
		"events":         events,
		"events_default": 100,
		"state_default":  100,
		"ban":            100,
		"kick":           100,
		"invite":         50,
		"redact":         100,
		"notifications": map[string]any{
			"room": 100,
		},
	}
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
func PipelineRoomPowerLevels(adminUserID string) map[string]any {
	events := AdminProtectedEvents()
	events[EventTypePipeline] = 100

	return map[string]any{
		"users": map[string]any{
			adminUserID: 100,
		},
		"users_default":  0,
		"events":         events,
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

// CommandMessage is the parsed content of an m.bureau.command message.
// Commands are posted by the CLI to workspace rooms or config rooms,
// and the daemon processes them via /sync. The human-readable Body
// field ensures commands display legibly in any Matrix client.
type CommandMessage struct {
	// MsgType is the Matrix message type (always MsgTypeCommand for
	// incoming commands).
	MsgType string `json:"msgtype"`

	// Body is a human-readable representation of the command, shown
	// in standard Matrix clients (e.g., "workspace status iree/amdgpu/inference").
	Body string `json:"body"`

	// Command is the structured command name (e.g., "workspace.status",
	// "workspace.fetch", "principal.spawn").
	Command string `json:"command"`

	// Workspace is the target workspace for workspace-scoped commands
	// (e.g., "iree/amdgpu/inference"). Empty for global commands like
	// workspace.list.
	Workspace string `json:"workspace,omitempty"`

	// RequestID is an opaque identifier for correlating requests and
	// responses. The daemon includes it in the threaded result reply.
	// Generated by the CLI.
	RequestID string `json:"request_id,omitempty"`

	// SenderMachine identifies which machine the CLI is running on.
	// Informational — the daemon does not use this for routing.
	SenderMachine string `json:"sender_machine,omitempty"`

	// Parameters carries additional command-specific arguments. For
	// example, principal.spawn includes "template" and "payload"
	// fields. The daemon passes these through to the handler.
	Parameters map[string]any `json:"parameters,omitempty"`
}

// CommandResultMessage is the content of an m.room.message event with
// msgtype MsgTypeCommandResult. The daemon posts these as threaded replies
// to the original CommandMessage. The union of all fields covers three
// result shapes:
//
//   - Success: Status "success", Result contains command-specific payload
//   - Error: Status "error", Error contains the error message
//   - Pipeline result: Status "success"/"error", ExitCode and Steps from
//     the pipeline executor, optionally LogEventID for thread logging
//   - Accepted acknowledgment: Status "accepted", Principal names the
//     ephemeral executor principal (async commands like pipeline.execute)
//
// Fields not relevant to a particular result shape are omitted from JSON
// via omitempty.
type CommandResultMessage struct {
	MsgType    string               `json:"msgtype"`
	Body       string               `json:"body"`
	Status     string               `json:"status"`
	Result     json.RawMessage      `json:"result,omitempty"`
	Error      string               `json:"error,omitempty"`
	ExitCode   *int                 `json:"exit_code,omitempty"`
	DurationMS int64                `json:"duration_ms"`
	Steps      []PipelineStepResult `json:"steps,omitempty"`
	Outputs    map[string]string    `json:"outputs,omitempty"`
	LogEventID string               `json:"log_event_id,omitempty"`
	RequestID  string               `json:"request_id,omitempty"`
	Principal  string               `json:"principal,omitempty"`
	RelatesTo  *ThreadRelation      `json:"m.relates_to,omitempty"`
}

// ThreadRelation is the m.relates_to structure for threaded messages
// (MSC3440 / Matrix spec §11.10.1). Commands and their results are
// linked via this threading relation so that clients can render the
// request→response pair as a conversation thread.
type ThreadRelation struct {
	RelType       string     `json:"rel_type"`
	EventID       string     `json:"event_id"`
	IsFallingBack bool       `json:"is_falling_back,omitempty"`
	InReplyTo     *InReplyTo `json:"m.in_reply_to,omitempty"`
}

// InReplyTo identifies the event being replied to within a thread
// relation. This is the fallback reply target for clients that do
// not support threads.
type InReplyTo struct {
	EventID string `json:"event_id"`
}

// NewThreadRelation constructs a ThreadRelation for a threaded reply
// to the given event ID. This matches the structure the daemon uses
// when posting command results and pipeline results.
func NewThreadRelation(eventID string) *ThreadRelation {
	return &ThreadRelation{
		RelType:       "m.thread",
		EventID:       eventID,
		IsFallingBack: true,
		InReplyTo: &InReplyTo{
			EventID: eventID,
		},
	}
}

// TicketContent is the content of an EventTypeTicket state event. Each
// ticket is a work item tracked in a room. The ticket service maintains
// an indexed cache of these events for fast queries via its unix socket
// API, and writes mutations back to Matrix as state event PUTs.
//
// Multiple ticket service instances in a fleet may operate on overlapping
// rooms. The Version field and CanModify guard prevent silent data loss
// during rolling upgrades where different instances run different code
// versions. See CanModify for details.
//
// State key: ticket ID (e.g., "tkt-a3f9")
// Room: any room with ticket management enabled
type TicketContent struct {
	// Version is the schema version (see TicketContentVersion).
	// Code that modifies this event must call CanModify() first; if
	// Version exceeds TicketContentVersion, the modification is
	// refused to prevent silent field loss. Readers may process any
	// version (unknown fields are harmlessly ignored by Go's JSON
	// unmarshaler).
	Version int `json:"version"`

	// Title is a short summary of the work item.
	Title string `json:"title"`

	// Body is the full description, supporting markdown.
	Body string `json:"body,omitempty"`

	// Status is the lifecycle state: "open", "in_progress",
	// "blocked", "closed". The ticket service computes derived
	// readiness from status + dependency graph + gate satisfaction,
	// but "blocked" is also a valid explicit status for agents to
	// signal blockage on things not tracked as ticket dependencies.
	//
	// Contention detection: the ticket service rejects transitions
	// to "in_progress" when the ticket is already "in_progress",
	// returning a conflict error with the current assignee. This
	// forces agents to claim work atomically (open → in_progress)
	// before beginning any planning or implementation. Assignee
	// must be set in the same mutation that transitions to
	// "in_progress".
	Status string `json:"status"`

	// Priority is 0-4: 0=critical, 1=high, 2=medium, 3=low,
	// 4=backlog.
	Priority int `json:"priority"`

	// Type categorizes the work: "task", "bug", "feature",
	// "epic", "chore", "docs", "question".
	Type string `json:"type"`

	// Labels are free-form tags for filtering and grouping.
	Labels []string `json:"labels,omitempty"`

	// Assignee is the Matrix user ID of the principal working
	// on this ticket (e.g., "@iree/amdgpu/pm:bureau.local").
	// Single assignee: Bureau agents are principals with unique
	// identities, one agent works one ticket. Multiple people
	// on something means sub-tickets (parent-child).
	Assignee string `json:"assignee,omitempty"`

	// Parent is the ticket ID of the parent work item (e.g., an
	// epic). Enables hierarchical breakdown: the ticket service
	// computes children sets from the reverse mapping for progress
	// tracking and cascading operations.
	Parent string `json:"parent,omitempty"`

	// BlockedBy lists ticket IDs (in this room) that must be
	// closed before this ticket is considered ready. The ticket
	// service computes the transitive closure for dependency
	// queries and detects cycles on mutation.
	BlockedBy []string `json:"blocked_by,omitempty"`

	// Gates are async coordination conditions that must all be
	// satisfied before the ticket is considered ready. See
	// TicketGate for the gate evaluation model.
	Gates []TicketGate `json:"gates,omitempty"`

	// Notes are short annotations attached to the ticket by
	// agents, services, or humans. Each note is a self-contained
	// piece of context that travels with the ticket — warnings,
	// references, analysis results. Notes are embedded (not
	// Matrix threads) for single-read completeness: an agent
	// reading a ticket gets all context without a second fetch.
	Notes []TicketNote `json:"notes,omitempty"`

	// Attachments are references to artifacts stored outside the
	// ticket. The ticket stores references, not content.
	Attachments []TicketAttachment `json:"attachments,omitempty"`

	// CreatedBy is the Matrix user ID of the ticket creator.
	CreatedBy string `json:"created_by"`

	// CreatedAt is an ISO 8601 timestamp.
	CreatedAt string `json:"created_at"`

	// UpdatedAt is an ISO 8601 timestamp of the last modification.
	UpdatedAt string `json:"updated_at"`

	// ClosedAt is set when status transitions to "closed".
	ClosedAt string `json:"closed_at,omitempty"`

	// CloseReason explains why the ticket was closed.
	CloseReason string `json:"close_reason,omitempty"`

	// Origin tracks where this ticket came from when imported
	// from an external system (GitHub, beads JSONL, etc.).
	Origin *TicketOrigin `json:"origin,omitempty"`

	// Extra is a documented extension namespace for experimental or
	// preview fields before promotion to top-level schema fields in
	// a version bump. Keys are field names; values are arbitrary
	// JSON. Application code should not read or write Extra directly
	// in production — it exists for forward compatibility and field
	// staging. Extra is NOT a round-trip preservation mechanism for
	// unknown top-level fields; the Version/CanModify guard handles
	// that by refusing modification of events with unrecognized
	// versions.
	Extra map[string]json.RawMessage `json:"extra,omitempty"`
}

// Validate checks that all required fields are present and well-formed.
// Returns an error describing the first invalid field found, or nil if
// the content is valid. Recursively validates embedded gates, notes,
// attachments, and origin.
func (t *TicketContent) Validate() error {
	if t.Version < 1 {
		return fmt.Errorf("ticket content: version must be >= 1, got %d", t.Version)
	}
	if t.Title == "" {
		return errors.New("ticket content: title is required")
	}
	switch t.Status {
	case "open", "in_progress", "blocked", "closed":
		// Valid.
	case "":
		return errors.New("ticket content: status is required")
	default:
		return fmt.Errorf("ticket content: unknown status %q", t.Status)
	}
	if t.Priority < 0 || t.Priority > 4 {
		return fmt.Errorf("ticket content: priority must be 0-4, got %d", t.Priority)
	}
	switch t.Type {
	case "task", "bug", "feature", "epic", "chore", "docs", "question":
		// Valid.
	case "":
		return errors.New("ticket content: type is required")
	default:
		return fmt.Errorf("ticket content: unknown type %q", t.Type)
	}
	if t.CreatedBy == "" {
		return errors.New("ticket content: created_by is required")
	}
	if t.CreatedAt == "" {
		return errors.New("ticket content: created_at is required")
	}
	if t.UpdatedAt == "" {
		return errors.New("ticket content: updated_at is required")
	}
	for i := range t.Gates {
		if err := t.Gates[i].Validate(); err != nil {
			return fmt.Errorf("ticket content: gates[%d]: %w", i, err)
		}
	}
	for i := range t.Notes {
		if err := t.Notes[i].Validate(); err != nil {
			return fmt.Errorf("ticket content: notes[%d]: %w", i, err)
		}
	}
	for i := range t.Attachments {
		if err := t.Attachments[i].Validate(); err != nil {
			return fmt.Errorf("ticket content: attachments[%d]: %w", i, err)
		}
	}
	if t.Origin != nil {
		if err := t.Origin.Validate(); err != nil {
			return fmt.Errorf("ticket content: origin: %w", err)
		}
	}
	return nil
}

// CanModify checks whether this code version can safely perform a
// read-modify-write cycle on this event. Returns nil if safe, or an
// error explaining why modification would risk data loss.
//
// If the event's Version exceeds TicketContentVersion, this code does
// not understand all fields in the event. Marshaling the modified struct
// back to JSON would silently drop the unknown fields. The caller must
// either upgrade the ticket service or refuse the operation.
//
// Read-only access does not require CanModify — unknown fields are
// harmlessly ignored during display, listing, and search.
func (t *TicketContent) CanModify() error {
	if t.Version > TicketContentVersion {
		return fmt.Errorf(
			"ticket content version %d exceeds supported version %d: "+
				"modification would lose fields added in newer versions; "+
				"upgrade the ticket service before modifying this event",
			t.Version, TicketContentVersion,
		)
	}
	return nil
}

// TicketGate is an async coordination condition on a ticket. Each gate
// represents something that must happen before the ticket is ready: a
// CI pipeline must pass, a human must approve, a timer must expire,
// another ticket must close, or an arbitrary Matrix state event must
// appear.
//
// The ticket service evaluates gates via its /sync loop — no polling.
// Gate types map to evaluation strategies:
//
//   - "human": No automatic evaluation. Resolved explicitly via
//     bureau ticket gate resolve. The ticket service records who
//     approved and when.
//
//   - "pipeline": Watches for m.bureau.pipeline_result state events
//     where the pipeline ref matches PipelineRef and the conclusion
//     matches Conclusion. Syntactic sugar for a state_event gate.
//
//   - "state_event": Watches for a Matrix state event matching
//     EventType + StateKey + RoomAlias + ContentMatch. This is the
//     general-purpose gate — pipeline and ticket gates are special
//     cases of it.
//
//   - "ticket": Watches for m.bureau.ticket with the given TicketID
//     transitioning to status "closed" in the same room.
//
//   - "timer": Checks whether the current time exceeds the gate's
//     CreatedAt + Duration on each /sync tick.
type TicketGate struct {
	// ID uniquely identifies this gate within the ticket (e.g.,
	// "ci-pass", "lead-approval"). Used for targeted updates.
	ID string `json:"id"`

	// Type determines how the condition is evaluated: "human",
	// "pipeline", "state_event", "ticket", or "timer".
	Type string `json:"type"`

	// Status is "pending" or "satisfied". The ticket service
	// transitions this when the condition is met.
	Status string `json:"status"`

	// Description is a human-readable explanation of what this
	// gate waits for (e.g., "CI pipeline must pass", "24h soak
	// period"). Shown in ticket listings and notifications.
	Description string `json:"description,omitempty"`

	// --- Type-specific condition fields ---

	// PipelineRef identifies the pipeline to watch (type
	// "pipeline"). Matches against the pipeline_ref field in
	// m.bureau.pipeline_result events.
	PipelineRef string `json:"pipeline_ref,omitempty"`

	// Conclusion is the required pipeline result (type
	// "pipeline"). Typically "success". If empty, any completed
	// result satisfies the gate.
	Conclusion string `json:"conclusion,omitempty"`

	// EventType is the Matrix state event type to watch (type
	// "state_event"). Same semantics as StartCondition.EventType.
	EventType string `json:"event_type,omitempty"`

	// StateKey is the state key to match (type "state_event").
	StateKey string `json:"state_key,omitempty"`

	// RoomAlias is the room to watch (type "state_event"). When
	// empty, watches the ticket's own room.
	RoomAlias string `json:"room_alias,omitempty"`

	// ContentMatch specifies criteria that must be satisfied in the
	// watched event's content (type "state_event"). Same semantics
	// as StartCondition.ContentMatch: bare scalars for equality,
	// $-prefixed operators for comparisons and set membership.
	ContentMatch ContentMatch `json:"content_match,omitempty"`

	// TicketID is the ticket to watch (type "ticket"). The gate
	// is satisfied when that ticket's status becomes "closed".
	TicketID string `json:"ticket_id,omitempty"`

	// Duration is how long to wait (type "timer"). Parsed by
	// time.ParseDuration (e.g., "24h", "30m"). The deadline is
	// computed from the gate's CreatedAt + Duration.
	Duration string `json:"duration,omitempty"`

	// --- Lifecycle metadata ---

	// CreatedAt is when this gate was added. For gates present
	// at ticket creation, equals the ticket's CreatedAt. For
	// gates added later, this is when the gate was appended.
	// Used as the base time for timer gates.
	CreatedAt string `json:"created_at,omitempty"`

	// SatisfiedAt is set when the gate transitions to "satisfied".
	SatisfiedAt string `json:"satisfied_at,omitempty"`

	// SatisfiedBy records what satisfied the gate: an event ID
	// for state_event/pipeline/ticket gates, a Matrix user ID
	// for human gates, "timer" for timer gates.
	SatisfiedBy string `json:"satisfied_by,omitempty"`
}

// Validate checks that the gate has a valid type, status, and the
// type-specific fields required for its gate type.
func (g *TicketGate) Validate() error {
	if g.ID == "" {
		return errors.New("gate: id is required")
	}
	switch g.Type {
	case "human":
		// No type-specific fields required.
	case "pipeline":
		if g.PipelineRef == "" {
			return fmt.Errorf("gate %q: pipeline_ref is required for pipeline gates", g.ID)
		}
	case "state_event":
		if g.EventType == "" {
			return fmt.Errorf("gate %q: event_type is required for state_event gates", g.ID)
		}
	case "ticket":
		if g.TicketID == "" {
			return fmt.Errorf("gate %q: ticket_id is required for ticket gates", g.ID)
		}
	case "timer":
		if g.Duration == "" {
			return fmt.Errorf("gate %q: duration is required for timer gates", g.ID)
		}
	case "":
		return fmt.Errorf("gate %q: type is required", g.ID)
	default:
		return fmt.Errorf("gate %q: unknown type %q", g.ID, g.Type)
	}
	switch g.Status {
	case "pending", "satisfied":
		// Valid.
	case "":
		return fmt.Errorf("gate %q: status is required", g.ID)
	default:
		return fmt.Errorf("gate %q: unknown status %q", g.ID, g.Status)
	}
	return nil
}

// TicketNote is a short annotation on a ticket. Notes are for context
// that should travel with the ticket: warnings, references, analysis
// results, review comments. They are not conversations — use Matrix
// thread replies for discussion.
//
// Notes are append-only from the caller's perspective: the ticket
// service assigns IDs and timestamps. Notes can be removed by ID
// but not edited (append a correction instead).
type TicketNote struct {
	// ID uniquely identifies this note within the ticket.
	// Assigned by the ticket service (e.g., "n-1", "n-2").
	ID string `json:"id"`

	// Author is the Matrix user ID of the note creator.
	Author string `json:"author"`

	// CreatedAt is an ISO 8601 timestamp.
	CreatedAt string `json:"created_at"`

	// Body is the note content, supporting markdown. Keep notes
	// concise — for content that exceeds a few hundred bytes,
	// store it as an artifact and reference it from a note.
	Body string `json:"body"`
}

// Validate checks that all required fields are present.
func (n *TicketNote) Validate() error {
	if n.ID == "" {
		return errors.New("note: id is required")
	}
	if n.Author == "" {
		return errors.New("note: author is required")
	}
	if n.CreatedAt == "" {
		return errors.New("note: created_at is required")
	}
	if n.Body == "" {
		return errors.New("note: body is required")
	}
	return nil
}

// TicketAttachment is a reference to an artifact stored outside the
// ticket state event. The ticket service stores references, not content.
type TicketAttachment struct {
	// Ref is the artifact reference (e.g., "art-<hash>").
	Ref string `json:"ref"`

	// Label is a human-readable description shown in listings
	// (e.g., "stack trace", "screenshot of rendering bug").
	Label string `json:"label,omitempty"`

	// ContentType is the MIME type of the referenced content
	// (e.g., "text/plain", "image/png").
	ContentType string `json:"content_type,omitempty"`
}

// Validate checks that the ref field is present and uses a supported
// format. MXC URIs are not supported — all blob data goes through the
// artifact service.
func (a *TicketAttachment) Validate() error {
	if a.Ref == "" {
		return errors.New("attachment: ref is required")
	}
	if strings.HasPrefix(a.Ref, "mxc://") {
		return errors.New("attachment: mxc:// refs are not supported, use the artifact service")
	}
	return nil
}

// TicketOrigin records the provenance of an imported ticket.
type TicketOrigin struct {
	// Source identifies the external system ("github", "beads",
	// "linear", etc.).
	Source string `json:"source"`

	// ExternalRef is the identifier in the source system
	// (e.g., "bureau-foundation/bureau#42" for GitHub, "PROJ-123"
	// for Jira/Linear).
	ExternalRef string `json:"external_ref"`

	// SourceRoom is the room ID where this ticket was originally
	// created, if it was moved between rooms.
	SourceRoom string `json:"source_room,omitempty"`
}

// Validate checks that the required source and external_ref fields
// are present.
func (o *TicketOrigin) Validate() error {
	if o.Source == "" {
		return errors.New("origin: source is required")
	}
	if o.ExternalRef == "" {
		return errors.New("origin: external_ref is required")
	}
	return nil
}

// TicketConfigContent enables and configures ticket management for a
// room. Rooms without this event do not accept ticket operations from
// the ticket service. Published by the admin via "bureau ticket enable".
//
// State key: "" (singleton per room)
// Room: any room that wants ticket management
type TicketConfigContent struct {
	// Version is the schema version (see TicketConfigVersion).
	// Same semantics as TicketContent.Version — call CanModify()
	// before any read-modify-write cycle.
	Version int `json:"version"`

	// Prefix is the ticket ID prefix for this room. Defaults
	// to "tkt" if empty. The ticket service generates IDs as
	// prefix + "-" + short hash.
	Prefix string `json:"prefix,omitempty"`

	// DefaultLabels are applied to new tickets that don't
	// explicitly specify labels.
	DefaultLabels []string `json:"default_labels,omitempty"`

	// Extra is a documented extension namespace. Same semantics as
	// TicketContent.Extra.
	Extra map[string]json.RawMessage `json:"extra,omitempty"`
}

// Validate checks that the config has a valid version.
func (c *TicketConfigContent) Validate() error {
	if c.Version < 1 {
		return fmt.Errorf("ticket config: version must be >= 1, got %d", c.Version)
	}
	return nil
}

// CanModify checks whether this code version can safely modify this
// config event. Same semantics as TicketContent.CanModify.
func (c *TicketConfigContent) CanModify() error {
	if c.Version > TicketConfigVersion {
		return fmt.Errorf(
			"ticket config version %d exceeds supported version %d: "+
				"modification would lose fields added in newer versions; "+
				"upgrade the ticket service before modifying this event",
			c.Version, TicketConfigVersion,
		)
	}
	return nil
}

// ArtifactScope is the content of an EventTypeArtifactScope state event.
// Room-level configuration that connects a room to the artifact service:
// which service principal manages artifacts for this room, and which
// tag name patterns the room subscribes to for notifications.
//
// Per-artifact metadata and per-tag mappings are owned by the artifact
// service's persistent store, not Matrix state events. This avoids
// scaling issues (100K+ artifacts would overwhelm Matrix /sync).
type ArtifactScope struct {
	// Version is the schema version (see ArtifactScopeVersion).
	// Code that modifies this event must call CanModify() first; if
	// Version exceeds ArtifactScopeVersion, the modification is
	// refused to prevent silent field loss. Readers may process any
	// version (unknown fields are harmlessly ignored by Go's JSON
	// unmarshaler).
	Version int `json:"version"`

	// ServicePrincipal is the Matrix user ID of the artifact service
	// instance that manages artifacts for this room
	// (e.g., "@service/artifact/main:bureau.local").
	ServicePrincipal string `json:"service_principal"`

	// TagGlobs is a list of tag name patterns (glob syntax) that
	// this room subscribes to. The artifact service pushes
	// notifications to the room when matching tags are created or
	// updated. Example: ["iree/resnet50/**", "shared/datasets/*"].
	TagGlobs []string `json:"tag_globs,omitempty"`
}

// Validate checks that all required fields are present and well-formed.
func (a *ArtifactScope) Validate() error {
	if a.Version < 1 {
		return fmt.Errorf("artifact scope: version must be >= 1, got %d", a.Version)
	}
	if a.ServicePrincipal == "" {
		return errors.New("artifact scope: service_principal is required")
	}
	return nil
}

// CanModify checks whether this code version can safely perform a
// read-modify-write cycle on this event. Returns nil if safe, or an
// error explaining why modification would risk data loss.
//
// If the event's Version exceeds ArtifactScopeVersion, this code does
// not understand all fields in the event. Marshaling the modified struct
// back to JSON would silently drop the unknown fields. The caller must
// either upgrade or refuse the operation.
//
// Read-only access does not require CanModify — unknown fields are
// harmlessly ignored during display, routing, and service discovery.
func (a *ArtifactScope) CanModify() error {
	if a.Version > ArtifactScopeVersion {
		return fmt.Errorf(
			"artifact scope version %d exceeds supported version %d: "+
				"modification would lose fields added in newer versions; "+
				"upgrade before modifying this event",
			a.Version, ArtifactScopeVersion,
		)
	}
	return nil
}

// ArtifactRoomPowerLevels returns the power level structure for the
// artifact coordination room (#bureau/artifact). Per-artifact metadata
// and tag mappings live in the artifact service, not as Matrix state
// events. The room carries only admin-level configuration
// (EventTypeArtifactScope) and coordination messages.
//
// Room membership is invite-only — the admin invites artifact service
// principals and machine daemons during setup.
func ArtifactRoomPowerLevels(adminUserID string) map[string]any {
	events := AdminProtectedEvents()

	return map[string]any{
		"users": map[string]any{
			adminUserID: 100,
		},
		"users_default":  0,
		"events":         events,
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

// --------------------------------------------------------------------
// Daemon notification message types
// --------------------------------------------------------------------
//
// Each notification type is an m.room.message event with a custom
// msgtype. The daemon posts these to config rooms so that operators
// and integration tests can observe system state changes. Every struct
// carries a human-readable Body for Matrix client display alongside
// typed fields for programmatic consumption. Tests must match on
// typed fields, never on Body content.

// ServiceDirectoryUpdatedMessage is the content of an m.room.message
// event with msgtype MsgTypeServiceDirectoryUpdated. Posted after the
// daemon syncs service room state and pushes the updated directory to
// all running proxies.
type ServiceDirectoryUpdatedMessage struct {
	MsgType string   `json:"msgtype"`
	Body    string   `json:"body"`
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
	Updated []string `json:"updated,omitempty"`
}

// NewServiceDirectoryUpdatedMessage constructs a ServiceDirectoryUpdatedMessage
// with a human-readable body summarizing the changes.
func NewServiceDirectoryUpdatedMessage(added, removed, updated []string) ServiceDirectoryUpdatedMessage {
	var parts []string
	for _, name := range added {
		parts = append(parts, "added "+name)
	}
	for _, name := range removed {
		parts = append(parts, "removed "+name)
	}
	for _, name := range updated {
		parts = append(parts, "updated "+name)
	}
	return ServiceDirectoryUpdatedMessage{
		MsgType: MsgTypeServiceDirectoryUpdated,
		Body:    "Service directory updated: " + strings.Join(parts, ", "),
		Added:   added,
		Removed: removed,
		Updated: updated,
	}
}

// GrantsUpdatedMessage is the content of an m.room.message event with
// msgtype MsgTypeGrantsUpdated. Posted after the daemon hot-reloads
// authorization grants for a running principal's proxy.
type GrantsUpdatedMessage struct {
	MsgType    string `json:"msgtype"`
	Body       string `json:"body"`
	Principal  string `json:"principal"`
	GrantCount int    `json:"grant_count"`
}

// NewGrantsUpdatedMessage constructs a GrantsUpdatedMessage.
func NewGrantsUpdatedMessage(principal string, grantCount int) GrantsUpdatedMessage {
	return GrantsUpdatedMessage{
		MsgType:    MsgTypeGrantsUpdated,
		Body:       fmt.Sprintf("Authorization grants updated for %s (%d grants)", principal, grantCount),
		Principal:  principal,
		GrantCount: grantCount,
	}
}

// PayloadUpdatedMessage is the content of an m.room.message event with
// msgtype MsgTypePayloadUpdated. Posted after the daemon writes an
// updated payload file for a running principal.
type PayloadUpdatedMessage struct {
	MsgType   string `json:"msgtype"`
	Body      string `json:"body"`
	Principal string `json:"principal"`
}

// NewPayloadUpdatedMessage constructs a PayloadUpdatedMessage.
func NewPayloadUpdatedMessage(principal string) PayloadUpdatedMessage {
	return PayloadUpdatedMessage{
		MsgType:   MsgTypePayloadUpdated,
		Body:      fmt.Sprintf("Payload updated for %s", principal),
		Principal: principal,
	}
}

// PrincipalAdoptedMessage is the content of an m.room.message event
// with msgtype MsgTypePrincipalAdopted. Posted when the daemon
// discovers and adopts a running sandbox from a previous daemon
// instance during startup recovery.
type PrincipalAdoptedMessage struct {
	MsgType   string `json:"msgtype"`
	Body      string `json:"body"`
	Principal string `json:"principal"`
}

// NewPrincipalAdoptedMessage constructs a PrincipalAdoptedMessage.
func NewPrincipalAdoptedMessage(principal string) PrincipalAdoptedMessage {
	return PrincipalAdoptedMessage{
		MsgType:   MsgTypePrincipalAdopted,
		Body:      fmt.Sprintf("Adopted %s from previous daemon instance", principal),
		Principal: principal,
	}
}

// SandboxExitedMessage is the content of an m.room.message event with
// msgtype MsgTypeSandboxExited. Posted when a sandbox process terminates.
// Non-zero exit codes include the exit description and captured terminal
// output for diagnosis.
type SandboxExitedMessage struct {
	MsgType         string `json:"msgtype"`
	Body            string `json:"body"`
	Principal       string `json:"principal"`
	ExitCode        int    `json:"exit_code"`
	ExitDescription string `json:"exit_description,omitempty"`
	CapturedOutput  string `json:"captured_output,omitempty"`
}

// NewSandboxExitedMessage constructs a SandboxExitedMessage with a
// human-readable body that includes exit status and captured output.
func NewSandboxExitedMessage(principal string, exitCode int, exitDescription, capturedOutput string) SandboxExitedMessage {
	status := "exited normally"
	if exitCode != 0 {
		status = fmt.Sprintf("exited with code %d", exitCode)
		if exitDescription != "" {
			status += fmt.Sprintf(" (%s)", exitDescription)
		}
	}
	body := fmt.Sprintf("Sandbox %s %s", principal, status)
	if exitCode != 0 && capturedOutput != "" {
		body += "\n\nCaptured output:\n" + capturedOutput
	}
	return SandboxExitedMessage{
		MsgType:         MsgTypeSandboxExited,
		Body:            body,
		Principal:       principal,
		ExitCode:        exitCode,
		ExitDescription: exitDescription,
		CapturedOutput:  capturedOutput,
	}
}

// CredentialsRotatedMessage is the content of an m.room.message event
// with msgtype MsgTypeCredentialsRotated. Posted during the credential
// rotation lifecycle: when a principal is being restarted for rotation,
// when the restart completes, or when it fails.
type CredentialsRotatedMessage struct {
	MsgType   string `json:"msgtype"`
	Body      string `json:"body"`
	Principal string `json:"principal"`
	Status    string `json:"status"` // "restarting", "completed", "failed"
	Error     string `json:"error,omitempty"`
}

// NewCredentialsRotatedMessage constructs a CredentialsRotatedMessage.
// Status must be "restarting", "completed", or "failed". For "failed"
// status, pass the error message; for other statuses pass empty string.
func NewCredentialsRotatedMessage(principal, status, errorMessage string) CredentialsRotatedMessage {
	var body string
	switch status {
	case "restarting":
		body = fmt.Sprintf("Restarting %s: credentials updated", principal)
	case "completed":
		body = fmt.Sprintf("Restarted %s with new credentials", principal)
	case "failed":
		body = fmt.Sprintf("FAILED to restart %s after credential rotation: %s", principal, errorMessage)
	default:
		body = fmt.Sprintf("Credential rotation for %s: %s", principal, status)
	}
	return CredentialsRotatedMessage{
		MsgType:   MsgTypeCredentialsRotated,
		Body:      body,
		Principal: principal,
		Status:    status,
		Error:     errorMessage,
	}
}

// ProxyCrashMessage is the content of an m.room.message event with
// msgtype MsgTypeProxyCrash. Posted when a proxy process exits
// unexpectedly and the daemon attempts recovery. The lifecycle is:
// detected (proxy crashed), then either recovered (sandbox recreated)
// or failed (recovery unsuccessful).
type ProxyCrashMessage struct {
	MsgType   string `json:"msgtype"`
	Body      string `json:"body"`
	Principal string `json:"principal"`
	ExitCode  int    `json:"exit_code,omitempty"`
	Status    string `json:"status"` // "detected", "recovered", "failed"
	Error     string `json:"error,omitempty"`
}

// NewProxyCrashMessage constructs a ProxyCrashMessage. Status must be
// "detected", "recovered", or "failed".
func NewProxyCrashMessage(principal, status string, exitCode int, errorMessage string) ProxyCrashMessage {
	var body string
	switch status {
	case "detected":
		body = fmt.Sprintf("CRITICAL: Proxy for %s exited unexpectedly (code %d). Sandbox destroyed, re-reconciling.", principal, exitCode)
	case "recovered":
		body = fmt.Sprintf("Recovered %s after proxy crash", principal)
	case "failed":
		body = fmt.Sprintf("FAILED to recover %s after proxy crash: %s", principal, errorMessage)
	default:
		body = fmt.Sprintf("Proxy crash for %s: %s", principal, status)
	}
	return ProxyCrashMessage{
		MsgType:   MsgTypeProxyCrash,
		Body:      body,
		Principal: principal,
		ExitCode:  exitCode,
		Status:    status,
		Error:     errorMessage,
	}
}

// HealthCheckMessage is the content of an m.room.message event with
// msgtype MsgTypeHealthCheck. Posted when a health check fails and the
// daemon takes corrective action. Outcomes: "destroyed" (no rollback
// available), "rolled_back" (reverted to previous working config),
// "rollback_failed" (rollback attempted but failed).
type HealthCheckMessage struct {
	MsgType   string `json:"msgtype"`
	Body      string `json:"body"`
	Principal string `json:"principal"`
	Outcome   string `json:"outcome"` // "destroyed", "rolled_back", "rollback_failed"
	Error     string `json:"error,omitempty"`
}

// NewHealthCheckMessage constructs a HealthCheckMessage.
func NewHealthCheckMessage(principal, outcome, errorMessage string) HealthCheckMessage {
	var body string
	switch outcome {
	case "destroyed":
		body = fmt.Sprintf("CRITICAL: %s health check failed, no previous working configuration. Principal destroyed.", principal)
	case "rolled_back":
		body = fmt.Sprintf("Rolled back %s to previous working configuration after health check failure.", principal)
	case "rollback_failed":
		body = fmt.Sprintf("CRITICAL: %s rollback failed: %s. Principal destroyed.", principal, errorMessage)
	default:
		body = fmt.Sprintf("Health check for %s: %s", principal, outcome)
	}
	return HealthCheckMessage{
		MsgType:   MsgTypeHealthCheck,
		Body:      body,
		Principal: principal,
		Outcome:   outcome,
		Error:     errorMessage,
	}
}

// DaemonSelfUpdateMessage is the content of an m.room.message event
// with msgtype MsgTypeDaemonSelfUpdate. Posted during daemon binary
// self-update: when exec() is initiated, when the new binary starts
// successfully, or when the update fails and reverts.
type DaemonSelfUpdateMessage struct {
	MsgType        string `json:"msgtype"`
	Body           string `json:"body"`
	PreviousBinary string `json:"previous_binary"`
	NewBinary      string `json:"new_binary"`
	Status         string `json:"status"` // "in_progress", "succeeded", "failed"
	Error          string `json:"error,omitempty"`
}

// NewDaemonSelfUpdateMessage constructs a DaemonSelfUpdateMessage.
func NewDaemonSelfUpdateMessage(previousBinary, newBinary, status, errorMessage string) DaemonSelfUpdateMessage {
	var body string
	switch status {
	case "in_progress":
		body = fmt.Sprintf("Daemon self-updating: exec() %s (was %s)", newBinary, previousBinary)
	case "succeeded":
		body = fmt.Sprintf("Daemon self-update succeeded: now running %s (was %s)", newBinary, previousBinary)
	case "failed":
		body = fmt.Sprintf("Daemon self-update failed: %s (was %s, continuing with %s)", errorMessage, previousBinary, previousBinary)
	default:
		body = fmt.Sprintf("Daemon self-update %s: %s → %s", status, previousBinary, newBinary)
	}
	return DaemonSelfUpdateMessage{
		MsgType:        MsgTypeDaemonSelfUpdate,
		Body:           body,
		PreviousBinary: previousBinary,
		NewBinary:      newBinary,
		Status:         status,
		Error:          errorMessage,
	}
}

// NixPrefetchFailedMessage is the content of an m.room.message event
// with msgtype MsgTypeNixPrefetchFailed. Posted when the daemon fails
// to prefetch a principal's Nix environment closure. The prefetch
// retries automatically on the next reconciliation cycle.
type NixPrefetchFailedMessage struct {
	MsgType   string `json:"msgtype"`
	Body      string `json:"body"`
	Principal string `json:"principal"`
	StorePath string `json:"store_path"`
	Error     string `json:"error"`
}

// NewNixPrefetchFailedMessage constructs a NixPrefetchFailedMessage.
func NewNixPrefetchFailedMessage(principal, storePath, errorMessage string) NixPrefetchFailedMessage {
	return NixPrefetchFailedMessage{
		MsgType:   MsgTypeNixPrefetchFailed,
		Body:      fmt.Sprintf("Failed to prefetch Nix environment for %s: %s (will retry on next reconcile cycle)", principal, errorMessage),
		Principal: principal,
		StorePath: storePath,
		Error:     errorMessage,
	}
}

// PrincipalStartFailedMessage is the content of an m.room.message
// event with msgtype MsgTypePrincipalStartFailed. Posted when the
// daemon cannot start a principal — service resolution failure, token
// minting failure, or other sandbox creation error.
type PrincipalStartFailedMessage struct {
	MsgType   string `json:"msgtype"`
	Body      string `json:"body"`
	Principal string `json:"principal"`
	Error     string `json:"error"`
}

// NewPrincipalStartFailedMessage constructs a PrincipalStartFailedMessage.
func NewPrincipalStartFailedMessage(principal, errorMessage string) PrincipalStartFailedMessage {
	return PrincipalStartFailedMessage{
		MsgType:   MsgTypePrincipalStartFailed,
		Body:      fmt.Sprintf("Cannot start %s: %s", principal, errorMessage),
		Principal: principal,
		Error:     errorMessage,
	}
}

// PrincipalRestartedMessage is the content of an m.room.message event
// with msgtype MsgTypePrincipalRestarted. Posted when the daemon
// destroys and recreates a principal because its sandbox configuration
// changed (template update, environment change, etc.).
type PrincipalRestartedMessage struct {
	MsgType   string `json:"msgtype"`
	Body      string `json:"body"`
	Principal string `json:"principal"`
	Template  string `json:"template"`
}

// NewPrincipalRestartedMessage constructs a PrincipalRestartedMessage.
func NewPrincipalRestartedMessage(principal, template string) PrincipalRestartedMessage {
	return PrincipalRestartedMessage{
		MsgType:   MsgTypePrincipalRestarted,
		Body:      fmt.Sprintf("Restarting %s: sandbox configuration changed (template %s)", principal, template),
		Principal: principal,
		Template:  template,
	}
}

// BureauVersionUpdateMessage is the content of an m.room.message event
// with msgtype MsgTypeBureauVersionUpdate. Posted when the daemon
// reconciles a BureauVersion state event. Status "prefetch_failed"
// indicates store path prefetching failed (retries next cycle). Status
// "reconciled" summarizes which binary updates were applied.
type BureauVersionUpdateMessage struct {
	MsgType         string `json:"msgtype"`
	Body            string `json:"body"`
	Status          string `json:"status"` // "prefetch_failed", "reconciled"
	Error           string `json:"error,omitempty"`
	ProxyChanged    bool   `json:"proxy_changed,omitempty"`
	LauncherChanged bool   `json:"launcher_changed,omitempty"`
}

// NewBureauVersionPrefetchFailedMessage constructs a BureauVersionUpdateMessage
// for a store path prefetch failure.
func NewBureauVersionPrefetchFailedMessage(errorMessage string) BureauVersionUpdateMessage {
	return BureauVersionUpdateMessage{
		MsgType: MsgTypeBureauVersionUpdate,
		Body:    fmt.Sprintf("Failed to prefetch BureauVersion store paths: %s (will retry on next reconcile cycle)", errorMessage),
		Status:  "prefetch_failed",
		Error:   errorMessage,
	}
}

// NewBureauVersionReconciledMessage constructs a BureauVersionUpdateMessage
// summarizing which binary updates were applied.
func NewBureauVersionReconciledMessage(proxyChanged, launcherChanged bool) BureauVersionUpdateMessage {
	var parts []string
	if proxyChanged {
		parts = append(parts, "proxy binary updated for future sandbox creation")
	}
	if launcherChanged {
		parts = append(parts, "launcher exec() initiated")
	}
	return BureauVersionUpdateMessage{
		MsgType:         MsgTypeBureauVersionUpdate,
		Body:            "BureauVersion: " + strings.Join(parts, "; ") + ".",
		Status:          "reconciled",
		ProxyChanged:    proxyChanged,
		LauncherChanged: launcherChanged,
	}
}
