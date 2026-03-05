// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

//nolint:dupl // Machine, Service, and Agent are structurally identical by design — distinct types for compile-time safety.
package ref

import "fmt"

// Machine identifies a machine entity within a fleet. A machine is a
// Bureau daemon+launcher instance on a physical or virtual host.
//
// Machine embeds an unexported entity type, which provides all
// accessor methods: Fleet, Name, Localpart, UserID, RoomAlias, Server,
// String, IsZero, SocketPath, AdminSocketPath, and MarshalText.
type Machine struct{ entity }

// NewMachine creates a validated Machine reference within a fleet.
// The name is the bare machine name (e.g., "gpu-box"), not the
// fleet-relative form ("machine/gpu-box").
func NewMachine(fleet Fleet, name string) (Machine, error) {
	ent, err := newEntity(fleet, entityTypeMachine, name)
	if err != nil {
		return Machine{}, err
	}
	return Machine{entity: ent}, nil
}

// ParseMachineUserID parses a full Matrix user ID
// ("@my_bureau/fleet/prod/machine/gpu-box:server") into a Machine.
func ParseMachineUserID(userID string) (Machine, error) {
	ent, err := parseEntityUserID(userID, entityTypeMachine)
	if err != nil {
		return Machine{}, err
	}
	return Machine{entity: ent}, nil
}

// ParseMachine parses a fleet-scoped localpart and server into a Machine.
func ParseMachine(localpart string, server ServerName) (Machine, error) {
	ent, err := parseEntityLocalpart(localpart, server, entityTypeMachine)
	if err != nil {
		return Machine{}, err
	}
	return Machine{entity: ent}, nil
}

// OpsRoomAlias returns the machine's ops room alias for operational
// coordination (reservations, drain, deployments). The ops room is
// separate from the config room: config rooms carry credentials with
// restrictive power levels; ops rooms allow services to coordinate
// without credential access.
//
// Alias convention: config room localpart + "/ops".
// Example: #my_bureau/fleet/prod/machine/gpu-box/ops:bureau.local
func (m Machine) OpsRoomAlias() RoomAlias {
	return newRoomAlias(m.Localpart()+"/ops", m.Server())
}

// Entity converts this Machine to a generic Entity reference. This is a
// zero-cost type conversion — Machine, Service, and Entity all embed the
// same unexported entity struct.
func (m Machine) Entity() Entity { return Entity{m.entity} }

// UnmarshalText implements encoding.TextUnmarshaler. Parses the Matrix
// user ID form: @localpart:server. Empty input produces a zero value.
func (m *Machine) UnmarshalText(data []byte) error {
	if len(data) == 0 {
		*m = Machine{}
		return nil
	}
	parsed, err := ParseMachineUserID(string(data))
	if err != nil {
		return fmt.Errorf("unmarshal Machine: %w", err)
	}
	*m = parsed
	return nil
}
