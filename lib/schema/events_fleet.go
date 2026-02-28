// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"github.com/bureau-foundation/bureau/lib/ref"
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
	EventTypeFleetService ref.EventType = "m.bureau.fleet_service"

	// EventTypeMachineDefinition defines a provisionable machine or
	// machine pool. The fleet controller reads these to determine what
	// machines it can bring online when existing capacity is insufficient.
	// Machines that are always on do not need definitions — they register
	// at boot and the fleet controller discovers them via heartbeat.
	//
	// State key: pool name or machine localpart (e.g., "gpu-cloud-pool"
	// or "machine/spare-workstation")
	// Room: #bureau/fleet:<server>
	EventTypeMachineDefinition ref.EventType = "m.bureau.machine_definition"

	// EventTypeFleetConfig defines global settings for a fleet controller.
	// Each fleet controller has its own config keyed by its localpart.
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
// Fleet rooms are admin-controlled but allow all members (machines, fleet
// controllers) to write fleet-specific state events: fleet services,
// machine definitions, HA leases, fleet config, service status, and
// fleet alerts. Administrative room events (name, join rules, power
// levels) remain locked to admin (PL 100).
func FleetRoomPowerLevels(adminUserID ref.UserID) map[string]any {
	events := AdminProtectedEvents()
	for _, eventType := range []ref.EventType{
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
