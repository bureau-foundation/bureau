// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

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

	// EventTypeMachineInfo is published to #bureau/machines when a
	// machine's daemon starts (or when hardware configuration changes).
	// Contains static system inventory: CPU topology, total memory,
	// GPU hardware, board identity. Unlike MachineStatus (periodic
	// heartbeat with changing values), MachineInfo is published once
	// and only updated if the hardware inventory changes.
	//
	// State key: machine localpart (e.g., "machine/workstation")
	// Room: #bureau/machines:<server>
	EventTypeMachineInfo = "m.bureau.machine_info"

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

	// EventTypeWorkspaceReady is published by the setup principal after
	// it finishes creating the workspace (clone, worktrees, config files).
	// Other principals use this as a gate via StartCondition — they don't
	// start until this event exists in the workspace room.
	//
	// State key: "" (singleton per room)
	// Room: the workspace room
	EventTypeWorkspaceReady = "m.bureau.workspace.ready"

	// EventTypePipeline defines a pipeline — a structured sequence of
	// steps that run inside a Bureau sandbox. Pipelines are stored as
	// state events in pipeline rooms (e.g., #bureau/pipeline for
	// built-ins, #iree/pipeline for project pipelines). Room power
	// levels control who can edit pipelines.
	//
	// State key: pipeline name (e.g., "dev-workspace-init", "dev-worktree-teardown")
	// Room: pipeline room (e.g., #bureau/pipeline:<server>)
	EventTypePipeline = "m.bureau.pipeline"
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
// Published once at daemon startup to #bureau/machines. Contains static
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

// PipelineStep is a single step in a pipeline. Exactly one of Run or
// Publish must be set — Run for shell commands, Publish for Matrix
// state events.
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
	// socket directly. Mutually exclusive with Run.
	Publish *PipelinePublish `json:"publish,omitempty"`

	// Timeout is the maximum duration for this step (e.g., "5m",
	// "30s", "1h"). Parsed by time.ParseDuration. The executor
	// kills the step if it exceeds this duration. When empty,
	// defaults to 5 minutes at runtime.
	Timeout string `json:"timeout,omitempty"`

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
	// "m.bureau.workspace.ready").
	EventType string `json:"event_type"`

	// Room is the target room alias or ID. Supports variable
	// substitution (e.g., "${WORKSPACE_ROOM_ID}").
	Room string `json:"room"`

	// StateKey is the state key for the event. Empty string is
	// valid (singleton events like workspace.ready).
	StateKey string `json:"state_key,omitempty"`

	// Content is the event content as a JSON-compatible map.
	// String values support variable substitution.
	Content map[string]any `json:"content"`
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
// WorkspaceRoomPowerLevels returns the power level structure for workspace
// collaboration rooms. Unlike config rooms (events_default: 100, admin-only),
// workspace rooms are collaboration spaces where agents send messages freely.
//
// Three tiers:
//   - Admin (100): project config, teardown, room metadata, power levels
//   - Machine/daemon (50): workspace.ready, layout, invite
//   - Default (0): messages, read state
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

	return map[string]any{
		"users":         users,
		"users_default": 0,
		"events": map[string]any{
			EventTypeProject:            100,
			EventTypeWorkspaceReady:     50,
			EventTypeLayout:             0,
			"m.room.name":               100,
			"m.room.topic":              100,
			"m.room.avatar":             100,
			"m.room.canonical_alias":    100,
			"m.room.history_visibility": 100,
			"m.room.power_levels":       100,
			"m.room.join_rules":         100,
		},
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

func ConfigRoomPowerLevels(adminUserID, machineUserID string) map[string]any {
	users := map[string]any{
		adminUserID: 100,
	}
	if machineUserID != "" && machineUserID != adminUserID {
		users[machineUserID] = 50
	}

	return map[string]any{
		"users":         users,
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
	return map[string]any{
		"users": map[string]any{
			adminUserID: 100,
		},
		"users_default": 0,
		"events": map[string]any{
			EventTypePipeline:           100,
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
