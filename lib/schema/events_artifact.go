// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"errors"
	"fmt"
)

// ArtifactScopeVersion is the current schema version for
// ArtifactScope events. Increment when adding fields that
// existing code must not silently drop during read-modify-write.
const ArtifactScopeVersion = 1

// ArtifactScope is the content of an EventTypeArtifactScope state event.
// Room-level configuration that connects a room to the artifact service:
// which service principal manages artifacts for this room, and which
// tag name patterns the room subscribes to for notifications.
//
// Per-artifact metadata and per-tag mappings are owned by the artifact
// service's persistent store, not Matrix state events. This avoids
// scaling issues (100K+ artifacts would overwhelm Matrix /sync).
type ArtifactScope struct {
	// Version is the schema version (see ArtifactScopeVersion).
	// Code that modifies this event must call CanModify() first; if
	// Version exceeds ArtifactScopeVersion, the modification is
	// refused to prevent silent field loss. Readers may process any
	// version (unknown fields are harmlessly ignored by Go's JSON
	// unmarshaler).
	Version int `json:"version"`

	// ServicePrincipal is the Matrix user ID of the artifact service
	// instance that manages artifacts for this room
	// (e.g., "@service/artifact/main:bureau.local").
	ServicePrincipal string `json:"service_principal"`

	// TagGlobs is a list of tag name patterns (glob syntax) that
	// this room subscribes to. The artifact service pushes
	// notifications to the room when matching tags are created or
	// updated. Example: ["iree/resnet50/**", "shared/datasets/*"].
	TagGlobs []string `json:"tag_globs,omitempty"`
}

// Validate checks that all required fields are present and well-formed.
func (a *ArtifactScope) Validate() error {
	if a.Version < 1 {
		return fmt.Errorf("artifact scope: version must be >= 1, got %d", a.Version)
	}
	if a.ServicePrincipal == "" {
		return errors.New("artifact scope: service_principal is required")
	}
	return nil
}

// CanModify checks whether this code version can safely perform a
// read-modify-write cycle on this event. Returns nil if safe, or an
// error explaining why modification would risk data loss.
//
// If the event's Version exceeds ArtifactScopeVersion, this code does
// not understand all fields in the event. Marshaling the modified struct
// back to JSON would silently drop the unknown fields. The caller must
// either upgrade or refuse the operation.
//
// Read-only access does not require CanModify — unknown fields are
// harmlessly ignored during display, routing, and service discovery.
func (a *ArtifactScope) CanModify() error {
	if a.Version > ArtifactScopeVersion {
		return fmt.Errorf(
			"artifact scope version %d exceeds supported version %d: "+
				"modification would lose fields added in newer versions; "+
				"upgrade before modifying this event",
			a.Version, ArtifactScopeVersion,
		)
	}
	return nil
}

// ArtifactRoomPowerLevels returns the power level structure for the
// artifact coordination room (#bureau/artifact). Per-artifact metadata
// and tag mappings live in the artifact service, not as Matrix state
// events. The room carries only admin-level configuration
// (EventTypeArtifactScope) and coordination messages.
//
// Room membership is invite-only — the admin invites artifact service
// principals and machine daemons during setup.
func ArtifactRoomPowerLevels(adminUserID string) map[string]any {
	return roomPowerLevels(powerLevelSpec{
		users: map[string]any{
			adminUserID: 100,
		},
		events:        AdminProtectedEvents(),
		eventsDefault: 100,
		stateDefault:  100,
		invite:        100,
	})
}
