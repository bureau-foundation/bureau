// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ref

import "fmt"

// Namespace identifies an organizational root on a Matrix homeserver.
// A namespace owns infrastructure definitions (templates, pipelines,
// artifacts) and contains fleets. Different namespaces on the same
// homeserver are fully independent trust boundaries.
//
// Size: ~32 bytes (two string headers). Alias methods compute strings
// on demand via simple concatenation.
type Namespace struct {
	server    string
	namespace string
}

// NewNamespace creates a validated Namespace reference.
// The namespace name must be a single path segment (no slashes)
// using only lowercase letters, digits, and the symbols . _ = -.
func NewNamespace(server, namespace string) (Namespace, error) {
	if err := validateServer(server); err != nil {
		return Namespace{}, fmt.Errorf("invalid namespace: %w", err)
	}
	if namespace == "" {
		return Namespace{}, fmt.Errorf("invalid namespace: namespace name is empty")
	}
	if err := validateSegment(namespace, "namespace name"); err != nil {
		return Namespace{}, fmt.Errorf("invalid namespace: %w", err)
	}
	if err := validatePath(namespace, "namespace name"); err != nil {
		return Namespace{}, fmt.Errorf("invalid namespace: %w", err)
	}
	return Namespace{server: server, namespace: namespace}, nil
}

// Server returns the Matrix homeserver name.
func (n Namespace) Server() string { return n.server }

// Name returns the namespace name (e.g., "my_bureau").
func (n Namespace) Name() string { return n.namespace }

// String returns the namespace name, satisfying fmt.Stringer.
func (n Namespace) String() string { return n.namespace }

// IsZero reports whether this is an uninitialized zero-value Namespace.
func (n Namespace) IsZero() bool { return n.server == "" && n.namespace == "" }

// SpaceAlias returns the Matrix space alias: #namespace:server.
func (n Namespace) SpaceAlias() RoomAlias {
	return newRoomAlias(n.namespace, n.server)
}

// SystemRoomAlias returns the operational messages room alias.
func (n Namespace) SystemRoomAlias() RoomAlias {
	return newRoomAlias(n.namespace+"/system", n.server)
}

// TemplateRoomAlias returns the sandbox template definitions room alias.
func (n Namespace) TemplateRoomAlias() RoomAlias {
	return newRoomAlias(n.namespace+"/template", n.server)
}

// PipelineRoomAlias returns the pipeline definitions room alias.
func (n Namespace) PipelineRoomAlias() RoomAlias {
	return newRoomAlias(n.namespace+"/pipeline", n.server)
}

// ArtifactRoomAlias returns the artifact metadata room alias.
func (n Namespace) ArtifactRoomAlias() RoomAlias {
	return newRoomAlias(n.namespace+"/artifact", n.server)
}

// SpaceAliasLocalpart returns the localpart of the space alias
// (without sigil or server): the namespace name itself.
func (n Namespace) SpaceAliasLocalpart() string {
	return n.namespace
}

// SystemRoomAliasLocalpart returns the localpart of the system room alias.
func (n Namespace) SystemRoomAliasLocalpart() string {
	return n.namespace + "/system"
}

// TemplateRoomAliasLocalpart returns the localpart of the template room alias.
func (n Namespace) TemplateRoomAliasLocalpart() string {
	return n.namespace + "/template"
}

// PipelineRoomAliasLocalpart returns the localpart of the pipeline room alias.
func (n Namespace) PipelineRoomAliasLocalpart() string {
	return n.namespace + "/pipeline"
}

// ArtifactRoomAliasLocalpart returns the localpart of the artifact room alias.
func (n Namespace) ArtifactRoomAliasLocalpart() string {
	return n.namespace + "/artifact"
}

// MarshalText implements encoding.TextMarshaler. Serializes as the
// space alias form: #namespace:server.
func (n Namespace) MarshalText() ([]byte, error) {
	if n.IsZero() {
		return nil, fmt.Errorf("cannot marshal zero-value Namespace")
	}
	return []byte(n.SpaceAlias().String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler. Parses the
// space alias form: #namespace:server.
func (n *Namespace) UnmarshalText(data []byte) error {
	localpart, server, err := parseRoomAlias(string(data))
	if err != nil {
		return fmt.Errorf("invalid Namespace: %w", err)
	}
	parsed, err := NewNamespace(server, localpart)
	if err != nil {
		return err
	}
	*n = parsed
	return nil
}
