// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package principal

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// Location describes where a principal is assigned in the fleet.
type Location struct {
	// Machine identifies the machine this principal is assigned to.
	Machine ref.Machine

	// ConfigRoomID is the Matrix room ID of the machine's config room.
	ConfigRoomID string

	// Assignment is the full PrincipalAssignment from the MachineConfig.
	Assignment schema.PrincipalAssignment
}

// Resolve finds which machine a principal is assigned to.
//
// If machine is non-zero, reads only that machine's config room and
// verifies the principal is assigned there. If machine is zero, scans
// all machines in the fleet to find the assignment.
//
// Returns the number of machines scanned in the second return value
// (0 when machine is provided directly).
//
// Returns an error if the principal is not found on any machine.
func Resolve(ctx context.Context, session messaging.Session, localpart string, machine ref.Machine, fleet ref.Fleet) (*Location, int, error) {
	if !machine.IsZero() {
		location, err := readFromMachine(ctx, session, localpart, machine)
		if err != nil {
			return nil, 0, err
		}
		return location, 0, nil
	}

	// Scan all machines.
	locations, machineCount, err := List(ctx, session, ref.Machine{}, fleet)
	if err != nil {
		return nil, machineCount, fmt.Errorf("scanning machines for %q: %w", localpart, err)
	}

	for i := range locations {
		if locations[i].Assignment.Localpart == localpart {
			return &locations[i], machineCount, nil
		}
	}

	return nil, machineCount, fmt.Errorf("principal %q not assigned to any machine (scanned %d machines)", localpart, machineCount)
}

// List returns all principal assignments across machines.
//
// If machine is non-zero, returns only assignments from that machine.
// If machine is zero, enumerates all machines from the fleet's machine
// room and reads each machine's config. Returns the total number of
// machines scanned in the second return value.
func List(ctx context.Context, session messaging.Session, machine ref.Machine, fleet ref.Fleet) ([]Location, int, error) {
	if !machine.IsZero() {
		locations, err := listOnMachine(ctx, session, machine)
		if err != nil {
			return nil, 0, err
		}
		return locations, 0, nil
	}

	// Resolve the fleet's machine room.
	machineRoomAlias := fleet.MachineRoomAlias()
	machineRoomID, err := session.ResolveAlias(ctx, machineRoomAlias)
	if err != nil {
		return nil, 0, fmt.Errorf("resolving fleet machine room %q: %w", machineRoomAlias, err)
	}

	// Enumerate all machines from the fleet machine room.
	machines, err := enumerateMachines(ctx, session, machineRoomID, fleet)
	if err != nil {
		return nil, 0, err
	}

	var allLocations []Location
	for _, m := range machines {
		locations, err := listOnMachine(ctx, session, m)
		if err != nil {
			// A machine with no config room or no config is not an error —
			// it just has no agents. Skip it.
			continue
		}
		allLocations = append(allLocations, locations...)
	}

	return allLocations, len(machines), nil
}

// readFromMachine reads a specific machine's config and finds the
// named principal.
func readFromMachine(ctx context.Context, session messaging.Session, localpart string, machine ref.Machine) (*Location, error) {
	configRoomID, config, err := readMachineConfig(ctx, session, machine)
	if err != nil {
		return nil, err
	}

	for _, assignment := range config.Principals {
		if assignment.Localpart == localpart {
			return &Location{
				Machine:      machine,
				ConfigRoomID: configRoomID,
				Assignment:   assignment,
			}, nil
		}
	}

	return nil, fmt.Errorf("principal %q not assigned to %s", localpart, machine.Localpart())
}

// listOnMachine reads a machine's config and returns all assignments.
func listOnMachine(ctx context.Context, session messaging.Session, machine ref.Machine) ([]Location, error) {
	configRoomID, config, err := readMachineConfig(ctx, session, machine)
	if err != nil {
		return nil, err
	}

	locations := make([]Location, 0, len(config.Principals))
	for _, assignment := range config.Principals {
		locations = append(locations, Location{
			Machine:      machine,
			ConfigRoomID: configRoomID,
			Assignment:   assignment,
		})
	}
	return locations, nil
}

// readMachineConfig resolves a machine's config room and reads its
// MachineConfig state event.
func readMachineConfig(ctx context.Context, session messaging.Session, machine ref.Machine) (string, *schema.MachineConfig, error) {
	configRoomID, err := session.ResolveAlias(ctx, machine.RoomAlias())
	if err != nil {
		return "", nil, fmt.Errorf("resolve config room for %s: %w", machine.Localpart(), err)
	}

	configRaw, err := session.GetStateEvent(ctx, configRoomID, schema.EventTypeMachineConfig, machine.Localpart())
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			return configRoomID, &schema.MachineConfig{}, nil
		}
		return "", nil, fmt.Errorf("read machine config for %s: %w", machine.Localpart(), err)
	}

	var config schema.MachineConfig
	if err := json.Unmarshal(configRaw, &config); err != nil {
		return "", nil, fmt.Errorf("parse machine config for %s: %w", machine.Localpart(), err)
	}

	return configRoomID, &config, nil
}

// enumerateMachines reads a fleet's machine room state to find all
// machines. Machines publish m.bureau.machine_status state events keyed
// by their fleet-scoped localpart.
func enumerateMachines(ctx context.Context, session messaging.Session, machineRoomID string, fleet ref.Fleet) ([]ref.Machine, error) {
	events, err := session.GetRoomState(ctx, machineRoomID)
	if err != nil {
		return nil, fmt.Errorf("read machine room state: %w", err)
	}

	var machines []ref.Machine
	for _, event := range events {
		if event.Type != schema.EventTypeMachineStatus || event.StateKey == nil || *event.StateKey == "" {
			continue
		}
		machine, err := ref.ParseMachine(*event.StateKey, fleet.Server())
		if err != nil {
			// State key doesn't parse as a valid machine — skip rather
			// than failing the entire enumeration. This handles legacy
			// or malformed entries.
			continue
		}
		machines = append(machines, machine)
	}

	return machines, nil
}
