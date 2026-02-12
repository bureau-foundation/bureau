// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// Registration describes a service for the #bureau/service directory.
type Registration struct {
	// Machine is the full Matrix user ID of the machine running this
	// service instance (e.g., "@machine/workstation:bureau.local").
	Machine string

	// Protocol is the wire protocol on the service socket. Bureau
	// services use "ndjson" (newline-delimited JSON, one request-
	// response per connection). External services may use "http".
	Protocol string

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
// room. The state key is the service's localpart. The daemon's
// syncServiceDirectory picks this up and makes the service available
// for routing.
func Register(ctx context.Context, session *messaging.Session, serviceRoomID, localpart, serverName string, reg Registration) error {
	service := schema.Service{
		Principal:    principal.MatrixUserID(localpart, serverName),
		Machine:      reg.Machine,
		Protocol:     reg.Protocol,
		Description:  reg.Description,
		Capabilities: reg.Capabilities,
		Metadata:     reg.Metadata,
	}

	if _, err := session.SendStateEvent(ctx, serviceRoomID, schema.EventTypeService, localpart, service); err != nil {
		return fmt.Errorf("registering service %s in %s: %w", localpart, serviceRoomID, err)
	}
	return nil
}

// Deregister clears the service registration from #bureau/service by
// publishing a state event with an empty Principal field. The daemon's
// syncServiceDirectory skips entries with empty principals, effectively
// removing the service from the directory.
func Deregister(ctx context.Context, session *messaging.Session, serviceRoomID, localpart string) error {
	empty := schema.Service{}
	if _, err := session.SendStateEvent(ctx, serviceRoomID, schema.EventTypeService, localpart, empty); err != nil {
		return fmt.Errorf("deregistering service %s from %s: %w", localpart, serviceRoomID, err)
	}
	return nil
}

// ResolveServiceRoom resolves the #bureau/service room alias and joins
// it. Returns the room ID. This is called once at startup to establish
// the service's connection to the service directory room.
func ResolveServiceRoom(ctx context.Context, session *messaging.Session, serverName string) (string, error) {
	alias := principal.RoomAlias("bureau/service", serverName)

	roomID, err := session.ResolveAlias(ctx, alias)
	if err != nil {
		return "", fmt.Errorf("resolving service room alias %q: %w", alias, err)
	}

	if _, err := session.JoinRoom(ctx, roomID); err != nil {
		return "", fmt.Errorf("joining service room %s: %w", roomID, err)
	}

	return roomID, nil
}
