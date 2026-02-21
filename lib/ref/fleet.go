// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ref

import "fmt"

// Fleet identifies an infrastructure isolation boundary within a
// namespace. Each fleet has its own machines, services, and fleet
// controller. A fleet's localpart follows the structure:
// namespace/fleet/fleetName.
//
// Size: ~64 bytes (Namespace + fleet string + pre-computed localpart).
// Room alias methods compute strings on demand.
type Fleet struct {
	ns        Namespace
	fleet     string
	localpart string // pre-computed: "my_bureau/fleet/prod"
}

// NewFleet creates a validated Fleet reference within a namespace.
// The fleet name must be a single path segment (no slashes).
func NewFleet(ns Namespace, fleet string) (Fleet, error) {
	if ns.IsZero() {
		return Fleet{}, fmt.Errorf("invalid fleet: namespace is zero-value")
	}
	if fleet == "" {
		return Fleet{}, fmt.Errorf("invalid fleet: fleet name is empty")
	}
	if err := validateSegment(fleet, "fleet name"); err != nil {
		return Fleet{}, fmt.Errorf("invalid fleet: %w", err)
	}
	if err := validatePath(fleet, "fleet name"); err != nil {
		return Fleet{}, fmt.Errorf("invalid fleet: %w", err)
	}
	localpart := ns.namespace + "/" + fleetLiteral + "/" + fleet
	return Fleet{
		ns:        ns,
		fleet:     fleet,
		localpart: localpart,
	}, nil
}

// ParseFleet parses a fleet localpart ("my_bureau/fleet/prod") and server
// into a Fleet reference.
func ParseFleet(localpart, server string) (Fleet, error) {
	namespace, fleetName, err := parseFleetPrefix(localpart)
	if err != nil {
		return Fleet{}, fmt.Errorf("invalid fleet: %w", err)
	}
	ns, err := NewNamespace(server, namespace)
	if err != nil {
		return Fleet{}, err
	}
	return NewFleet(ns, fleetName)
}

// ParseFleetRoomAlias parses a fleet room alias
// ("#my_bureau/fleet/prod:server") into a Fleet reference.
func ParseFleetRoomAlias(alias string) (Fleet, error) {
	localpart, server, err := parseRoomAlias(alias)
	if err != nil {
		return Fleet{}, fmt.Errorf("invalid fleet: %w", err)
	}
	return ParseFleet(localpart, server)
}

// Namespace returns the parent Namespace.
func (f Fleet) Namespace() Namespace { return f.ns }

// FleetName returns the bare fleet name (e.g., "prod").
func (f Fleet) FleetName() string { return f.fleet }

// Server returns the Matrix homeserver name.
func (f Fleet) Server() string { return f.ns.server }

// Localpart returns the full fleet localpart: "my_bureau/fleet/prod".
func (f Fleet) Localpart() string { return f.localpart }

// String returns the localpart, satisfying fmt.Stringer.
func (f Fleet) String() string { return f.localpart }

// IsZero reports whether this is an uninitialized zero-value Fleet.
func (f Fleet) IsZero() bool { return f.localpart == "" }

// RoomAlias returns the fleet config room alias: #localpart:server.
func (f Fleet) RoomAlias() RoomAlias {
	return newRoomAlias(f.localpart, f.ns.server)
}

// MachineRoomAlias returns the fleet's machine presence room alias.
func (f Fleet) MachineRoomAlias() RoomAlias {
	return newRoomAlias(f.localpart+"/machine", f.ns.server)
}

// ServiceRoomAlias returns the fleet's service directory room alias.
func (f Fleet) ServiceRoomAlias() RoomAlias {
	return newRoomAlias(f.localpart+"/service", f.ns.server)
}

// MachineRoomAliasLocalpart returns the localpart of the machine room alias.
func (f Fleet) MachineRoomAliasLocalpart() string {
	return f.localpart + "/machine"
}

// ServiceRoomAliasLocalpart returns the localpart of the service room alias.
func (f Fleet) ServiceRoomAliasLocalpart() string {
	return f.localpart + "/service"
}

// RunDir returns the fleet-scoped runtime directory: base/fleet/fleetName.
// Socket paths for entities in this fleet are relative to this directory.
func (f Fleet) RunDir(base string) string {
	return base + "/" + fleetLiteral + "/" + f.fleet
}

// MarshalText implements encoding.TextMarshaler. Serializes as the
// fleet room alias: #localpart:server.
func (f Fleet) MarshalText() ([]byte, error) {
	if f.IsZero() {
		return nil, fmt.Errorf("cannot marshal zero-value Fleet")
	}
	return []byte(f.RoomAlias().String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler. Parses the
// fleet room alias: #localpart:server.
func (f *Fleet) UnmarshalText(data []byte) error {
	parsed, err := ParseFleetRoomAlias(string(data))
	if err != nil {
		return err
	}
	*f = parsed
	return nil
}
