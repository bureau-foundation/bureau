// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

	// EventTypeArtifact is published to #bureau/artifact when an
	// artifact is stored in the content-addressable store. Contains
	// metadata: BLAKE3 hash, content type, size, chunk/container
	// counts, compression algorithm, lifecycle policy. The artifact
	// data itself lives in the CAS filesystem; this event is the
	// metadata index.
	//
	// State key: artifact reference (e.g., "art-a3f9b2c1e7d4")
	// Room: #bureau/artifact:<server>
	EventTypeArtifact = "m.bureau.artifact"

	// EventTypeArtifactTag is published to #bureau/artifact when a
	// named mutable pointer to an artifact is created or updated.
	// Tags provide stable URIs for changing content (e.g.,
	// "iree/resnet50/compiled/latest" always points to the most
	// recent compiled model). Tag updates are compare-and-swap by
	// default to detect concurrent writes.
	//
	// State key: tag name (e.g., "iree/resnet50/compiled/latest")
	// Room: #bureau/artifact:<server>
	EventTypeArtifactTag = "m.bureau.artifact_tag"
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
	// ArtifactContentVersion is the current schema version for
	// ArtifactContent events. Increment when adding fields that
	// existing code must not silently drop during read-modify-write.
	ArtifactContentVersion = 1

	// ArtifactTagVersion is the current schema version for
	// ArtifactTag events.
	ArtifactTagVersion = 1

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

	// DefaultObservePolicy is the fallback observation policy for
	// principals on this machine that do not specify their own
	// ObservePolicy in PrincipalAssignment. When nil, observation is
	// denied for any principal without an explicit per-principal policy.
	// This provides a convenient machine-wide default while allowing
	// per-principal overrides.
	DefaultObservePolicy *ObservePolicy `json:"default_observe_policy,omitempty"`

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

	// ContentMatch specifies key-value pairs that must all match in
	// the event content for the condition to be satisfied. When nil
	// or empty, only event existence is checked.
	//
	// For string fields, this is exact equality: the field value must
	// equal the match value. For array fields, this is containment:
	// the array must contain a string element equal to the match value.
	// All entries must match (AND semantics across keys).
	//
	// Examples:
	//   - {"status": "active"} matches when the event's "status" field
	//     is the string "active".
	//   - {"labels": "bug"} matches when the event's "labels" field is
	//     an array containing the string "bug".
	ContentMatch map[string]string `json:"content_match,omitempty"`
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

	// Interactive means this step expects terminal interaction.
	// The executor allocates a PTY and does not capture stdout.
	// The operator interacts via bureau observe (readwrite mode).
	// Only valid with Run.
	Interactive bool `json:"interactive,omitempty"`
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
		"m.room.avatar":             100,
		"m.room.canonical_alias":    100,
		"m.room.encryption":         100,
		"m.room.history_visibility": 100,
		"m.room.join_rules":         100,
		"m.room.name":               100,
		"m.room.power_levels":       100,
		"m.room.server_acl":         100,
		"m.room.tombstone":          100,
		"m.room.topic":              100,
		"m.space.child":             100,
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
// Workspace state events (m.bureau.workspace) are at PL 0 — any room member
// can publish them. Authorization relies on room membership: the room is
// invite-only, and only the daemon (PL 50) or admin can invite. This avoids
// the daemon needing to modify m.room.power_levels, which Continuwuity
// rejects from non-admin senders due to a spec compliance gap (it validates
// ALL fields in the event against the sender's PL, not just changed fields).
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

// ConfigRoomPowerLevels returns power level content for a per-machine config
// room. The admin has power level 100 (can set config and credentials). The
// machine has power level 50, which is sufficient to invite and write layouts
// (PL 0) but insufficient to modify config, credentials, or room metadata
// (PL 100).
//
// IMPORTANT: Do not use this as PowerLevelContentOverride when the machine is
// the room creator. The override is merged into the m.room.power_levels event
// BEFORE the preset's subsequent events (join_rules, history_visibility, etc.)
// are authorized. Since the machine would be PL 50 but state_default is 100,
// the homeserver rejects the preset events as unauthorized. Instead, create
// the room without an override (letting the preset give the creator PL 100),
// then send this as a separate m.room.power_levels state event.
//
// When the admin creates the room (bureau-credentials), the admin gets PL 100
// in the override, which satisfies all event PL requirements — so using this
// as PowerLevelContentOverride is safe in that path.
func ConfigRoomPowerLevels(adminUserID, machineUserID string) map[string]any {
	users := map[string]any{
		adminUserID: 100,
	}
	if machineUserID != "" && machineUserID != adminUserID {
		users[machineUserID] = 50
	}

	events := AdminProtectedEvents()
	events[EventTypeMachineConfig] = 100
	events[EventTypeCredentials] = 100
	events[EventTypeLayout] = 0  // daemon publishes layout state
	events["m.room.message"] = 0 // daemon posts command results and pipeline results

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
	// ticket (in the artifact service or Matrix media repository).
	// The ticket stores references, not content.
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
	// a version bump. Same semantics as ArtifactContent.Extra.
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

	// ContentMatch specifies key-value pairs that must all match
	// in the watched event's content (type "state_event"). Same
	// semantics as StartCondition.ContentMatch.
	ContentMatch map[string]string `json:"content_match,omitempty"`

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
// ticket state event (in the artifact service or Matrix media
// repository). The ticket service stores references, not content.
type TicketAttachment struct {
	// Ref is the artifact reference. Format depends on the storage
	// backend: "art-<hash>" for the artifact service, or
	// "mxc://<server>/<id>" for Matrix media.
	Ref string `json:"ref"`

	// Label is a human-readable description shown in listings
	// (e.g., "stack trace", "screenshot of rendering bug").
	Label string `json:"label,omitempty"`

	// ContentType is the MIME type of the referenced content
	// (e.g., "text/plain", "image/png").
	ContentType string `json:"content_type,omitempty"`
}

// Validate checks that the required ref field is present.
func (a *TicketAttachment) Validate() error {
	if a.Ref == "" {
		return errors.New("attachment: ref is required")
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

// ArtifactContent is the content of an EventTypeArtifact state event.
// Published to #bureau/artifact when an artifact is stored in the
// content-addressable store. Contains the metadata index: BLAKE3 hash,
// content type, size, chunking details, compression algorithm, lifecycle
// policy, and provenance.
//
// The artifact data itself lives in the CAS filesystem
// (/var/bureau/artifact/containers/). This state event is the metadata
// layer — it tells consumers what the artifact is, who created it, how
// it should be cached, and when it expires. The artifact service is the
// primary writer; daemons and CLI read.
//
// Multiple artifact service instances in a fleet operate on the same
// #bureau/artifact room. The Version field and CanModify guard prevent
// silent data loss during rolling upgrades where different instances run
// different code versions. See CanModify for details.
type ArtifactContent struct {
	// Version is the schema version (see ArtifactContentVersion).
	// Code that modifies this event must call CanModify() first; if
	// Version exceeds ArtifactContentVersion, the modification is
	// refused to prevent silent field loss. Readers may process any
	// version (unknown fields are harmlessly ignored by Go's JSON
	// unmarshaler).
	Version int `json:"version"`

	// Hash is the full BLAKE3 file hash (64 hex characters). For
	// artifacts below the chunking threshold (single chunk), this
	// equals the chunk hash. For chunked artifacts, this is the
	// Merkle root over all chunk hashes.
	Hash string `json:"hash"`

	// ContentType is the MIME type (e.g., "text/plain",
	// "image/png", "application/octet-stream").
	ContentType string `json:"content_type"`

	// Filename is the original filename, if known. Informational
	// only — the artifact reference is the canonical identifier.
	Filename string `json:"filename,omitempty"`

	// Size is the uncompressed content size in bytes.
	Size int64 `json:"size"`

	// ChunkCount is the number of content-defined chunks. 1 for
	// small artifacts (below the chunking threshold).
	ChunkCount int `json:"chunk_count"`

	// ContainerCount is the number of containers holding this
	// artifact's chunks. 1 for small artifacts.
	ContainerCount int `json:"container_count"`

	// Compression is the compression algorithm applied to this
	// artifact's chunks within their containers. Values: "none",
	// "lz4", "zstd", "bg4_lz4". Empty means the compression
	// algorithm is not recorded in metadata (check the container's
	// per-chunk compression tags instead).
	Compression string `json:"compression,omitempty"`

	// Source records where externally-imported artifacts came from
	// (e.g., "https://huggingface.co/meta-llama/Llama-3-8B",
	// "s3://bucket/key"). Empty for artifacts produced within
	// Bureau (pipeline outputs, agent logs). Informational — not
	// validated or dereferenced by the artifact service.
	Source string `json:"source,omitempty"`

	// Description is an optional human-readable summary.
	Description string `json:"description,omitempty"`

	// Labels are free-form tags for discovery and lifecycle
	// management (e.g., "ci-output", "model-weights", "ephemeral").
	Labels []string `json:"labels,omitempty"`

	// Visibility controls encryption requirements when artifacts
	// leave the Bureau network. "public" artifacts are not encrypted
	// for external storage. "private" (default when empty) artifacts
	// are encrypted with age before any external transfer.
	Visibility string `json:"visibility,omitempty"`

	// CreatedBy is the Matrix user ID of the uploader
	// (e.g., "@service/artifact/main:bureau.local").
	CreatedBy string `json:"created_by"`

	// CreatedAt is an ISO 8601 timestamp.
	CreatedAt string `json:"created_at"`

	// TTL is an optional time-to-live duration string (e.g., "168h"
	// for 7 days). After CreatedAt + TTL, the artifact is eligible
	// for garbage collection unless pinned or referenced by a tag
	// or ticket attachment.
	TTL string `json:"ttl,omitempty"`

	// CachePolicy controls how aggressively this artifact is cached
	// across the fleet. Values: "default" (normal LRU), "pin" (keep
	// in shared cache), "ephemeral" (local only, lower eviction
	// priority), "replicate" (push to all shared caches). Empty is
	// treated as "default".
	CachePolicy string `json:"cache_policy,omitempty"`

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
// the content is valid.
func (a *ArtifactContent) Validate() error {
	if a.Version < 1 {
		return fmt.Errorf("artifact content: version must be >= 1, got %d", a.Version)
	}
	if a.Hash == "" {
		return errors.New("artifact content: hash is required")
	}
	if len(a.Hash) != 64 {
		return fmt.Errorf("artifact content: hash must be 64 hex characters, got %d", len(a.Hash))
	}
	if _, err := hex.DecodeString(a.Hash); err != nil {
		return fmt.Errorf("artifact content: hash is not valid hex: %w", err)
	}
	if a.ContentType == "" {
		return errors.New("artifact content: content_type is required")
	}
	if a.Size <= 0 {
		return fmt.Errorf("artifact content: size must be positive, got %d", a.Size)
	}
	if a.ChunkCount < 1 {
		return fmt.Errorf("artifact content: chunk_count must be >= 1, got %d", a.ChunkCount)
	}
	if a.ContainerCount < 1 {
		return fmt.Errorf("artifact content: container_count must be >= 1, got %d", a.ContainerCount)
	}
	if a.CreatedBy == "" {
		return errors.New("artifact content: created_by is required")
	}
	if a.CreatedAt == "" {
		return errors.New("artifact content: created_at is required")
	}
	if a.Compression != "" {
		switch a.Compression {
		case "none", "lz4", "zstd", "bg4_lz4":
			// Valid.
		default:
			return fmt.Errorf("artifact content: unknown compression %q", a.Compression)
		}
	}
	if a.Visibility != "" {
		switch a.Visibility {
		case "public", "private":
			// Valid.
		default:
			return fmt.Errorf("artifact content: unknown visibility %q", a.Visibility)
		}
	}
	if a.CachePolicy != "" {
		switch a.CachePolicy {
		case "default", "pin", "ephemeral", "replicate":
			// Valid.
		default:
			return fmt.Errorf("artifact content: unknown cache_policy %q", a.CachePolicy)
		}
	}
	return nil
}

// CanModify checks whether this code version can safely perform a
// read-modify-write cycle on this event. Returns nil if safe, or an
// error explaining why modification would risk data loss.
//
// If the event's Version exceeds ArtifactContentVersion, this code does
// not understand all fields in the event. Marshaling the modified struct
// back to JSON would silently drop the unknown fields. The caller must
// either upgrade the artifact service or refuse the operation.
//
// Read-only access does not require CanModify — unknown fields are
// harmlessly ignored during display, routing, and service discovery.
func (a *ArtifactContent) CanModify() error {
	if a.Version > ArtifactContentVersion {
		return fmt.Errorf(
			"artifact content version %d exceeds supported version %d: "+
				"modification would lose fields added in newer versions; "+
				"upgrade the artifact service before modifying this event",
			a.Version, ArtifactContentVersion,
		)
	}
	return nil
}

// ArtifactTag is the content of an EventTypeArtifactTag state event.
// A named mutable pointer to an artifact, providing stable URIs for
// changing content. Tag names use hierarchical paths (same convention as
// principal localparts): "iree/resnet50/compiled/latest".
//
// Tag updates are compare-and-swap by default: the caller specifies
// PreviousRef (the expected current target), and the update fails if
// another writer changed it concurrently. This prevents lost updates
// without requiring distributed locks. An explicit optimistic mode
// (last-writer-wins) is available for tags where concurrent writes are
// expected and acceptable.
type ArtifactTag struct {
	// Version is the schema version (see ArtifactTagVersion). Same
	// semantics as ArtifactContent.Version — call CanModify() before
	// any read-modify-write cycle.
	Version int `json:"version"`

	// Ref is the artifact reference this tag currently points to
	// (e.g., "art-a3f9b2c1e7d4").
	Ref string `json:"ref"`

	// PreviousRef is the ref this tag pointed to before this update.
	// Enables conflict detection (compare-and-swap) and provides an
	// audit trail of tag history. Empty for the initial tag creation.
	PreviousRef string `json:"previous_ref,omitempty"`

	// UpdatedBy is the Matrix user ID that last updated the tag.
	UpdatedBy string `json:"updated_by"`

	// UpdatedAt is an ISO 8601 timestamp of the last update.
	UpdatedAt string `json:"updated_at"`

	// Extra is a documented extension namespace. Same semantics as
	// ArtifactContent.Extra.
	Extra map[string]json.RawMessage `json:"extra,omitempty"`
}

// Validate checks that all required fields are present and well-formed.
func (a *ArtifactTag) Validate() error {
	if a.Version < 1 {
		return fmt.Errorf("artifact tag: version must be >= 1, got %d", a.Version)
	}
	if a.Ref == "" {
		return errors.New("artifact tag: ref is required")
	}
	if a.UpdatedBy == "" {
		return errors.New("artifact tag: updated_by is required")
	}
	if a.UpdatedAt == "" {
		return errors.New("artifact tag: updated_at is required")
	}
	return nil
}

// CanModify checks whether this code version can safely modify this tag
// event. Same semantics as ArtifactContent.CanModify.
func (a *ArtifactTag) CanModify() error {
	if a.Version > ArtifactTagVersion {
		return fmt.Errorf(
			"artifact tag version %d exceeds supported version %d: "+
				"modification would lose fields added in newer versions; "+
				"upgrade the artifact service before modifying this event",
			a.Version, ArtifactTagVersion,
		)
	}
	return nil
}

// ArtifactRoomPowerLevels returns the power level structure for the
// artifact metadata room (#bureau/artifact). Artifact service principals
// need to write EventTypeArtifact and EventTypeArtifactTag state events
// at PL 0 (they are invited members, not admins). Daemons are also
// members (for reading artifact state via /sync) but do not write
// artifact events.
//
// Room membership is invite-only — the admin invites artifact service
// principals and machine daemons during setup. The PL 0 write access
// for artifact events is safe because only invited members can write,
// and event state keys (artifact references and tag names) are scoped
// per-artifact, so one service cannot overwrite another's metadata.
func ArtifactRoomPowerLevels(adminUserID string) map[string]any {
	events := AdminProtectedEvents()
	events[EventTypeArtifact] = 0
	events[EventTypeArtifactTag] = 0

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
