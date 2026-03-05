// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package relayauth

import (
	"fmt"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// ResolveOpsRoom determines the ops room alias for a resource claim
// within a fleet. The relay policy for this resource lives in this
// room as an EventTypeRelayPolicy state event.
//
// Currently supported resource types:
//   - machine: target is the machine name; ops room is the machine's
//     ops room (#fleet/machine/<name>/ops:server).
//
// Unsupported resource types return an error. As new resource types
// gain ops rooms (quota controllers, service capacity managers), add
// resolution logic here.
func ResolveOpsRoom(resource schema.ResourceRef, fleet ref.Fleet) (ref.RoomAlias, error) {
	switch resource.Type {
	case schema.ResourceMachine:
		machine, err := ref.NewMachine(fleet, resource.Target)
		if err != nil {
			return ref.RoomAlias{}, fmt.Errorf("resolve ops room for machine %q: %w", resource.Target, err)
		}
		return machine.OpsRoomAlias(), nil

	case schema.ResourceQuota, schema.ResourceHuman, schema.ResourceService:
		return ref.RoomAlias{}, fmt.Errorf("resource type %q does not have ops rooms yet", resource.Type)

	default:
		return ref.RoomAlias{}, fmt.Errorf("unknown resource type %q", resource.Type)
	}
}
