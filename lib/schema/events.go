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

	// TransportAddress is the address where this machine's daemon accepts
	// transport connections from peer daemons for cross-machine service
	// routing (e.g., "192.168.1.10:7891" for TCP transport). Peer daemons
	// read this field to discover how to reach this machine.
	// Empty if the daemon is not accepting inbound transport connections
	// (local-only mode).
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
