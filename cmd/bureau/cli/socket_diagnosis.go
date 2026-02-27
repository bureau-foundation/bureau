// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"syscall"

	"github.com/bureau-foundation/bureau/lib/principal"
)

// DiagnoseSocketError inspects a socket connection error and returns a
// categorized ToolError with an actionable hint when the failure is due
// to Unix group membership. If the error does not wrap EACCES or EPERM,
// returns nil — the caller should use its own error wrapping.
//
// The diagnosis checks whether the current process belongs to the
// bureau-operators group (the Unix group that controls filesystem
// access to Bureau's operator-facing sockets). Three outcomes:
//
//   - Permission denied + group doesn't exist → hint to run doctor
//   - Permission denied + group exists but user not a member → hint to
//     add user to group and re-login
//   - Permission denied + user IS in group → some other permission
//     issue (SELinux, ACL, wrong owner); returns a generic forbidden
//     error without misleading group advice
func DiagnoseSocketError(err error, socketPath string) *ToolError {
	if !errors.Is(err, syscall.EACCES) && !errors.Is(err, syscall.EPERM) {
		return nil
	}

	operatorsGID := principal.LookupOperatorsGID(principal.OperatorsGroupName)
	if operatorsGID < 0 {
		// The group doesn't exist at all. This machine hasn't been
		// bootstrapped for multi-user operation.
		return Forbidden("permission denied accessing %s", socketPath).
			WithHint("The " + principal.OperatorsGroupName + " group does not exist on this machine.\n" +
				"Run 'sudo bureau machine doctor --fix' to create the group and configure socket permissions.")
	}

	if !principal.CurrentUserInGroup(principal.OperatorsGroupName) {
		return Forbidden("permission denied accessing %s (user not in %s group)", socketPath, principal.OperatorsGroupName).
			WithHint("Add your user to the group and re-login for it to take effect:\n" +
				"  sudo usermod -aG " + principal.OperatorsGroupName + " $USER\n" +
				"  newgrp " + principal.OperatorsGroupName + "\n\n" +
				"The newgrp command applies the group to the current shell immediately.\n" +
				"For all sessions, log out and log back in.")
	}

	// User IS in the group but still got permission denied. Could be
	// SELinux, filesystem ACLs, wrong socket owner, or the socket
	// belongs to a different group. Don't mislead with group advice.
	return Forbidden("permission denied accessing %s", socketPath).
		WithHint("Your user is in the " + principal.OperatorsGroupName + " group, but the socket is still not accessible.\n" +
			"Check the socket's ownership and permissions: ls -la " + socketPath + "\n" +
			"The socket should be owned by " + principal.SystemUserName + ":" + principal.OperatorsGroupName + " with mode 0660.")
}
