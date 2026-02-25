// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/artifact"
	"github.com/bureau-foundation/bureau/lib/schema/fleet"
	"github.com/bureau-foundation/bureau/lib/schema/observation"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
)

func TestIsSensitiveAction(t *testing.T) {
	t.Parallel()

	sensitive := []string{
		schema.ActionCredentialProvisionKeyPrefix + "FORGEJO_TOKEN",
		schema.ActionInterrupt,
		schema.ActionInterruptTerminate,
		fleet.ActionAssign,
		fleet.ActionProvision,
		observation.ActionReadWrite,
		schema.ActionGrantApprovePrefix + observation.ActionObserve,
	}
	for _, action := range sensitive {
		if !isSensitiveAction(action) {
			t.Errorf("expected %q to be sensitive", action)
		}
	}

	notSensitive := []string{
		observation.ActionObserve,
		schema.ActionMatrixJoin,
		schema.ActionMatrixInvite,
		schema.ActionServiceDiscover,
		ticket.ActionCreate,
		"ticket/assign",
		artifact.ActionStore,
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
	daemon.postAuditDeny(ref.MustParseUserID("@test/actor:bureau.local"), observation.ActionObserve, ref.MustParseUserID("@test/target:bureau.local"),
		"daemon/observe", 0, nil, nil)
}

func TestPostAuditAllow_NoSession(t *testing.T) {
	t.Parallel()

	// Verify postAuditAllow does not panic when session is nil.
	daemon, _ := newTestDaemon(t)
	daemon.postAuditAllow(ref.MustParseUserID("@test/actor:bureau.local"), observation.ActionReadWrite, ref.MustParseUserID("@test/target:bureau.local"),
		"daemon/observe", nil)
}
