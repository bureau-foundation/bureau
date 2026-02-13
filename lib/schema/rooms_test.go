// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import "testing"

func TestRoomAliasConstants(t *testing.T) {
	t.Parallel()
	// Room alias localparts are wire-format identifiers used in Matrix
	// room alias resolution. They must match the values used by
	// "bureau matrix setup" and the daemon's room discovery.
	tests := []struct {
		name     string
		constant string
		want     string
	}{
		{"system", RoomAliasSystem, "bureau/system"},
		{"machine", RoomAliasMachine, "bureau/machine"},
		{"service", RoomAliasService, "bureau/service"},
		{"template", RoomAliasTemplate, "bureau/template"},
		{"pipeline", RoomAliasPipeline, "bureau/pipeline"},
		{"fleet", RoomAliasFleet, "bureau/fleet"},
		{"artifact", RoomAliasArtifact, "bureau/artifact"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if test.constant != test.want {
				t.Errorf("%s = %q, want %q", test.name, test.constant, test.want)
			}
		})
	}
}

func TestConfigRoomAlias(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		machineLocalpart string
		want             string
	}{
		{"simple", "workstation", "bureau/config/workstation"},
		{"hierarchical", "machine/rack-01", "bureau/config/machine/rack-01"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := ConfigRoomAlias(test.machineLocalpart)
			if got != test.want {
				t.Errorf("ConfigRoomAlias(%q) = %q, want %q", test.machineLocalpart, got, test.want)
			}
		})
	}
}

func TestFullRoomAlias(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		localpart  string
		serverName string
		want       string
	}{
		{"machine_room", RoomAliasMachine, "bureau.local", "#bureau/machine:bureau.local"},
		{"config_room", ConfigRoomAlias("workstation"), "bureau.local", "#bureau/config/workstation:bureau.local"},
		{"service_room", RoomAliasService, "example.com", "#bureau/service:example.com"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := FullRoomAlias(test.localpart, test.serverName)
			if got != test.want {
				t.Errorf("FullRoomAlias(%q, %q) = %q, want %q", test.localpart, test.serverName, got, test.want)
			}
		})
	}
}
