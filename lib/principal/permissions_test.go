// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package principal

import (
	"os"
	"os/user"
	"strconv"
	"testing"
)

func TestCurrentUserInGroup_PrimaryGroup(t *testing.T) {
	// The current user's primary group should always be accessible.
	// Look up the primary group by GID to get its name, then check
	// membership.
	primaryGID := os.Getgid()
	group, err := user.LookupGroupId(strconv.Itoa(primaryGID))
	if err != nil {
		t.Skipf("cannot look up primary GID %d: %v", primaryGID, err)
	}

	if !CurrentUserInGroup(group.Name) {
		t.Fatalf("expected current user to be in primary group %q (GID %d)", group.Name, primaryGID)
	}
}

func TestCurrentUserInGroup_NonexistentGroup(t *testing.T) {
	if CurrentUserInGroup("bureau-definitely-not-a-real-group-xyzzy") {
		t.Fatal("expected false for nonexistent group")
	}
}

func TestCurrentUserInGroup_EmptyGroup(t *testing.T) {
	if CurrentUserInGroup("") {
		t.Fatal("expected false for empty group name")
	}
}

func TestLookupOperatorsGID_EmptyName(t *testing.T) {
	if gid := LookupOperatorsGID(""); gid != -1 {
		t.Fatalf("expected -1 for empty group name, got %d", gid)
	}
}

func TestLookupOperatorsGID_NonexistentGroup(t *testing.T) {
	if gid := LookupOperatorsGID("bureau-definitely-not-a-real-group-xyzzy"); gid != -1 {
		t.Fatalf("expected -1 for nonexistent group, got %d", gid)
	}
}
