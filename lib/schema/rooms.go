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
