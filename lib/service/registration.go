// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// Registration describes a service for the fleet's service directory room.
type Registration struct {
	// Machine is the machine running this service instance.
	Machine ref.Machine

	// Endpoints maps endpoint names to socket filenames relative to
	// the service's listen directory. When nil, Register() uses the
	// default {"cbor": "service.sock"}.
	Endpoints map[string]string

	// Description is a human-readable summary shown in service
	// directory listings.
	Description string

	// Capabilities lists feature tags for this service instance.
	// Used by the daemon for service selection when a template
	// requests a capability (e.g., ["dependency-graph", "gates"]).
	Capabilities []string

	// Metadata holds arbitrary key-value pairs for service-specific
	// configuration visible in the directory.
	Metadata map[string]any
}

// Register publishes an m.bureau.service state event to the service
// room. The state key is the service's fleet-scoped localpart (e.g.,
// "bureau/fleet/prod/service/stt/whisper"). The daemon's syncServiceDirectory
// picks this up and makes the service available for routing.
func Register(ctx context.Context, session messaging.Session, serviceRoomID ref.RoomID, svc ref.Service, reg Registration) error {
	stateKey := svc.Localpart()
	endpoints := reg.Endpoints
	if endpoints == nil {
		endpoints = map[string]string{"cbor": "service.sock"}
	}
	entry := schema.Service{
		Principal:    svc.Entity(),
		Machine:      reg.Machine,
		Endpoints:    endpoints,
		Description:  reg.Description,
		Capabilities: reg.Capabilities,
		Metadata:     reg.Metadata,
	}

	if _, err := session.SendStateEvent(ctx, serviceRoomID, schema.EventTypeService, stateKey, entry); err != nil {
		return fmt.Errorf("registering service %s in %s: %w", stateKey, serviceRoomID, err)
	}
	return nil
}

// Deregister clears the service registration from the service directory
// by publishing a state event with an empty Principal field. The daemon's
// syncServiceDirectory skips entries with empty principals, effectively
// removing the service from the directory.
func Deregister(ctx context.Context, session messaging.Session, serviceRoomID ref.RoomID, svc ref.Service) error {
	stateKey := svc.Localpart()
	empty := schema.Service{}
	if _, err := session.SendStateEvent(ctx, serviceRoomID, schema.EventTypeService, stateKey, empty); err != nil {
		return fmt.Errorf("deregistering service %s from %s: %w", stateKey, serviceRoomID, err)
	}
	return nil
}
