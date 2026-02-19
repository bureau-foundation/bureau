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
		{"space", RoomAliasSpace, "bureau"},
		{"system", RoomAliasSystem, "bureau/system"},
		{"template", RoomAliasTemplate, "bureau/template"},
		{"pipeline", RoomAliasPipeline, "bureau/pipeline"},
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

func TestFullRoomAlias(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		localpart  string
		serverName string
		want       string
	}{
		{
			"fleet_machine_room",
			"bureau/fleet/prod/machine",
			"bureau.local",
			"#bureau/fleet/prod/machine:bureau.local",
		},
		{
			"entity_config_room",
			"bureau/fleet/prod/machine/gpu-box",
			"bureau.local",
			"#bureau/fleet/prod/machine/gpu-box:bureau.local",
		},
		{
			"fleet_service_room",
			"bureau/fleet/prod/service",
			"example.com",
			"#bureau/fleet/prod/service:example.com",
		},
		{
			"global_system_room",
			RoomAliasSystem,
			"bureau.local",
			"#bureau/system:bureau.local",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := FullRoomAlias(test.localpart, test.serverName)
			if got != test.want {
				t.Errorf("FullRoomAlias(%q, %q) = %q, want %q",
					test.localpart, test.serverName, got, test.want)
			}
		})
	}
}
