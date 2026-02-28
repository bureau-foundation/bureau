// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
)

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
			"namespace_system_room",
			"bureau/system",
			"bureau.local",
			"#bureau/system:bureau.local",
		},
		{
			"workspace_alias",
			"iree/amdgpu/inference",
			"bureau.local",
			"#iree/amdgpu/inference:bureau.local",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := FullRoomAlias(test.localpart, ref.MustParseServerName(test.serverName))
			if got != test.want {
				t.Errorf("FullRoomAlias(%q, %q) = %q, want %q",
					test.localpart, test.serverName, got, test.want)
			}
		})
	}
}

func TestDevTeamRoomAlias(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		namespace string
		server    string
		want      string
	}{
		{
			"bureau_namespace",
			"bureau",
			"bureau.local",
			"#bureau/dev:bureau.local",
		},
		{
			"project_namespace",
			"iree",
			"bureau.local",
			"#iree/dev:bureau.local",
		},
		{
			"different_server",
			"stories",
			"example.com",
			"#stories/dev:example.com",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := ref.MustParseServerName(test.server)
			namespace, err := ref.NewNamespace(server, test.namespace)
			if err != nil {
				t.Fatalf("NewNamespace(%q, %q): %v", test.server, test.namespace, err)
			}
			got := DevTeamRoomAlias(namespace)
			if got.String() != test.want {
				t.Errorf("DevTeamRoomAlias(%q) = %q, want %q",
					test.namespace, got.String(), test.want)
			}
		})
	}
}
