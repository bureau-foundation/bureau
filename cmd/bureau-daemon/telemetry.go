// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// telemetryServiceRole is the service role name for the per-machine
// telemetry relay. The daemon resolves this role from the config room's
// m.bureau.service_binding state to find the relay's socket path. The
// proxy uses a token with this audience to authenticate its telemetry
// submissions.
const telemetryServiceRole = "telemetry"

// resolveTelemetrySocket looks up the telemetry service binding in the
// machine's config room and returns the host-side socket path to the
// telemetry relay. Returns an empty string if no binding exists (telemetry
// relay not deployed) or if resolution fails (logged as a warning — not
// a fatal condition).
//
// Unlike resolveServiceSocket, this method treats all failures as
// non-fatal: telemetry is an optional enhancement, not a required
// dependency. Proxies start without telemetry when the relay is absent
// and log a diagnostic message at startup.
//
// Caches the result in d.telemetrySocketPath so the token refresh loop
// can include the telemetry role without re-resolving the binding.
// The caller must hold reconcileMu (which reconcile() always does).
func (d *Daemon) resolveTelemetrySocket(ctx context.Context) string {
	content, err := d.session.GetStateEvent(ctx, d.configRoomID, schema.EventTypeServiceBinding, telemetryServiceRole)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			// No telemetry binding — relay not deployed on this machine.
			d.telemetrySocketPath = ""
			return ""
		}
		d.logger.Warn("failed to resolve telemetry service binding",
			"error", err,
		)
		d.telemetrySocketPath = ""
		return ""
	}

	var binding schema.ServiceBindingContent
	if err := json.Unmarshal(content, &binding); err != nil {
		d.logger.Warn("failed to parse telemetry service binding",
			"error", err,
		)
		d.telemetrySocketPath = ""
		return ""
	}

	if binding.Principal.IsZero() {
		d.logger.Warn("telemetry service binding has empty principal")
		d.telemetrySocketPath = ""
		return ""
	}

	socketPath := binding.Principal.ServiceSocketPath(d.fleetRunDir)
	d.logger.Info("resolved telemetry relay binding",
		"principal", binding.Principal,
		"socket", socketPath,
	)
	d.telemetrySocketPath = socketPath
	return socketPath
}

// telemetryServiceRoles returns ["telemetry"] if a telemetry relay is
// deployed on this machine, or nil otherwise. Used when computing the
// full set of service roles that need token minting and refresh.
//
// Reads d.telemetrySocketPath which is set by resolveTelemetrySocket
// during each reconcile cycle.
func (d *Daemon) telemetryServiceRoles() []string {
	if d.telemetrySocketPath != "" {
		return []string{telemetryServiceRole}
	}
	return nil
}
