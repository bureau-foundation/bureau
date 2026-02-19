// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

// Room alias localpart constants for Bureau's global Matrix rooms.
// These are the localpart portion of the full room alias — combine
// with FullRoomAlias to construct the complete "#localpart:server"
// form used in Matrix API calls.
//
// Global rooms are fleet-independent: they hold definitions (templates,
// pipelines, artifacts) and operational messages. Fleet-scoped rooms
// (machines, services, fleet config) live under
// <namespace>/fleet/<name>/ and are constructed via the
// Fleet*RoomAlias helpers below.
//
// See naming-conventions.md for the full room topology.
const (
	// RoomAliasSpace is the root Bureau space.
	RoomAliasSpace = "bureau"

	// RoomAliasSystem is the operational messages room.
	RoomAliasSystem = "bureau/system"

	// RoomAliasTemplate is the built-in sandbox template room.
	RoomAliasTemplate = "bureau/template"

	// RoomAliasPipeline is the pipeline definitions room.
	RoomAliasPipeline = "bureau/pipeline"

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

// FullRoomAlias constructs a full Matrix room alias from a localpart
// and server name: "#<localpart>:<serverName>".
func FullRoomAlias(localpart, serverName string) string {
	return "#" + localpart + ":" + serverName
}
