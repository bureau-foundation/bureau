// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ref

import "fmt"

const (
	entityTypeMachine = "machine"
	entityTypeService = "service"
	entityTypeAgent   = "agent"
)

// entity is the shared base for Machine, Service, and Agent references.
// It is unexported to prevent direct construction — callers must use
// the typed constructors (NewMachine, NewService, NewAgent).
//
// Size: ~128 bytes (Fleet + 4 string headers for name, localpart,
// userID, roomAlias, plus the entityType string).
type entity struct {
	fleet      Fleet
	name       string
	entityType string
	localpart  string // pre-computed: "my_bureau/fleet/prod/machine/gpu-box"
	userID     string // pre-computed: "@my_bureau/fleet/prod/machine/gpu-box:server"
	roomAlias  string // pre-computed: "#my_bureau/fleet/prod/machine/gpu-box:server"
}

// newEntity constructs a validated entity. The name may contain slashes
// for hierarchical names (e.g., "stt/whisper" for services).
func newEntity(fleet Fleet, entityType, name string) (entity, error) {
	if fleet.IsZero() {
		return entity{}, fmt.Errorf("invalid %s ref: fleet is zero-value", entityType)
	}
	if name == "" {
		return entity{}, fmt.Errorf("invalid %s ref: name is empty", entityType)
	}
	if err := validatePath(name, entityType+" name"); err != nil {
		return entity{}, fmt.Errorf("invalid %s ref: %w", entityType, err)
	}

	localpart := fleet.localpart + "/" + entityType + "/" + name
	if err := validateLocalpart(localpart); err != nil {
		return entity{}, fmt.Errorf("invalid %s ref: %w", entityType, err)
	}

	server := fleet.ns.server
	return entity{
		fleet:      fleet,
		name:       name,
		entityType: entityType,
		localpart:  localpart,
		userID:     "@" + localpart + ":" + server,
		roomAlias:  "#" + localpart + ":" + server,
	}, nil
}

// parseEntityUserID parses a Matrix user ID (@localpart:server) into an
// entity, validating that the entity type matches expectedType.
func parseEntityUserID(userID, expectedType string) (entity, error) {
	localpart, server, err := parseMatrixID(userID)
	if err != nil {
		return entity{}, fmt.Errorf("invalid %s ref: %w", expectedType, err)
	}
	return parseEntityLocalpart(localpart, server, expectedType)
}

// parseEntityLocalpart parses a fleet-scoped localpart and server into
// an entity, validating the entity type.
func parseEntityLocalpart(localpart, server, expectedType string) (entity, error) {
	namespace, fleetName, entityType, entityName, err := parseFleetLocalpart(localpart)
	if err != nil {
		return entity{}, fmt.Errorf("invalid %s ref: %w", expectedType, err)
	}
	if entityType != expectedType {
		return entity{}, fmt.Errorf("invalid %s ref: entity type is %q, expected %q", expectedType, entityType, expectedType)
	}
	ns, err := NewNamespace(server, namespace)
	if err != nil {
		return entity{}, fmt.Errorf("invalid %s ref: %w", expectedType, err)
	}
	fleet, err := NewFleet(ns, fleetName)
	if err != nil {
		return entity{}, fmt.Errorf("invalid %s ref: %w", expectedType, err)
	}
	return newEntity(fleet, entityType, entityName)
}

// Accessor methods below are promoted to Machine, Service, and Agent
// via embedding.

// Fleet returns the parent Fleet reference.
func (e entity) Fleet() Fleet { return e.fleet }

// Name returns the bare entity name (e.g., "gpu-box", "stt/whisper").
func (e entity) Name() string { return e.name }

// Localpart returns the full fleet-scoped localpart.
func (e entity) Localpart() string { return e.localpart }

// UserID returns the full Matrix user ID: @localpart:server.
func (e entity) UserID() string { return e.userID }

// RoomAlias returns the entity's config room alias: #localpart:server.
// This is the @ -> # rule: swap the sigil to get the room.
func (e entity) RoomAlias() string { return e.roomAlias }

// Server returns the Matrix homeserver name.
func (e entity) Server() string { return e.fleet.ns.server }

// String returns the localpart, satisfying fmt.Stringer.
func (e entity) String() string { return e.localpart }

// IsZero reports whether this is an uninitialized zero-value entity.
func (e entity) IsZero() bool { return e.userID == "" }

// SocketPath returns the agent-facing socket path within a fleet's
// run directory.
func (e entity) SocketPath(runDir string) string {
	return runDir + "/" + e.entityType + "/" + e.name + socketSuffix
}

// AdminSocketPath returns the daemon-only admin socket path within a
// fleet's run directory.
func (e entity) AdminSocketPath(runDir string) string {
	return runDir + "/" + e.entityType + "/" + e.name + adminSocketSuffix
}

// MarshalText implements encoding.TextMarshaler. Serializes as the
// Matrix user ID: @localpart:server. This is the canonical external
// representation for entity references.
func (e entity) MarshalText() ([]byte, error) {
	if e.userID == "" {
		return nil, fmt.Errorf("cannot marshal zero-value entity ref")
	}
	return []byte(e.userID), nil
}
