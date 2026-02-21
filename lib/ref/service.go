// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

//nolint:dupl // Machine, Service, and Agent are structurally identical by design — distinct types for compile-time safety.
package ref

import "fmt"

// Service identifies a service entity within a fleet. Services are
// long-running processes that provide capabilities to agents and other
// services (e.g., "stt/whisper", "fleet", "ticket").
//
// Service embeds an unexported entity type, which provides all
// accessor methods. See Machine for the full method list.
type Service struct{ entity }

// NewService creates a validated Service reference. The name may contain
// slashes for hierarchical service names (e.g., "stt/whisper").
func NewService(fleet Fleet, name string) (Service, error) {
	ent, err := newEntity(fleet, entityTypeService, name)
	if err != nil {
		return Service{}, err
	}
	return Service{entity: ent}, nil
}

// ParseServiceUserID parses a full Matrix user ID into a Service.
func ParseServiceUserID(userID string) (Service, error) {
	ent, err := parseEntityUserID(userID, entityTypeService)
	if err != nil {
		return Service{}, err
	}
	return Service{entity: ent}, nil
}

// ParseService parses a fleet-scoped localpart and server into a Service.
func ParseService(localpart string, server ServerName) (Service, error) {
	ent, err := parseEntityLocalpart(localpart, server, entityTypeService)
	if err != nil {
		return Service{}, err
	}
	return Service{entity: ent}, nil
}

// Entity converts this Service to a generic Entity reference. This is a
// zero-cost type conversion — Machine, Service, and Entity all embed the
// same unexported entity struct.
func (s Service) Entity() Entity { return Entity{s.entity} }

// UnmarshalText implements encoding.TextUnmarshaler. Empty input
// produces a zero value.
func (s *Service) UnmarshalText(data []byte) error {
	if len(data) == 0 {
		*s = Service{}
		return nil
	}
	parsed, err := ParseServiceUserID(string(data))
	if err != nil {
		return fmt.Errorf("unmarshal Service: %w", err)
	}
	*s = parsed
	return nil
}
