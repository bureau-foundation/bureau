// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package principal

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// Location describes where a principal is assigned in the fleet.
type Location struct {
	// MachineName is the machine's localpart (e.g., "machine/workstation").
	MachineName string

	// ConfigRoomID is the Matrix room ID of the machine's config room.
	ConfigRoomID string

	// Assignment is the full PrincipalAssignment from the MachineConfig.
	Assignment schema.PrincipalAssignment
}

// Resolve finds which machine a principal is assigned to.
//
// If machineName is non-empty, reads only that machine's config room and
// verifies the principal is assigned there. If machineName is empty, scans
// all machines in the fleet to find the assignment. The fleetPrefix
// (e.g., "bureau/fleet/prod") identifies which fleet's machine room to
// enumerate; it is only used when machineName is empty.
//
// Returns the number of machines scanned in the second return value
// (0 when machineName is provided directly).
//
// Returns an error if the principal is not found on any machine.
func Resolve(ctx context.Context, session messaging.Session, localpart, machineName, fleetPrefix, serverName string) (*Location, int, error) {
	if machineName != "" {
		location, err := readFromMachine(ctx, session, localpart, machineName, serverName)
		if err != nil {
			return nil, 0, err
		}
		return location, 0, nil
	}

	// Scan all machines.
	locations, machineCount, err := List(ctx, session, "", fleetPrefix, serverName)
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
// If machineName is non-empty, returns only assignments from that machine.
// If machineName is empty, enumerates all machines from the fleet's
// machine room and reads each machine's config. The fleetPrefix
// (e.g., "bureau/fleet/prod") identifies the fleet; it is only used
// when machineName is empty. Returns the total number of machines
// scanned in the second return value.
func List(ctx context.Context, session messaging.Session, machineName, fleetPrefix, serverName string) ([]Location, int, error) {
	if machineName != "" {
		locations, err := listOnMachine(ctx, session, machineName, serverName)
		if err != nil {
			return nil, 0, err
		}
		return locations, 0, nil
	}

	// Resolve the fleet's machine room.
	namespace, fleetName, err := ParseFleetPrefix(fleetPrefix)
	if err != nil {
		return nil, 0, fmt.Errorf("parsing fleet prefix %q: %w", fleetPrefix, err)
	}
	machineRoomAlias := RoomAlias(schema.FleetMachineRoomAlias(namespace, fleetName), serverName)
	machineRoomID, err := session.ResolveAlias(ctx, machineRoomAlias)
	if err != nil {
		return nil, 0, fmt.Errorf("resolving fleet machine room %q: %w", machineRoomAlias, err)
	}

	// Enumerate all machines from the fleet machine room.
	machineNames, err := enumerateMachines(ctx, session, machineRoomID)
	if err != nil {
		return nil, 0, err
	}

	var allLocations []Location
	for _, machine := range machineNames {
		locations, err := listOnMachine(ctx, session, machine, serverName)
		if err != nil {
			// A machine with no config room or no config is not an error —
			// it just has no agents. Skip it.
			continue
		}
		allLocations = append(allLocations, locations...)
	}

	return allLocations, len(machineNames), nil
}

// readFromMachine reads a specific machine's config and finds the
// named principal.
func readFromMachine(ctx context.Context, session messaging.Session, localpart, machineName, serverName string) (*Location, error) {
	configRoomID, config, err := readMachineConfig(ctx, session, machineName, serverName)
	if err != nil {
		return nil, err
	}

	for _, assignment := range config.Principals {
		if assignment.Localpart == localpart {
			return &Location{
				MachineName:  machineName,
				ConfigRoomID: configRoomID,
				Assignment:   assignment,
			}, nil
		}
	}

	return nil, fmt.Errorf("principal %q not assigned to %s", localpart, machineName)
}

// listOnMachine reads a machine's config and returns all assignments.
func listOnMachine(ctx context.Context, session messaging.Session, machineName, serverName string) ([]Location, error) {
	configRoomID, config, err := readMachineConfig(ctx, session, machineName, serverName)
	if err != nil {
		return nil, err
	}

	locations := make([]Location, 0, len(config.Principals))
	for _, assignment := range config.Principals {
		locations = append(locations, Location{
			MachineName:  machineName,
			ConfigRoomID: configRoomID,
			Assignment:   assignment,
		})
	}
	return locations, nil
}

// readMachineConfig resolves a machine's config room and reads its
// MachineConfig state event.
func readMachineConfig(ctx context.Context, session messaging.Session, machineName, serverName string) (string, *schema.MachineConfig, error) {
	configAlias := RoomAlias(schema.ConfigRoomAlias(machineName), serverName)
	configRoomID, err := session.ResolveAlias(ctx, configAlias)
	if err != nil {
		return "", nil, fmt.Errorf("resolve config room for %s: %w", machineName, err)
	}

	configRaw, err := session.GetStateEvent(ctx, configRoomID, schema.EventTypeMachineConfig, machineName)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			return configRoomID, &schema.MachineConfig{}, nil
		}
		return "", nil, fmt.Errorf("read machine config for %s: %w", machineName, err)
	}

	var config schema.MachineConfig
	if err := json.Unmarshal(configRaw, &config); err != nil {
		return "", nil, fmt.Errorf("parse machine config for %s: %w", machineName, err)
	}

	return configRoomID, &config, nil
}

// enumerateMachines reads a fleet's machine room state to find all
// machine localparts. Machines publish m.bureau.machine_status state
// events keyed by their localpart.
func enumerateMachines(ctx context.Context, session messaging.Session, machineRoomID string) ([]string, error) {
	events, err := session.GetRoomState(ctx, machineRoomID)
	if err != nil {
		return nil, fmt.Errorf("read machine room state: %w", err)
	}

	var machineNames []string
	for _, event := range events {
		if event.Type == schema.EventTypeMachineStatus && event.StateKey != nil && *event.StateKey != "" {
			machineNames = append(machineNames, *event.StateKey)
		}
	}

	return machineNames, nil
}
