// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import "github.com/bureau-foundation/bureau/lib/ref"

// FullRoomAlias constructs a full Matrix room alias from a localpart
// and server name: "#<localpart>:<serverName>". Used at CLI and API
// boundaries where a user-provided or domain-specific room localpart
// needs to be turned into a resolvable alias. Namespace-scoped standard
// rooms (system, template, pipeline, artifact) should use
// ref.Namespace methods instead.
func FullRoomAlias(localpart string, serverName ref.ServerName) string {
	return "#" + localpart + ":" + serverName.String()
}

// DevTeamRoomAlias returns the conventional dev team room alias for a
// namespace: #<namespace>/dev:<server>. This is the naming convention
// for a namespace's primary development team room. Not every namespace
// has a dev team room — this returns the conventional alias whether or
// not the room exists. Callers should resolve the alias before using it.
func DevTeamRoomAlias(namespace ref.Namespace) ref.RoomAlias {
	return ref.MustParseRoomAlias(FullRoomAlias(namespace.Name()+"/dev", namespace.Server()))
}
