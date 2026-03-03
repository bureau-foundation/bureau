// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"log/slog"

	"github.com/bureau-foundation/bureau/cmd/bureau/fleet"
	libservice "github.com/bureau-foundation/bureau/lib/service"
)

// tryFleetConnect attempts to connect to the fleet controller socket.
// Returns a ServiceClient if successful, or nil if the fleet controller
// is not reachable. Connection failures are logged at debug level —
// the fleet controller is an optional enrichment source for list/show
// commands, not a hard requirement.
func tryFleetConnect(connection *fleet.FleetConnection, logger *slog.Logger) *libservice.ServiceClient {
	client, err := connection.Connect()
	if err != nil {
		logger.Debug("fleet controller not reachable, showing Matrix-only results", "error", err)
		return nil
	}
	return client
}
