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

func TestFleetRoomAlias(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		namespace string
		fleetName string
		want      string
	}{
		{"bureau_prod", "bureau", "prod", "bureau/fleet/prod"},
		{"bureau_dev", "bureau", "dev", "bureau/fleet/dev"},
		{"acme_staging", "acme", "staging", "acme/fleet/staging"},
		{"custom_namespace", "company_a", "us-east-gpu", "company_a/fleet/us-east-gpu"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := FleetRoomAlias(test.namespace, test.fleetName)
			if got != test.want {
				t.Errorf("FleetRoomAlias(%q, %q) = %q, want %q",
					test.namespace, test.fleetName, got, test.want)
			}
		})
	}
}

func TestFleetMachineRoomAlias(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		namespace string
		fleetName string
		want      string
	}{
		{"bureau_prod", "bureau", "prod", "bureau/fleet/prod/machine"},
		{"bureau_dev", "bureau", "dev", "bureau/fleet/dev/machine"},
		{"acme_staging", "acme", "staging", "acme/fleet/staging/machine"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := FleetMachineRoomAlias(test.namespace, test.fleetName)
			if got != test.want {
				t.Errorf("FleetMachineRoomAlias(%q, %q) = %q, want %q",
					test.namespace, test.fleetName, got, test.want)
			}
		})
	}
}

func TestFleetServiceRoomAlias(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		namespace string
		fleetName string
		want      string
	}{
		{"bureau_prod", "bureau", "prod", "bureau/fleet/prod/service"},
		{"bureau_dev", "bureau", "dev", "bureau/fleet/dev/service"},
		{"acme_staging", "acme", "staging", "acme/fleet/staging/service"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := FleetServiceRoomAlias(test.namespace, test.fleetName)
			if got != test.want {
				t.Errorf("FleetServiceRoomAlias(%q, %q) = %q, want %q",
					test.namespace, test.fleetName, got, test.want)
			}
		})
	}
}

func TestEntityConfigRoomAlias(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		entityLocalpart string
		want            string
	}{
		{"machine", "bureau/fleet/prod/machine/gpu-box", "bureau/fleet/prod/machine/gpu-box"},
		{"service", "bureau/fleet/prod/service/stt/whisper", "bureau/fleet/prod/service/stt/whisper"},
		{"agent", "bureau/fleet/dev/agent/code-reviewer", "bureau/fleet/dev/agent/code-reviewer"},
		{"other_namespace", "acme/fleet/staging/machine/k8s-node-1", "acme/fleet/staging/machine/k8s-node-1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := EntityConfigRoomAlias(test.entityLocalpart)
			if got != test.want {
				t.Errorf("EntityConfigRoomAlias(%q) = %q, want %q",
					test.entityLocalpart, got, test.want)
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
		{
			"fleet_machine_room",
			FleetMachineRoomAlias("bureau", "prod"),
			"bureau.local",
			"#bureau/fleet/prod/machine:bureau.local",
		},
		{
			"entity_config_room",
			EntityConfigRoomAlias("bureau/fleet/prod/machine/gpu-box"),
			"bureau.local",
			"#bureau/fleet/prod/machine/gpu-box:bureau.local",
		},
		{
			"fleet_service_room",
			FleetServiceRoomAlias("bureau", "prod"),
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
