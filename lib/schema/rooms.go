// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

// Room alias localpart constants for Bureau's global Matrix rooms.
// These are the localpart portion of the full room alias — combine
// with FullRoomAlias to construct the complete "#localpart:server"
// form used in Matrix API calls.
//
// See architecture.md and information-architecture.md for the room
// topology. All global room names are singular.
const (
	// RoomAliasSpace is the root Bureau space.
	RoomAliasSpace = "bureau"

	// RoomAliasSystem is the operational messages room.
	RoomAliasSystem = "bureau/system"

	// RoomAliasMachine is the machine keys, heartbeats, and WebRTC
	// signaling room.
	RoomAliasMachine = "bureau/machine"

	// RoomAliasService is the service directory room.
	RoomAliasService = "bureau/service"

	// RoomAliasTemplate is the built-in sandbox template room.
	RoomAliasTemplate = "bureau/template"

	// RoomAliasPipeline is the pipeline definitions room.
	RoomAliasPipeline = "bureau/pipeline"

	// RoomAliasFleet is the default fleet room alias localpart, used by
	// bureau matrix setup to create the initial fleet room. All runtime
	// consumers receive the fleet room ID as an explicit parameter.
	RoomAliasFleet = "bureau/fleet"

	// RoomAliasArtifact is the artifact metadata room.
	RoomAliasArtifact = "bureau/artifact"
)

// ConfigRoomAlias returns the room alias localpart for a per-machine
// config room: "bureau/config/<machineLocalpart>".
func ConfigRoomAlias(machineLocalpart string) string {
	return "bureau/config/" + machineLocalpart
}

// FullRoomAlias constructs a full Matrix room alias from a localpart
// and server name: "#<localpart>:<serverName>".
func FullRoomAlias(localpart, serverName string) string {
	return "#" + localpart + ":" + serverName
}
