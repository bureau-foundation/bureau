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
	userID     UserID // pre-computed: "@my_bureau/fleet/prod/machine/gpu-box:server"
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
		userID:     MatrixUserID(localpart, server),
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
func (e entity) UserID() UserID { return e.userID }

// RoomAlias returns the entity's config room alias: #localpart:server.
// This is the @ -> # rule: swap the sigil to get the room.
func (e entity) RoomAlias() string { return e.roomAlias }

// Server returns the Matrix homeserver name.
func (e entity) Server() string { return e.fleet.ns.server }

// String returns the localpart, satisfying fmt.Stringer.
func (e entity) String() string { return e.localpart }

// IsZero reports whether this is an uninitialized zero-value entity.
func (e entity) IsZero() bool { return e.userID.IsZero() }

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
// representation for entity references. A zero-value entity marshals
// as empty string — used by service deregistration (publishing
// schema.Service{} to clear a directory entry).
func (e entity) MarshalText() ([]byte, error) {
	return e.userID.MarshalText()
}

// AccountLocalpart returns the bare account localpart (entityType/entityName)
// without the fleet prefix. This is the form used in MachineConfig principal
// assignments and daemon internal state (e.g., "agent/frontend",
// "service/ticket", "pipeline/deploy/123456").
func (e entity) AccountLocalpart() string {
	return e.entityType + "/" + e.name
}

// Entity is a parsed fleet-scoped entity reference where the entity
// type (machine, service, agent) is not known at the call site. Unlike
// Machine, Service, and Agent, Entity accepts any valid entity type.
//
// Use ParseEntityUserID or ParseEntityLocalpart when you have a Matrix
// user ID or localpart and need socket paths without caring about the
// specific entity type. All accessor methods (SocketPath, AdminSocketPath,
// Localpart, UserID, Fleet, Name, Server, etc.) are inherited.
//
// Entity implements encoding.TextMarshaler (via entity) and
// encoding.TextUnmarshaler, so it can be used as a JSON or CBOR struct
// field: serialized as the Matrix user ID string, deserialized by parsing
// that user ID back into a full Entity.
type Entity struct{ entity }

// UnmarshalText implements encoding.TextUnmarshaler. Parses a Matrix
// user ID (@localpart:server) into a full Entity reference. Empty
// input produces a zero value — the symmetric counterpart to
// MarshalText's zero-value behavior.
func (e *Entity) UnmarshalText(data []byte) error {
	if len(data) == 0 {
		*e = Entity{}
		return nil
	}
	parsed, err := ParseEntityUserID(string(data))
	if err != nil {
		return fmt.Errorf("unmarshal Entity: %w", err)
	}
	*e = parsed
	return nil
}

// EntityType returns the entity type string ("machine", "service", "agent").
func (e Entity) EntityType() string { return e.entityType }

// ParseEntityUserID parses a Matrix user ID (@localpart:server) into a
// generic Entity reference. The entity type is extracted from the
// localpart structure — callers do not need to know it in advance.
func ParseEntityUserID(userID string) (Entity, error) {
	localpart, server, err := parseMatrixID(userID)
	if err != nil {
		return Entity{}, fmt.Errorf("invalid entity ref: %w", err)
	}
	return ParseEntityLocalpart(localpart, server)
}

// ParseEntityLocalpart parses a fleet-scoped localpart and server into
// a generic Entity reference. The localpart must have at least 5
// segments (namespace/fleet/name/entityType/entityName).
func ParseEntityLocalpart(localpart, server string) (Entity, error) {
	namespace, fleetName, entityType, entityName, err := parseFleetLocalpart(localpart)
	if err != nil {
		return Entity{}, fmt.Errorf("invalid entity ref: %w", err)
	}
	ns, err := NewNamespace(server, namespace)
	if err != nil {
		return Entity{}, fmt.Errorf("invalid entity ref: %w", err)
	}
	fleet, err := NewFleet(ns, fleetName)
	if err != nil {
		return Entity{}, fmt.Errorf("invalid entity ref: %w", err)
	}
	e, err := newEntity(fleet, entityType, entityName)
	if err != nil {
		return Entity{}, err
	}
	return Entity{e}, nil
}

// NewEntityFromAccountLocalpart creates an Entity from a fleet and a bare
// account localpart. The account localpart must have the form
// "entityType/entityName" (e.g., "agent/frontend", "service/ticket",
// "pipeline/deploy/123456"). The entity type is extracted from the first
// segment; the entity name is everything after the first slash.
//
// This is the bridge between MachineConfig's bare localparts and the
// fleet-scoped ref system. The daemon uses this to construct IPC
// requests with fleet-scoped principals, and the launcher parses them
// back with ParseEntityLocalpart.
func NewEntityFromAccountLocalpart(fleet Fleet, accountLocalpart string) (Entity, error) {
	entityType, entityName, err := ExtractEntityName(accountLocalpart)
	if err != nil {
		return Entity{}, fmt.Errorf("invalid account localpart %q: %w", accountLocalpart, err)
	}
	e, err := newEntity(fleet, entityType, entityName)
	if err != nil {
		return Entity{}, err
	}
	return Entity{e}, nil
}
