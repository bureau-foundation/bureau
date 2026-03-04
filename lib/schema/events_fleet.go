// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"github.com/bureau-foundation/bureau/lib/ref"
)

// Fleet management event type constants. These events live in the
// #bureau/fleet room and are consumed by the fleet controller service
// and (for HA leases) by every daemon.
//
// Power level policy: runtime operational events (HA leases, service
// status, fleet alerts) are PL 0 — machines and fleet controllers
// write these during normal operation. Administrative configuration
// events (service definitions, machine definitions, fleet controller
// config, binary cache) are PL 100 — only operators write these
// through the CLI, and a compromised member rewriting them could
// disrupt placement, controller behavior, or binary provenance.
const (
	// EventTypeFleetService declares a service that the fleet controller
	// manages. The fleet controller reads these, evaluates placement
	// constraints against available machines, and writes PrincipalAssignment
	// events to the chosen machine config rooms.
	//
	// Admin-only (PL 100): a compromised member injecting phantom service
	// definitions could cause the fleet controller to attempt placements
	// for nonexistent services, wasting resources and potentially
	// displacing real services.
	//
	// State key: service localpart (e.g., "service/stt/whisper")
	// Room: #bureau/fleet:<server>
	EventTypeFleetService ref.EventType = "m.bureau.fleet_service"

	// EventTypeMachineDefinition defines a provisionable machine or
	// machine pool. The fleet controller reads these to determine what
	// machines it can bring online when existing capacity is insufficient.
	// Machines that are always on do not need definitions — they register
	// at boot and the fleet controller discovers them via heartbeat.
	//
	// Admin-only (PL 100): a compromised member injecting phantom machine
	// definitions could pollute the fleet controller's capacity model.
	//
	// State key: pool name or machine localpart (e.g., "gpu-cloud-pool"
	// or "machine/spare-workstation")
	// Room: #bureau/fleet:<server>
	EventTypeMachineDefinition ref.EventType = "m.bureau.machine_definition"

	// EventTypeFleetConfig defines global settings for a fleet controller.
	// Each fleet controller has its own config keyed by its localpart.
	//
	// Admin-only (PL 100): controls rebalance policy, pressure thresholds,
	// heartbeat interval, and preemption rules. A compromised member
	// rewriting these could trigger failover storms, constant rebalancing,
	// or unauthorized preemption.
	//
	// State key: fleet controller localpart (e.g., "service/fleet/prod")
	// Room: #bureau/fleet:<server>
	EventTypeFleetConfig ref.EventType = "m.bureau.fleet_config"

	// EventTypeHALease represents a lease held by a machine for hosting
	// a critical service. Daemons compete to acquire this lease when the
	// current holder goes offline. The random-backoff-plus-verify pattern
	// compensates for Matrix's last-writer-wins semantics.
	//
	// State key: service localpart (e.g., "service/fleet/prod")
	// Room: #bureau/fleet:<server>
	EventTypeHALease ref.EventType = "m.bureau.ha_lease"

	// EventTypeServiceStatus contains application-level metrics published
	// by a service through its proxy. The fleet controller uses these for
	// scaling decisions (e.g., high queue depth triggers replica scale-up).
	//
	// State key: service localpart
	// Room: #bureau/service:<server>
	EventTypeServiceStatus ref.EventType = "m.bureau.service_status"

	// EventTypeFleetAlert is published by the fleet controller for events
	// requiring attention: failover alerts, rebalancing proposals, capacity
	// requests, preemption requests. Timeline messages in #bureau/fleet
	// provide the audit trail.
	//
	// State key: alert ID (unique per alert)
	// Room: #bureau/fleet:<server>
	EventTypeFleetAlert ref.EventType = "m.bureau.fleet_alert"

	// EventTypeFleetCache declares the Nix binary cache that a fleet uses
	// for store path distribution. Machines configure their nix.conf to
	// trust this cache's signing keys and pull from its URL. Compose
	// pipelines push built closures to this cache by name.
	//
	// Admin-only (PL 100): controls binary provenance for every machine
	// in the fleet. A malicious rewrite — replacing the substituter URL
	// and signing keys — is a supply chain compromise affecting all
	// machines, services, and sandboxes.
	//
	// State key: "" (singleton per fleet)
	// Room: #<ns>/fleet/<name>:<server>
	EventTypeFleetCache ref.EventType = "m.bureau.fleet_cache"
)

// MachineRoomPowerLevels returns the power level content for machine
// presence rooms (both fleet-scoped and global). Members at power level 0
// can publish machine keys, hardware info, status heartbeats, and WebRTC
// signaling. Administrative room events remain locked to admin (PL 100).
func MachineRoomPowerLevels(adminUserID ref.UserID) map[string]any {
	events := AdminProtectedEvents()
	for _, eventType := range []ref.EventType{
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
			adminUserID.String(): 100,
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
// 0 can register and deregister services. The invite threshold is 50
// so that machine daemons (granted PL 50 during provisioning) can
// invite service principals for HA failover and fleet placement.
// Administrative room events remain locked to admin (PL 100).
func ServiceRoomPowerLevels(adminUserID ref.UserID) map[string]any {
	events := AdminProtectedEvents()
	events[EventTypeService] = 0

	return map[string]any{
		"users": map[string]any{
			adminUserID.String(): 100,
		},
		"users_default":  0,
		"events":         events,
		"events_default": 0,
		"state_default":  100,
		"ban":            100,
		"kick":           100,
		"invite":         50,
		"redact":         50,
		"notifications":  map[string]any{"room": 50},
	}
}

// FleetRoomPowerLevels returns the power level content for fleet rooms.
// Fleet rooms are admin-controlled. Members (machines, fleet controllers)
// can write runtime operational events: HA leases, service status, and
// fleet alerts. Administrative configuration — service definitions,
// machine definitions, fleet controller config, and binary cache
// config — is restricted to admin (PL 100) because these events are
// only written by operators through the CLI and a compromised member
// rewriting them could disrupt placement, failover, or binary provenance.
func FleetRoomPowerLevels(adminUserID ref.UserID) map[string]any {
	events := AdminProtectedEvents()

	// Operational state: machines and fleet controllers write these
	// during normal fleet operation. HA leases are written by daemons
	// competing for service ownership, service status is published by
	// running services, and fleet alerts are emitted by the fleet
	// controller.
	for _, eventType := range []ref.EventType{
		EventTypeHALease,
		EventTypeServiceStatus,
		EventTypeFleetAlert,
	} {
		events[eventType] = 0
	}

	// Administrative configuration: admin-only (PL 100). These events
	// are written by operators through the CLI, not by runtime
	// components. A compromised member rewriting them could inject
	// phantom services or machines into the fleet controller's model
	// (FleetService, MachineDefinition), manipulate controller behavior
	// parameters like rebalance policy and heartbeat thresholds
	// (FleetConfig), or compromise binary provenance (FleetCache).
	for _, eventType := range []ref.EventType{
		EventTypeFleetService,
		EventTypeMachineDefinition,
		EventTypeFleetConfig,
		EventTypeFleetCache,
	} {
		events[eventType] = 100
	}

	return map[string]any{
		"users": map[string]any{
			adminUserID.String(): 100,
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

// FleetCacheContent declares the Nix binary cache that a fleet uses for
// store path distribution. Machines configure their nix.conf to trust this
// cache's signing keys and pull from its URL. Compose pipelines push built
// closures to this cache by name.
//
// This is admin-only (PL 100 in FleetRoomPowerLevels) because it controls
// binary provenance for every machine in the fleet. A malicious rewrite —
// replacing the substituter URL and signing keys — is a supply chain
// compromise affecting all machines, services, and sandboxes.
//
// This type lives in the root schema package (rather than lib/schema/fleet)
// because DaemonStatus references it, and lib/schema/fleet imports
// lib/schema — placing it there would create a circular dependency.
type FleetCacheContent struct {
	// URL is the substituter URL that machines use to pull closures.
	// This is the URL that appears in nix.conf substituters or
	// extra-substituters. Examples:
	//   "https://cache.infra.bureau.foundation"
	//   "http://attic.internal:5580/main"
	URL string `json:"url"`

	// Name is the Attic cache name used for push operations
	// (`attic push <name> <store-path>`). Empty if the cache backend
	// does not support named push (e.g., a static S3 bucket populated
	// by CI with `nix copy`).
	Name string `json:"name,omitempty"`

	// PublicKeys lists the Nix signing public keys for this cache.
	// Each key uses the standard Nix format: "name:base64-ed25519-key".
	// Machines add these to trusted-public-keys in nix.conf. Multiple
	// keys support rotation: publish the new key, wait for propagation
	// via `bureau machine doctor --fix`, then retire the old key.
	PublicKeys []string `json:"public_keys"`
}
