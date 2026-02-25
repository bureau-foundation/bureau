// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package principal

import (
	"os"
	"os/user"
	"strconv"
)

const (
	// SystemUserName is the Unix user that runs Bureau's launcher and
	// daemon processes. Created during machine bootstrap by
	// "bureau machine doctor --fix".
	SystemUserName = "bureau"

	// OperatorsGroupName is the Unix group whose members get CLI access
	// to Bureau's operator-facing sockets (observe.sock, service CBOR
	// endpoints). Membership grants filesystem access to the sockets;
	// authorization beyond that is governed by each operator's Matrix
	// identity and grants.
	OperatorsGroupName = "bureau-operators"
)

// LookupOperatorsGID returns the numeric GID of the bureau-operators
// system group. Returns -1 if the group does not exist.
//
// Development environments typically lack the bureau-operators group
// (all processes run as the developer's user). Production environments
// have it created by "bureau machine doctor --fix". Callers should
// treat -1 as "skip group ownership changes" and log a warning at
// startup.
func LookupOperatorsGID() int {
	group, err := user.LookupGroup(OperatorsGroupName)
	if err != nil {
		return -1
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil {
		return -1
	}
	return gid
}

// SetOperatorGroupOwnership changes the group of a file to the
// bureau-operators group, making it accessible to operators. The
// file owner is left unchanged (-1).
//
// If operatorsGID is negative (group not found), this is a no-op
// and returns nil — the socket is still accessible to the process
// owner but not to other operators. This allows development
// environments to work without the bureau-operators group.
func SetOperatorGroupOwnership(path string, operatorsGID int) error {
	if operatorsGID < 0 {
		return nil
	}
	return os.Chown(path, -1, operatorsGID)
}
