// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// RevokeParams holds the parameters for machine credential revocation.
type RevokeParams struct {
	// Machine is the typed machine reference within the fleet.
	Machine ref.Machine

	// Reason is the human-readable reason for revocation, recorded in
	// the credential revocation event.
	Reason string
}

// revokeParams holds the CLI-specific parameters for the machine revoke command.
type revokeParams struct {
	cli.SessionConfig
	Reason string `json:"reason" flag:"reason" desc:"reason for revocation (recorded in revocation event)"`
}

func revokeCommand() *cli.Command {
	var params revokeParams

	return &cli.Command{
		Name:    "revoke",
		Summary: "Emergency credential revocation for a compromised machine",
		Description: `Immediately revoke all credentials for a machine and shut it down.

The argument is a machine reference — either a bare localpart (e.g.,
"bureau/fleet/prod/machine/worker-01") or a full Matrix user ID (e.g.,
"@bureau/fleet/prod/machine/worker-01:remote.server"). The @ sigil
distinguishes the two forms. When a bare localpart is given, the server
name is derived from the connected admin session.

This is the emergency response command for a compromised machine. It
executes three layers of defense:

  Layer 1 — Machine isolation (seconds): Deactivates the machine's Matrix
  account. The daemon detects the auth failure, destroys all sandboxes,
  and exits. No cooperation from the compromised machine is needed.

  Layer 2 — Access revocation and state cleanup (seconds): Kicks the
  machine from all rooms (homeserver rejects further writes). Then clears
  credential state events, machine key, and machine status. Publishes a
  credential revocation event for fleet-wide notification.

  Layer 3 — Token expiry (≤5 minutes): Outstanding service tokens expire
  via natural TTL. The daemon's emergency shutdown also pushes revocations
  to reachable services for faster invalidation.

After revocation, the machine name can be re-provisioned with
"bureau machine provision". Account deactivation may be permanent
depending on the homeserver — this is by design for emergency revocation.`,
		Usage: "bureau machine revoke <machine-ref> [flags]",
		Examples: []cli.Example{
			{
				Description: "Revoke a compromised machine",
				Command:     "bureau machine revoke bureau/fleet/prod/machine/worker-01 --credential-file ./bureau-creds --reason 'suspected compromise'",
			},
		},
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/machine/revoke"},
		Annotations:    cli.Destructive(),
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) == 0 {
				return cli.Validation("machine reference is required\n\nUsage: bureau machine revoke <machine-ref> [flags]")
			}
			if len(args) > 1 {
				return cli.Validation("expected exactly one argument (machine reference), got %d", len(args))
			}
			if params.SessionConfig.CredentialFile == "" {
				return cli.Validation("--credential-file is required")
			}

			ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()

			genericSession, err := params.SessionConfig.Connect(ctx)
			if err != nil {
				return cli.Internal("connect: %w", err)
			}
			defer genericSession.Close()

			adminSession, ok := genericSession.(*messaging.DirectSession)
			if !ok {
				return cli.Internal("revoke requires a direct session (credential file), not a proxy session")
			}

			defaultServer, err := ref.ServerFromUserID(adminSession.UserID().String())
			if err != nil {
				return cli.Internal("cannot determine server name from session: %w", err)
			}
			machine, err := resolveMachineArg(args[0], defaultServer)
			if err != nil {
				return cli.Validation("invalid machine reference: %v", err)
			}

			logger = logger.With(
				"machine", machine.Localpart(),
			)

			return Revoke(ctx, adminSession, RevokeParams{
				Machine: machine,
				Reason:  params.Reason,
			}, logger)
		},
	}
}

// Revoke performs emergency credential revocation for a compromised machine.
// It executes three layers of defense:
//   - Layer 1: Deactivates the machine's Matrix account (daemon self-destructs)
//   - Layer 2: Kicks from all rooms, then clears credential state events
//   - Layer 3: Outstanding service tokens expire via natural TTL
//
// The caller provides a DirectSession (required for account deactivation,
// password reset, and KickUser) and a context with an appropriate deadline.
func Revoke(ctx context.Context, adminSession *messaging.DirectSession, params RevokeParams, logger *slog.Logger) error {
	machine := params.Machine
	fleet := machine.Fleet()
	adminUserID := adminSession.UserID()
	machineUsername := machine.Localpart()
	machineUserID := machine.UserID()

	logger.Warn("emergency revocation initiated",
		"machine_user_id", machineUserID.String(),
		"reason", params.Reason,
	)

	// Layer 1: Deactivate the Matrix account. This causes the daemon to
	// detect an auth failure on its next /sync, triggering emergency
	// shutdown (sandbox destruction + exit). No cooperation needed from
	// the compromised machine.
	//
	// Try the deactivate endpoint first (Synapse). If the homeserver
	// doesn't support it (Continuwuity returns M_UNRECOGNIZED), fall
	// back to resetting the password with logout_devices=true. The
	// password reset invalidates all access tokens, producing the same
	// M_UNKNOWN_TOKEN auth failure on the daemon's next /sync.
	accountDeactivated := true
	logger.Info("deactivating machine account", "layer", 1)
	if err := adminSession.DeactivateUser(ctx, machineUserID, false); err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeUnrecognized) {
			logger.Info("deactivate endpoint not supported, falling back to password reset")
			randomPassword, passwordErr := generateRandomPassword()
			if passwordErr != nil {
				accountDeactivated = false
				logger.Warn("could not generate random password for fallback", "error", passwordErr)
			} else if resetErr := adminSession.ResetUserPassword(ctx, machineUserID, randomPassword, true); resetErr != nil {
				accountDeactivated = false
				logger.Warn("password reset failed", "error", resetErr)
			} else {
				logger.Info("password reset with token invalidation applied")
			}
		} else {
			accountDeactivated = false
			logger.Warn("account deactivation failed", "error", err)
		}
		if !accountDeactivated {
			logger.Info("continuing with state cleanup", "layer", 2)
		}
	} else {
		logger.Info("account deactivated")
	}

	// Layer 2: Clean up state events and kick from rooms.
	logger.Info("clearing state and revoking access", "layer", 2)

	// Resolve global Bureau rooms (template, pipeline, system) for kick.
	namespace := fleet.Namespace()
	globalRooms, failedRooms, resolveErrors := resolveGlobalRooms(ctx, adminSession, namespace)
	for index, room := range failedRooms {
		logger.Warn("could not resolve global room",
			"room", room.displayName,
			"error", resolveErrors[index],
		)
	}

	// Resolve the fleet-scoped rooms for state event cleanup and kicking.
	machineRoomID, serviceRoomID, fleetRoomID, err := resolveFleetRooms(ctx, adminSession, fleet)
	if err != nil {
		return cli.NotFound("fleet rooms could not be resolved — cannot clear machine state: %w", err)
	}

	// Kick the machine from ALL rooms first. This is the access
	// revocation: after the kick, the homeserver rejects all writes
	// from the machine. State event cleanup (below) is safe because
	// the machine can no longer interfere.
	//
	// Resolve the config room early so we can kick from it and then
	// clean up its state events.
	configAlias := machine.RoomAlias()
	configRoomID, err := adminSession.ResolveAlias(ctx, configAlias)
	configRoomExists := err == nil
	if err != nil && !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
		return cli.NotFound("resolve config room %q: %w", configAlias, err)
	}
	if !configRoomExists {
		logger.Info("config room does not exist, skipping", "alias", configAlias)
	}

	if configRoomExists {
		err = adminSession.KickUser(ctx, configRoomID, machineUserID, "machine credentials revoked")
		if err != nil {
			logger.Warn("could not kick from config room", "error", err)
		} else {
			logger.Info("kicked from config room")
		}
	}

	for _, room := range globalRooms {
		err = adminSession.KickUser(ctx, room.roomID, machineUserID, "machine credentials revoked")
		if err != nil {
			logger.Warn("could not kick from room", "room", room.alias, "error", err)
		} else {
			logger.Info("kicked from room", "room", room.alias)
		}
	}

	fleetRooms := []struct {
		roomID ref.RoomID
		name   string
	}{
		{machineRoomID, "fleet machine room"},
		{serviceRoomID, "fleet service room"},
		{fleetRoomID, "fleet config room"},
	}
	for _, room := range fleetRooms {
		err = adminSession.KickUser(ctx, room.roomID, machineUserID, "machine credentials revoked")
		if err != nil {
			logger.Warn("could not kick from room", "room", room.name, "error", err)
		} else {
			logger.Info("kicked from room", "room", room.name)
		}
	}

	// Clear state events. The machine is already kicked from all
	// rooms, so it cannot overwrite these cleared values.
	_, err = adminSession.SendStateEvent(ctx, machineRoomID, schema.EventTypeMachineKey, machineUsername, map[string]any{})
	if err != nil {
		logger.Warn("could not clear machine_key", "error", err)
	} else {
		logger.Info("cleared machine_key")
	}

	_, err = adminSession.SendStateEvent(ctx, machineRoomID, schema.EventTypeMachineStatus, machineUsername, map[string]any{})
	if err != nil {
		logger.Warn("could not clear machine_status", "error", err)
	} else {
		logger.Info("cleared machine_status")
	}

	var principals []string
	var credentialKeys []string

	if configRoomExists {
		_, err = adminSession.SendStateEvent(ctx, configRoomID, schema.EventTypeMachineConfig, machineUsername, map[string]any{})
		if err != nil {
			logger.Warn("could not clear machine_config", "error", err)
		} else {
			logger.Info("cleared machine_config")
		}

		cleared, err := clearConfigRoomCredentials(ctx, adminSession, configRoomID, logger)
		if err != nil {
			logger.Warn("could not clear config room credentials", "error", err)
		} else {
			principals = cleared
			credentialKeys = cleared
		}
	}

	// Publish a credential revocation event for fleet-wide notification.
	// Other machines and future connectors watch for this event to
	// invalidate cached tokens and revoke external API keys.
	revocationContent := schema.CredentialRevocationContent{
		Machine:            machineUsername,
		MachineUserID:      machineUserID,
		Principals:         principals,
		CredentialKeys:     credentialKeys,
		InitiatedBy:        adminUserID,
		InitiatedAt:        time.Now().UTC().Format(time.RFC3339),
		Reason:             params.Reason,
		AccountDeactivated: accountDeactivated,
	}
	_, err = adminSession.SendStateEvent(ctx, machineRoomID,
		schema.EventTypeCredentialRevocation, machineUsername, revocationContent)
	if err != nil {
		logger.Warn("could not publish revocation event", "error", err)
	} else {
		logger.Info("published credential revocation event")
	}

	// Revocation summary as a single structured event.
	logger.Info("revocation complete",
		"account_deactivated", accountDeactivated,
		"principals_affected", len(principals),
		"principals", principals,
	)
	if !accountDeactivated {
		logger.Warn("account deactivation failed, daemon may still be running with in-memory credentials until restart")
	}

	return nil
}

// generateRandomPassword returns a 64-character hex string from 32 random
// bytes. Used as the replacement password when falling back to password
// reset for account invalidation.
func generateRandomPassword() (string, error) {
	randomBytes := make([]byte, 32)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", cli.Internal("generate random bytes: %w", err)
	}
	return hex.EncodeToString(randomBytes), nil
}
