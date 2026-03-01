// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/ref"
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

// PrincipalRunStatus is the lifecycle state of a principal's sandbox.
// Values are self-describing strings that serialize directly to JSON.
type PrincipalRunStatus string

const (
	// PrincipalRunning means the sandbox is actively consuming CPU
	// (utilization > 1%).
	PrincipalRunning PrincipalRunStatus = "running"

	// PrincipalIdle means the sandbox is running but consuming minimal
	// CPU (utilization <= 1%), with at least one prior reading for
	// baseline comparison.
	PrincipalIdle PrincipalRunStatus = "idle"

	// PrincipalStarting means the sandbox was recently created and no
	// baseline CPU reading exists yet for delta computation.
	PrincipalStarting PrincipalRunStatus = "starting"
)

// IsKnown reports whether s is one of the defined PrincipalRunStatus values.
func (s PrincipalRunStatus) IsKnown() bool {
	switch s {
	case PrincipalRunning, PrincipalIdle, PrincipalStarting:
		return true
	}
	return false
}

// PrincipalResourceUsage reports resource consumption for one sandbox.
// Populated from cgroup v2 statistics by the daemon and included in the
// enriched MachineStatus heartbeat.
type PrincipalResourceUsage struct {
	// CPUPercent is the sandbox CPU utilization (0-100, may exceed 100
	// on multi-core for per-core reporting). Derived from cpu.stat
	// usage_usec delta.
	CPUPercent int `json:"cpu_percent"`

	// MemoryMB is the current memory usage in megabytes. From cgroup
	// memory.current.
	MemoryMB int `json:"memory_mb"`

	// GPUPercent is the sandbox GPU utilization (0-100). Best-effort
	// from per-process NVML stats. Zero when GPU monitoring is
	// unavailable.
	GPUPercent int `json:"gpu_percent,omitempty"`

	// GPUMemoryMB is the sandbox GPU memory usage in megabytes.
	// Best-effort from per-process NVML stats.
	GPUMemoryMB int `json:"gpu_memory_mb,omitempty"`

	// Status is the sandbox lifecycle state derived from cgroup
	// statistics and sandbox lifecycle.
	Status PrincipalRunStatus `json:"status"`
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
// Four persistent/recurring process types are tracked: daemon (runs
// continuously), launcher (runs continuously), proxy (spawned per-sandbox
// by the launcher), and log-relay (wraps each sandbox process, lives for
// the sandbox's lifetime). Other Bureau binaries (bridge, sandbox,
// credentials, proxy-call, observe-relay) are short-lived utilities
// resolved from PATH or the Nix environment at invocation time.
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

	// LogRelayStorePath is the Nix store path containing the
	// bureau-log-relay binary. The launcher uses this path when
	// generating sandbox scripts for new sandbox creation. Existing
	// sandboxes continue running their current log-relay binary until
	// recycled.
	LogRelayStorePath string `json:"log_relay_store_path,omitempty"`

	// HostEnvironmentPath is the Nix store path of the bureau-host-env
	// buildEnv derivation (e.g., "/nix/store/...-bureau-host-env").
	// Its bin/ directory contains symlinks to every Bureau binary. The
	// daemon uses this to resolve service binary commands (bare names
	// like "bureau-ticket-service") from the Nix closure instead of
	// relying on /var/bureau/bin symlinks, enabling fully automated
	// service binary updates without root privileges.
	HostEnvironmentPath string `json:"host_environment_path,omitempty"`
}

// CredentialsVersion is the current schema version for
// Credentials events.
const CredentialsVersion = 1

// Credentials is the content of an EventTypeCredentials state event.
// Contains an age-encrypted credential bundle for a specific principal
// on a specific machine. See CREDENTIALS.md for the full lifecycle.
type Credentials struct {
	// Version is the schema version (see CredentialsVersion).
	Version int `json:"version"`

	// Principal is the full Matrix user ID of the principal these
	// credentials are for (e.g., "@iree/amdgpu/pm:bureau.local").
	Principal ref.UserID `json:"principal"`

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
	ProvisionedBy ref.UserID `json:"provisioned_by"`

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
	// Principal is the service provider entity (e.g., the ref for
	// "@bureau/fleet/prod/service/stt/whisper:bureau.local").
	// Serialized as the Matrix user ID string via MarshalText.
	Principal ref.Entity `json:"principal"`

	// Machine is the machine running this service instance (e.g.,
	// the ref for "@bureau/fleet/prod/machine/gpu-box:bureau.local").
	Machine ref.Machine `json:"machine"`

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

// ServiceBindingContent is the content of an EventTypeServiceBinding state
// event. It binds a service role to a specific service principal in a room.
// The daemon reads these when resolving a template's RequiredServices to
// determine which service sockets to bind-mount into a sandbox.
//
// State key: the service role name (e.g., "ticket", "rag", "ci")
type ServiceBindingContent struct {
	// Principal is the service instance that handles this role in this
	// room (e.g., the ref for
	// "@bureau/fleet/prod/service/ticket:bureau.local").
	Principal ref.Entity `json:"principal"`
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

// SetMachineLabels reads the current MachineInfo state event for a machine,
// replaces its Labels field, and writes the updated event back. This is a
// read-modify-write operation that preserves all other MachineInfo fields
// (hostname, CPU, memory, GPUs, daemon version) published by the daemon.
//
// The roomID is the fleet-scoped machine room (fleet.MachineRoomID) where
// the daemon publishes MachineInfo. The stateKey is the machine's localpart.
func SetMachineLabels(ctx context.Context, session StateSession, roomID ref.RoomID, stateKey string, labels map[string]string) error {
	raw, err := session.GetStateEvent(ctx, roomID, EventTypeMachineInfo, stateKey)
	if err != nil {
		return fmt.Errorf("reading machine info for %s: %w", stateKey, err)
	}

	var info MachineInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return fmt.Errorf("parsing machine info for %s: %w", stateKey, err)
	}

	info.Labels = labels

	if _, err := session.SendStateEvent(ctx, roomID, EventTypeMachineInfo, stateKey, info); err != nil {
		return fmt.Errorf("writing machine info labels for %s: %w", stateKey, err)
	}

	return nil
}
