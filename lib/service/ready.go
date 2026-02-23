// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// ServiceReadyContent is the content of an m.bureau.service_ready
// timeline event. Services send this after joining a room and
// finishing room-specific initialization (parsing config, building
// indexes). The body field is human-readable; the structured fields
// are for machine consumers.
type ServiceReadyContent struct {
	Body         string   `json:"body"`
	ServiceType  string   `json:"service_type"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// AnnounceReady sends an m.bureau.service_ready event to a room,
// signaling that this service has joined, processed the room's
// configuration, and is ready to accept requests scoped to this room.
//
// Services should call this once per room, immediately after the room
// enters their tracking state. The event is a timeline message (not a
// state event) so it cannot be overwritten by other services — the
// homeserver-authenticated sender field identifies the source.
//
// Best-effort: logs a warning on failure but does not return an error.
// A failed ready announcement means consumers waiting via
// EnsureServiceInRoom will time out, which surfaces the problem
// clearly. Retrying here would delay the service's sync loop.
func AnnounceReady(ctx context.Context, session messaging.Session, roomID ref.RoomID, serviceType string, capabilities []string, logger *slog.Logger) {
	content := ServiceReadyContent{
		Body:         fmt.Sprintf("%s service ready", serviceType),
		ServiceType:  serviceType,
		Capabilities: capabilities,
	}
	if _, err := session.SendEvent(ctx, roomID, schema.EventTypeServiceReady, content); err != nil {
		logger.Warn("failed to announce service readiness",
			"room_id", roomID,
			"service_type", serviceType,
			"error", err,
		)
	}
}
