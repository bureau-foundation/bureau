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
