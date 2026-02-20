// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package credential

import (
	"context"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/messaging"
)

// AsProvisionFunc returns a principal.ProvisionFunc backed by
// credential.Provision. This adapts the credential package's full
// provisioning workflow (machine key lookup, age encryption, state event
// publishing) into the function signature that principal.Create expects.
func AsProvisionFunc() principal.ProvisionFunc {
	return func(ctx context.Context, session messaging.Session, machine ref.Machine, entity ref.Entity, machineRoomID ref.RoomID, credentials map[string]string) (ref.RoomID, error) {
		result, err := Provision(ctx, session, ProvisionParams{
			Machine:       machine,
			Principal:     entity,
			MachineRoomID: machineRoomID,
			Credentials:   credentials,
		})
		if err != nil {
			return ref.RoomID{}, err
		}
		return result.ConfigRoomID, nil
	}
}
