// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"encoding/json"
	"fmt"
)

// Fleet management event type constants. These events live in the
// #bureau/fleet room and are consumed by the fleet controller service
// and (for HA leases) by every daemon.
const (
	// EventTypeFleetService declares a service that the fleet controller
	// manages. The fleet controller reads these, evaluates placement
	// constraints against available machines, and writes PrincipalAssignment
	// events to the chosen machine config rooms.
	//
	// State key: service localpart (e.g., "service/stt/whisper")
	// Room: #bureau/fleet:<server>
	EventTypeFleetService = "m.bureau.fleet_service"

	// EventTypeMachineDefinition defines a provisionable machine or
	// machine pool. The fleet controller reads these to determine what
	// machines it can bring online when existing capacity is insufficient.
	// Machines that are always on do not need definitions — they register
	// at boot and the fleet controller discovers them via heartbeat.
	//
	// State key: pool name or machine localpart (e.g., "gpu-cloud-pool"
	// or "machine/spare-workstation")
	// Room: #bureau/fleet:<server>
	EventTypeMachineDefinition = "m.bureau.machine_definition"

	// EventTypeFleetConfig defines global settings for a fleet controller.
	// Each fleet controller has its own config keyed by its localpart.
	//
	// State key: fleet controller localpart (e.g., "service/fleet/prod")
	// Room: #bureau/fleet:<server>
	EventTypeFleetConfig = "m.bureau.fleet_config"

	// EventTypeHALease represents a lease held by a machine for hosting
	// a critical service. Daemons compete to acquire this lease when the
	// current holder goes offline. The random-backoff-plus-verify pattern
	// compensates for Matrix's last-writer-wins semantics.
	//
	// State key: service localpart (e.g., "service/fleet/prod")
	// Room: #bureau/fleet:<server>
	EventTypeHALease = "m.bureau.ha_lease"

	// EventTypeServiceStatus contains application-level metrics published
	// by a service through its proxy. The fleet controller uses these for
	// scaling decisions (e.g., high queue depth triggers replica scale-up).
	//
	// State key: service localpart
	// Room: #bureau/service:<server>
	EventTypeServiceStatus = "m.bureau.service_status"

	// EventTypeFleetAlert is published by the fleet controller for events
	// requiring attention: failover alerts, rebalancing proposals, capacity
	// requests, preemption requests. Timeline messages in #bureau/fleet
	// provide the audit trail.
	//
	// State key: alert ID (unique per alert)
	// Room: #bureau/fleet:<server>
	EventTypeFleetAlert = "m.bureau.fleet_alert"
)

// Fleet controller notification msgtype constants. These are posted as
// m.room.message events to the fleet room when the fleet controller's
// internal model changes. External observers (tests, dashboards) match
// on msgtype to synchronize with fleet controller state.
const (
	// MsgTypeFleetConfigRoomDiscovered is posted when the fleet
	// controller joins and processes a machine's config room.
	MsgTypeFleetConfigRoomDiscovered = "m.bureau.fleet_config_room_discovered"

	// MsgTypeFleetServiceDiscovered is posted when the fleet
	// controller processes a new fleet service definition.
	MsgTypeFleetServiceDiscovered = "m.bureau.fleet_service_discovered"
)

// FleetConfigRoomDiscoveredMessage is the content of an m.room.message
// event with msgtype MsgTypeFleetConfigRoomDiscovered. Posted when the
// fleet controller joins a machine's config room and processes its
// MachineConfig state event.
type FleetConfigRoomDiscoveredMessage struct {
	MsgType      string `json:"msgtype"`
	Body         string `json:"body"`
	Machine      string `json:"machine"`
	ConfigRoomID string `json:"config_room_id"`
}

// NewFleetConfigRoomDiscoveredMessage constructs a FleetConfigRoomDiscoveredMessage.
func NewFleetConfigRoomDiscoveredMessage(machine, configRoomID string) FleetConfigRoomDiscoveredMessage {
	return FleetConfigRoomDiscoveredMessage{
		MsgType:      MsgTypeFleetConfigRoomDiscovered,
		Body:         fmt.Sprintf("Config room discovered for %s", machine),
		Machine:      machine,
		ConfigRoomID: configRoomID,
	}
}

// FleetServiceDiscoveredMessage is the content of an m.room.message
// event with msgtype MsgTypeFleetServiceDiscovered. Posted when the
// fleet controller processes a new fleet service definition from the
// fleet room.
type FleetServiceDiscoveredMessage struct {
	MsgType string `json:"msgtype"`
	Body    string `json:"body"`
	Service string `json:"service"`
}

// NewFleetServiceDiscoveredMessage constructs a FleetServiceDiscoveredMessage.
func NewFleetServiceDiscoveredMessage(service string) FleetServiceDiscoveredMessage {
	return FleetServiceDiscoveredMessage{
		MsgType: MsgTypeFleetServiceDiscovered,
		Body:    fmt.Sprintf("Service discovered: %s", service),
		Service: service,
	}
}

// FleetServiceContent defines a service that the fleet controller manages.
// The fleet controller reads these, evaluates placement constraints against
// available machines, and writes PrincipalAssignment events to the chosen
// machine config rooms.
type FleetServiceContent struct {
	// Template is the reference to the sandbox template for this service
	// (e.g., "bureau/template:whisper-stt"). Parsed by ParseTemplateRef.
	Template string `json:"template"`

	// Replicas defines how many instances should be running fleet-wide.
	Replicas ReplicaSpec `json:"replicas"`

	// Resources declares the resource requirements for one instance.
	// Used by the placement algorithm to determine which machines have
	// sufficient capacity.
	Resources ResourceRequirements `json:"resources,omitempty"`

	// Placement defines constraints on which machines can host this
	// service.
	Placement PlacementConstraints `json:"placement"`

	// Failover controls what happens when a machine hosting this service
	// goes offline.
	//
	//   "migrate" — fleet controller re-places on another machine that
	//               satisfies constraints
	//   "alert"   — fleet controller publishes an alert event but does
	//               not auto-move; a human or sysadmin agent decides
	//   "none"    — pinned to this machine, no automatic action
	Failover string `json:"failover"`

	// HAClass marks services that get daemon-level failover independent
	// of the fleet controller. "critical" enables the daemon watchdog
	// protocol. Empty means normal fleet-controller-managed failover.
	HAClass string `json:"ha_class,omitempty"`

	// ServiceRooms is a list of room alias glob patterns. The fleet
	// controller ensures the service is bound (via m.bureau.room_service
	// state events) in rooms matching these patterns.
	ServiceRooms []string `json:"service_rooms,omitempty"`

	// Priority determines placement order when multiple services compete
	// for the same resources. Lower numbers are higher priority.
	// 0 = critical infrastructure, 10 = production, 50 = development,
	// 100 = batch/background.
	Priority int `json:"priority"`

	// Scheduling controls when this service should run. Nil means the
	// service should always be running.
	Scheduling *SchedulingSpec `json:"scheduling,omitempty"`

	// Fleet is the localpart of the fleet controller that manages this
	// service. If empty, any fleet controller may manage it. If set,
	// only the named controller manages this service.
	Fleet string `json:"fleet,omitempty"`

	// Payload is per-service payload configuration passed to the template.
	// Merged with the PrincipalAssignment payload that the fleet controller
	// generates.
	Payload json.RawMessage `json:"payload,omitempty"`

	// MatrixPolicy controls which self-service Matrix operations principals
	// created from this fleet service can perform (join rooms, invite
	// others, create rooms). Propagated to each PrincipalAssignment that
	// the fleet controller or HA watchdog creates. When nil, the principal
	// cannot change its own room membership (default-deny).
	//
	// Ignored when Authorization is set — the full authorization policy
	// takes precedence, matching PrincipalAssignment semantics.
	MatrixPolicy *MatrixPolicy `json:"matrix_policy,omitempty"`

	// ServiceVisibility is a list of glob patterns controlling which
	// services principals created from this fleet service can discover
	// via GET /v1/services. Propagated to each PrincipalAssignment.
	// An empty or nil list means the principal cannot see any services
	// (default-deny).
	//
	// Ignored when Authorization is set.
	ServiceVisibility []string `json:"service_visibility,omitempty"`

	// Authorization is the full authorization policy for principals
	// created from this fleet service. Propagated to each
	// PrincipalAssignment. When set, the daemon uses it for all
	// authorization decisions and ignores MatrixPolicy and
	// ServiceVisibility (matching PrincipalAssignment semantics).
	Authorization *AuthorizationPolicy `json:"authorization,omitempty"`

	// ManagedBy is set by the fleet controller when it claims management
	// of this service definition. Contains the fleet controller's
	// localpart. Read-only from the user's perspective — the fleet
	// controller writes this when it starts managing the service.
	ManagedBy string `json:"managed_by,omitempty"`
}

// ReplicaSpec defines the replica count bounds for a fleet service.
type ReplicaSpec struct {
	// Min is the minimum number of running instances.
	Min int `json:"min"`

	// Max is the maximum number of instances for auto-scaling.
	// If 0, defaults to Min (no auto-scaling).
	Max int `json:"max,omitempty"`
}

// ResourceRequirements declares what one instance of the service needs.
// The fleet controller uses these to determine which machines have
// sufficient capacity for placement.
//
// All values use integer units compatible with Matrix canonical JSON
// (which forbids fractional numbers). Memory is in megabytes, CPU is
// in millicores (1000 = one full core).
type ResourceRequirements struct {
	// MemoryMB is the expected memory consumption in megabytes.
	MemoryMB int `json:"memory_mb,omitempty"`

	// CPUMillicores is the expected CPU usage in millicores (1000 = one
	// core). Fractional cores are expressed naturally: 500 = half a core,
	// 2500 = two and a half cores.
	CPUMillicores int `json:"cpu_millicores,omitempty"`

	// GPU is true if the service requires a GPU.
	GPU bool `json:"gpu,omitempty"`

	// GPUMemoryMB is the expected GPU memory consumption in megabytes.
	// Zero means no GPU memory requirement (the service may still
	// require a GPU for compute without reserving specific VRAM).
	GPUMemoryMB int `json:"gpu_memory_mb,omitempty"`
}

// PlacementConstraints defines where a service can run.
type PlacementConstraints struct {
	// Requires is a list of labels that a machine must have for this
	// service to be placed on it. All labels must be present (AND
	// semantics). Label format is "key=value" or just "key" (presence
	// check without value matching).
	Requires []string `json:"requires,omitempty"`

	// PreferredMachines is an ordered list of machine localparts to
	// prefer for placement. The fleet controller tries these first
	// (in order) before scoring other candidates.
	PreferredMachines []string `json:"preferred_machines,omitempty"`

	// AllowedMachines is a list of glob patterns for which machines
	// are candidates. If empty, all machines are candidates (subject
	// to Requires).
	AllowedMachines []string `json:"allowed_machines,omitempty"`

	// AntiAffinity is a list of fleet service state keys. The fleet
	// controller avoids placing this service on a machine that already
	// hosts any of these services. Use for spreading replicas or
	// avoiding GPU contention between large models.
	AntiAffinity []string `json:"anti_affinity,omitempty"`

	// CoLocateWith is a list of fleet service state keys. The fleet
	// controller prefers placing this service on a machine that already
	// hosts these services. Use for locality-sensitive service pairs
	// (e.g., an agent and the RAG service it queries frequently).
	CoLocateWith []string `json:"co_locate_with,omitempty"`
}

// SchedulingSpec controls when a service should run. Used for batch
// workloads, training jobs, and cost-sensitive tasks.
type SchedulingSpec struct {
	// Class categorizes the scheduling behavior.
	//
	//   "always"    — run continuously (default if Scheduling is nil)
	//   "batch"     — can be deferred to preferred windows, can be
	//                 preempted by higher-priority work
	//   "triggered" — only runs in response to an event (uses
	//                 StartCondition on the generated PrincipalAssignment)
	Class string `json:"class"`

	// Preemptible means higher-priority services can displace this one.
	// The fleet controller sends CheckpointSignal, waits
	// DrainGraceSeconds, then removes the PrincipalAssignment.
	Preemptible bool `json:"preemptible,omitempty"`

	// PreferredWindows defines time windows when this service should
	// preferentially run. The fleet controller scores placement higher
	// during these windows. Outside these windows, the service may still
	// run if resources are available and no higher-priority work needs
	// them.
	PreferredWindows []TimeWindow `json:"preferred_windows,omitempty"`

	// CheckpointSignal is the Unix signal name to send to the service
	// process before preemption or drain (e.g., "SIGUSR1"). The service
	// should save its state and prepare for shutdown.
	CheckpointSignal string `json:"checkpoint_signal,omitempty"`

	// DrainGraceSeconds is how long to wait after sending
	// CheckpointSignal before force-stopping the service.
	DrainGraceSeconds int `json:"drain_grace_seconds,omitempty"`
}

// TimeWindow defines a recurring time window for batch scheduling.
type TimeWindow struct {
	// Days is a list of day abbreviations: "mon", "tue", "wed", "thu",
	// "fri", "sat", "sun". If empty, applies to all days.
	Days []string `json:"days,omitempty"`

	// StartHour is the start of the window (0-23, local time of the
	// machine).
	StartHour int `json:"start_hour"`

	// EndHour is the end of the window (0-23, local time of the
	// machine). If EndHour <= StartHour, the window wraps midnight
	// (e.g., 22-06 means 10 PM to 6 AM).
	EndHour int `json:"end_hour"`
}

// MachineDefinitionContent defines a provisionable machine or machine pool.
// The fleet controller reads these to determine what machines it can bring
// online when existing capacity is insufficient.
//
// For cloud providers, one definition represents a pool of identical
// instances (min/max scaling). For local/WoL machines, one definition
// represents one physical machine.
type MachineDefinitionContent struct {
	// Provider identifies how this machine is provisioned.
	//
	//   "local"  — physical machine on the local network, woken via
	//              wake-on-LAN or powered on manually
	//   "gcloud" — Google Cloud Compute Engine instance
	//   "aws"    — Amazon EC2 instance
	//   "azure"  — Azure Virtual Machine
	//   "manual" — provisioned by a human or sysadmin agent (fleet
	//              controller publishes a capacity request)
	Provider string `json:"provider"`

	// Labels are the labels that instances from this definition will
	// have when they register. The fleet controller uses these to
	// match machine definitions to service placement constraints
	// before the machine is even online.
	Labels map[string]string `json:"labels,omitempty"`

	// Resources describes the capacity of one instance from this
	// definition. Used by the placement algorithm to predict whether
	// a provisioned machine will satisfy a pending service's
	// requirements.
	Resources MachineResources `json:"resources"`

	// Provisioning contains provider-specific configuration for
	// creating or waking instances.
	Provisioning ProvisioningConfig `json:"provisioning"`

	// Scaling defines how many instances of this definition can exist.
	// For local/WoL machines, Min and Max are both 0 or 1. For cloud
	// pools, this controls how many instances the fleet controller can
	// create.
	Scaling ScalingConfig `json:"scaling"`

	// Lifecycle controls when machines are provisioned and
	// deprovisioned.
	Lifecycle MachineLifecycleConfig `json:"lifecycle"`
}

// MachineResources describes the capacity of a machine.
// All values use integer units compatible with Matrix canonical JSON.
type MachineResources struct {
	// CPUCores is the number of physical CPU cores.
	CPUCores int `json:"cpu_cores,omitempty"`

	// MemoryMB is the total RAM in megabytes.
	MemoryMB int `json:"memory_mb,omitempty"`

	// GPU is the GPU model identifier (e.g., "rtx4090", "h100", "mi300x").
	// Empty if no GPU.
	GPU string `json:"gpu,omitempty"`

	// GPUCount is the number of GPUs.
	GPUCount int `json:"gpu_count,omitempty"`

	// GPUMemoryMB is the total GPU memory in megabytes (per GPU).
	GPUMemoryMB int `json:"gpu_memory_mb,omitempty"`

	// DiskMB is the available disk space in megabytes.
	DiskMB int `json:"disk_mb,omitempty"`
}

// ProvisioningConfig contains provider-specific settings for machine
// provisioning. Not all fields apply to every provider.
type ProvisioningConfig struct {
	// InstanceType is the cloud instance type (e.g., "a3-highgpu-1g",
	// "p4d.24xlarge"). Used by gcloud, aws, and azure providers.
	InstanceType string `json:"instance_type,omitempty"`

	// Region is the cloud region (e.g., "us-central1", "us-east-1").
	Region string `json:"region,omitempty"`

	// Zone is the cloud availability zone within the region.
	Zone string `json:"zone,omitempty"`

	// BootImage is the machine image for cloud provisioning (e.g.,
	// "bureau-runner-v1").
	BootImage string `json:"boot_image,omitempty"`

	// StartupScript is a script to execute on instance boot for cloud
	// provisioning. Typically starts the Bureau daemon.
	StartupScript string `json:"startup_script,omitempty"`

	// CredentialName references a credential in the proxy's external
	// credential set. The fleet controller calls cloud APIs through
	// the proxy, which injects the credential. The fleet controller
	// never sees the raw API key.
	CredentialName string `json:"credential_name,omitempty"`

	// MACAddress is the MAC address for wake-on-LAN. Used by the
	// local provider.
	MACAddress string `json:"mac_address,omitempty"`

	// IPAddress is the network address for the local machine. Used
	// for SSH-based suspend/wake.
	IPAddress string `json:"ip_address,omitempty"`

	// CostPerHourMilliUSD is the estimated hourly cost in thousandths
	// of a US dollar (e.g., 3500 = $3.50/hr). Integer for Matrix
	// canonical JSON compatibility. Used by the fleet controller for
	// cost-aware placement decisions and by batch scheduling to respect
	// cost budgets.
	CostPerHourMilliUSD int `json:"cost_per_hour_milliusd,omitempty"`
}

// ScalingConfig controls how many instances of a machine definition
// can exist simultaneously.
type ScalingConfig struct {
	// MinInstances is the minimum number of instances to keep running.
	MinInstances int `json:"min_instances"`

	// MaxInstances is the maximum number of instances the fleet
	// controller can create.
	MaxInstances int `json:"max_instances"`
}

// MachineLifecycleConfig controls when machines are provisioned and
// deprovisioned.
type MachineLifecycleConfig struct {
	// ProvisionOn controls when the fleet controller brings this
	// machine online.
	//
	//   "demand"  — provision when a service needs placement and no
	//               existing machine satisfies constraints
	//   "always"  — keep MinInstances running at all times
	//   "manual"  — never auto-provision; fleet controller publishes
	//               a capacity request and waits for a human or
	//               sysadmin agent
	ProvisionOn string `json:"provision_on"`

	// DeprovisionOn controls when the fleet controller takes this
	// machine offline.
	//
	//   "idle"   — deprovision after IdleTimeoutSeconds with no
	//              fleet-managed services running
	//   "manual" — never auto-deprovision
	//   "never"  — machine stays on (same as omitting lifecycle)
	DeprovisionOn string `json:"deprovision_on,omitempty"`

	// IdleTimeoutSeconds is how long a machine must be idle before
	// the fleet controller deprovisions it. Only applies when
	// DeprovisionOn is "idle".
	IdleTimeoutSeconds int `json:"idle_timeout_seconds,omitempty"`

	// WakeMethod is how to bring a local machine online.
	//
	//   "wol"    — wake-on-LAN magic packet
	//   "ssh"    — SSH command (machine is suspended, not off)
	//   "api"    — cloud provider API call
	//   "manual" — human intervention required
	WakeMethod string `json:"wake_method,omitempty"`

	// SuspendMethod is how to put a local machine to sleep.
	//
	//   "ssh_command" — send a command via SSH or Matrix command
	//   "api"        — cloud provider API call (terminate/stop)
	//   "manual"     — human intervention required
	SuspendMethod string `json:"suspend_method,omitempty"`

	// SuspendCommand is the command to execute for ssh_command suspend
	// method (e.g., "systemctl suspend").
	SuspendCommand string `json:"suspend_command,omitempty"`

	// WakeLatencySeconds is the estimated time from wake signal to
	// machine registration (heartbeat in #bureau/machine). The fleet
	// controller uses this when deciding whether to wake a machine
	// or wait for an existing one.
	WakeLatencySeconds int `json:"wake_latency_seconds,omitempty"`
}

// FleetConfigContent defines global settings for a fleet controller.
type FleetConfigContent struct {
	// RebalancePolicy controls how the fleet controller responds to
	// resource pressure.
	//
	//   "auto"  — fleet controller moves services automatically when
	//             pressure thresholds are exceeded
	//   "alert" — fleet controller publishes an alert but waits for
	//             human or sysadmin agent approval before moving
	RebalancePolicy string `json:"rebalance_policy"`

	// PressureThresholdCPU is the sustained CPU utilization percentage
	// that triggers rebalancing evaluation. Default: 85.
	PressureThresholdCPU int `json:"pressure_threshold_cpu,omitempty"`

	// PressureThresholdMemory is the sustained memory utilization
	// percentage that triggers rebalancing evaluation. Default: 90.
	PressureThresholdMemory int `json:"pressure_threshold_memory,omitempty"`

	// PressureThresholdGPU is the sustained GPU utilization percentage
	// that triggers rebalancing evaluation. Default: 95.
	PressureThresholdGPU int `json:"pressure_threshold_gpu,omitempty"`

	// PressureSustainedSeconds is how long a machine must exceed a
	// pressure threshold before the fleet controller acts. Prevents
	// rebalancing on transient spikes. Default: 300 (5 minutes).
	PressureSustainedSeconds int `json:"pressure_sustained_seconds,omitempty"`

	// RebalanceCooldownSeconds prevents a service from being moved
	// again within this window after a placement. Default: 600
	// (10 minutes).
	RebalanceCooldownSeconds int `json:"rebalance_cooldown_seconds,omitempty"`

	// HeartbeatIntervalSeconds is the expected interval between
	// machine status heartbeats. The fleet controller considers a
	// machine offline if no heartbeat arrives within 3x this interval.
	// Default: 30.
	HeartbeatIntervalSeconds int `json:"heartbeat_interval_seconds,omitempty"`

	// PreemptibleBy lists fleet controller localparts whose services
	// can preempt this fleet's services. A prod fleet controller
	// listing nothing means its services are never preempted. A
	// sandbox fleet listing "service/fleet/prod" means prod can
	// preempt sandbox services.
	PreemptibleBy []string `json:"preemptible_by,omitempty"`
}

// HALeaseContent represents a lease held by a machine for hosting a
// critical service. Daemons compete to acquire this lease when the
// current holder goes offline.
type HALeaseContent struct {
	// Holder is the machine localpart currently holding the lease
	// (e.g., "machine/workstation").
	Holder string `json:"holder"`

	// ExpiresAt is an ISO 8601 timestamp. If the holder does not
	// renew the lease before this time, other daemons may claim it.
	ExpiresAt string `json:"expires_at"`

	// Service is the fleet service localpart this lease covers.
	Service string `json:"service"`

	// AcquiredAt is when the current holder acquired the lease.
	AcquiredAt string `json:"acquired_at"`
}

// ServiceStatusContent contains application-level metrics published
// by a service. The fleet controller uses these for scaling decisions
// (e.g., high queue depth triggers replica scale-up).
type ServiceStatusContent struct {
	// Machine is the machine localpart where this instance is running.
	Machine string `json:"machine"`

	// QueueDepth is the number of pending requests or tasks.
	QueueDepth int `json:"queue_depth,omitempty"`

	// AvgLatencyMS is the average request latency in milliseconds.
	AvgLatencyMS int `json:"avg_latency_ms,omitempty"`

	// RequestsPerMinute is the current request throughput.
	RequestsPerMinute int `json:"requests_per_minute,omitempty"`

	// ErrorRatePercent is the error rate as an integer percentage (0-100).
	ErrorRatePercent int `json:"error_rate_percent,omitempty"`

	// Healthy indicates whether the service considers itself healthy.
	Healthy bool `json:"healthy"`
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

	// Status is the sandbox lifecycle state: "running", "idle", or
	// "starting". Derived from cgroup state and sandbox lifecycle.
	Status string `json:"status"`
}

// FleetAlertContent is the content of an EventTypeFleetAlert event.
// Published by the fleet controller for events requiring attention.
type FleetAlertContent struct {
	// AlertType categorizes the alert.
	//
	//   "failover"           — machine down, service needs re-placement
	//   "rebalance_proposal" — pressure detected, rebalancing proposed
	//   "capacity_request"   — no machine can satisfy placement constraints
	//   "preemption_request" — higher-priority fleet needs resources
	//   "preemption_ack"     — lower-priority fleet acknowledged preemption
	//   "rollback"           — health check failure triggered rollback
	AlertType string `json:"alert_type"`

	// Fleet is the localpart of the fleet controller that published
	// this alert.
	Fleet string `json:"fleet"`

	// Service is the affected fleet service localpart (if applicable).
	Service string `json:"service,omitempty"`

	// Machine is the affected machine localpart (if applicable).
	Machine string `json:"machine,omitempty"`

	// Message is a human-readable description of the alert.
	Message string `json:"message"`

	// ProposedActions is a list of actions the fleet controller would
	// take (for alert-mode events that require approval).
	ProposedActions []ProposedAction `json:"proposed_actions,omitempty"`
}

// ProposedAction describes one action in a fleet alert that requires
// approval (e.g., a rebalancing move or failover placement).
type ProposedAction struct {
	// Action is the operation type: "place", "remove", "move", "provision".
	Action string `json:"action"`

	// Service is the target fleet service localpart.
	Service string `json:"service"`

	// FromMachine is the source machine (for "move" and "remove").
	FromMachine string `json:"from_machine,omitempty"`

	// ToMachine is the destination machine (for "place" and "move").
	ToMachine string `json:"to_machine,omitempty"`

	// Score is the placement score for the target machine (for "place"
	// and "move").
	Score int `json:"score,omitempty"`
}

// MachineRoomPowerLevels returns the power level content for machine
// presence rooms (both fleet-scoped and global). Members at power level 0
// can publish machine keys, hardware info, status heartbeats, and WebRTC
// signaling. Administrative room events remain locked to admin (PL 100).
func MachineRoomPowerLevels(adminUserID string) map[string]any {
	events := AdminProtectedEvents()
	for _, eventType := range []string{
		EventTypeMachineKey,
		EventTypeMachineInfo,
		EventTypeMachineStatus,
		EventTypeWebRTCOffer,
		EventTypeWebRTCAnswer,
	} {
		events[eventType] = 0
	}

	return map[string]any{
		"users": map[string]any{
			adminUserID: 100,
		},
		"users_default":  0,
		"events":         events,
		"events_default": 0,
		"state_default":  100,
		"ban":            100,
		"kick":           100,
		"invite":         100,
		"redact":         50,
		"notifications":  map[string]any{"room": 50},
	}
}

// ServiceRoomPowerLevels returns the power level content for service
// directory rooms (both fleet-scoped and global). Members at power level
// 0 can register and deregister services. Administrative room events
// remain locked to admin (PL 100).
func ServiceRoomPowerLevels(adminUserID string) map[string]any {
	events := AdminProtectedEvents()
	events[EventTypeService] = 0

	return map[string]any{
		"users": map[string]any{
			adminUserID: 100,
		},
		"users_default":  0,
		"events":         events,
		"events_default": 0,
		"state_default":  100,
		"ban":            100,
		"kick":           100,
		"invite":         100,
		"redact":         50,
		"notifications":  map[string]any{"room": 50},
	}
}

// FleetRoomPowerLevels returns the power level content for fleet rooms.
// Fleet rooms are admin-controlled but allow all members (machines, fleet
// controllers) to write fleet-specific state events: fleet services,
// machine definitions, HA leases, fleet config, service status, and
// fleet alerts. Administrative room events (name, join rules, power
// levels) remain locked to admin (PL 100).
func FleetRoomPowerLevels(adminUserID string) map[string]any {
	events := AdminProtectedEvents()
	for _, eventType := range []string{
		EventTypeFleetService,
		EventTypeMachineDefinition,
		EventTypeFleetConfig,
		EventTypeHALease,
		EventTypeServiceStatus,
		EventTypeFleetAlert,
	} {
		events[eventType] = 0
	}

	return map[string]any{
		"users": map[string]any{
			adminUserID: 100,
		},
		"users_default":  0,
		"events":         events,
		"events_default": 0,
		"state_default":  100,
		"ban":            100,
		"kick":           100,
		"invite":         100,
		"redact":         50,
		"notifications":  map[string]any{"room": 50},
	}
}
