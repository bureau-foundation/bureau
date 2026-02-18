// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

// Room alias localpart constants for Bureau's global Matrix rooms.
// These are the localpart portion of the full room alias — combine
// with FullRoomAlias to construct the complete "#localpart:server"
// form used in Matrix API calls.
//
// Global rooms are fleet-independent: they hold definitions (templates,
// pipelines) that are reusable across fleets. Fleet-scoped rooms
// (machines, services) live under <namespace>/fleet/<name>/ and are
// constructed via the Fleet*RoomAlias helpers below.
//
// See naming-conventions.md for the full room topology.
const (
	// RoomAliasSpace is the root Bureau space.
	RoomAliasSpace = "bureau"

	// RoomAliasSystem is the operational messages room.
	RoomAliasSystem = "bureau/system"

	// RoomAliasMachine is the global machine room. Under fleet-scoped
	// naming, each fleet gets its own machine room via
	// FleetMachineRoomAlias. This constant is used by callers that have
	// not yet migrated to fleet-scoped rooms.
	RoomAliasMachine = "bureau/machine"

	// RoomAliasService is the global service directory room. Under
	// fleet-scoped naming, each fleet gets its own service room via
	// FleetServiceRoomAlias. This constant is used by callers that have
	// not yet migrated to fleet-scoped rooms.
	RoomAliasService = "bureau/service"

	// RoomAliasTemplate is the built-in sandbox template room.
	RoomAliasTemplate = "bureau/template"

	// RoomAliasPipeline is the pipeline definitions room.
	RoomAliasPipeline = "bureau/pipeline"

	// RoomAliasFleet is the global fleet room alias localpart. Under
	// fleet-scoped naming, individual fleets are constructed via
	// FleetRoomAlias. This constant is used by callers that have not
	// yet migrated to fleet-scoped rooms.
	RoomAliasFleet = "bureau/fleet"

	// RoomAliasArtifact is the artifact metadata room.
	RoomAliasArtifact = "bureau/artifact"
)

// FleetRoomAlias returns the room alias localpart for a fleet's config
// room. This room holds fleet configuration, HA leases, and fleet-wide
// service definitions.
//
// Example: FleetRoomAlias("bureau", "prod") → "bureau/fleet/prod"
func FleetRoomAlias(namespace, fleetName string) string {
	return namespace + "/fleet/" + fleetName
}

// FleetMachineRoomAlias returns the room alias localpart for a fleet's
// machine presence room. This room aggregates MachineInfo and
// MachineStatus state events for all machines in the fleet.
//
// Example: FleetMachineRoomAlias("bureau", "prod") → "bureau/fleet/prod/machine"
func FleetMachineRoomAlias(namespace, fleetName string) string {
	return namespace + "/fleet/" + fleetName + "/machine"
}

// FleetServiceRoomAlias returns the room alias localpart for a fleet's
// service directory room. This room holds service registrations for all
// services running in the fleet.
//
// Example: FleetServiceRoomAlias("bureau", "prod") → "bureau/fleet/prod/service"
func FleetServiceRoomAlias(namespace, fleetName string) string {
	return namespace + "/fleet/" + fleetName + "/service"
}

// EntityConfigRoomAlias returns the room alias localpart for an
// entity's config room. Under the @→# convention, an entity's Matrix
// user ID localpart IS its config room alias localpart — swap the
// sigil and you get the room. This function makes that convention
// explicit at call sites.
//
// Example: EntityConfigRoomAlias("bureau/fleet/prod/machine/gpu-box")
// → "bureau/fleet/prod/machine/gpu-box"
func EntityConfigRoomAlias(entityLocalpart string) string {
	return entityLocalpart
}

// ConfigRoomAlias returns the room alias localpart for a per-machine
// config room: "bureau/config/<machineLocalpart>". Under fleet-scoped
// naming, entity config rooms use the @→# convention via
// EntityConfigRoomAlias instead. This function is used by callers
// that have not yet migrated to fleet-scoped naming.
func ConfigRoomAlias(machineLocalpart string) string {
	return "bureau/config/" + machineLocalpart
}

// FullRoomAlias constructs a full Matrix room alias from a localpart
// and server name: "#<localpart>:<serverName>".
func FullRoomAlias(localpart, serverName string) string {
	return "#" + localpart + ":" + serverName
}
