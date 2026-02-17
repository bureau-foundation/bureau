// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

func TestIsSensitiveAction(t *testing.T) {
	t.Parallel()

	sensitive := []string{
		"credential/provision/key/FORGEJO_TOKEN",
		"interrupt",
		"interrupt/terminate",
		"fleet/assign",
		"fleet/provision",
		"observe/read-write",
		"grant/approve/observe",
	}
	for _, action := range sensitive {
		if !isSensitiveAction(action) {
			t.Errorf("expected %q to be sensitive", action)
		}
	}

	notSensitive := []string{
		"observe",
		"matrix/join",
		"matrix/invite",
		"service/discover",
		"ticket/create",
		"ticket/assign",
		"artifact/store",
		"command/ticket/list",
	}
	for _, action := range notSensitive {
		if isSensitiveAction(action) {
			t.Errorf("expected %q to NOT be sensitive", action)
		}
	}
}

func TestPostAuditDeny_NoSession(t *testing.T) {
	t.Parallel()

	// Verify postAuditDeny does not panic when session is nil.
	daemon, _ := newTestDaemon(t)
	daemon.postAuditDeny("test/actor", "observe", "test/target",
		"daemon/observe", 0, nil, nil)
}

func TestPostAuditAllow_NoSession(t *testing.T) {
	t.Parallel()

	// Verify postAuditAllow does not panic when session is nil.
	daemon, _ := newTestDaemon(t)
	daemon.postAuditAllow("test/actor", "observe/read-write", "test/target",
		"daemon/observe", nil)
}
