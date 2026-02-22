// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
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

The first argument is a fleet localpart (e.g., "bureau/fleet/prod").
The second argument is the bare machine name within the fleet (e.g.,
"worker-01"). The server name is derived from the connected session's
identity.

This is the emergency response command for a compromised machine. It
executes three layers of defense:

  Layer 1 — Machine isolation (seconds): Deactivates the machine's Matrix
  account. The daemon detects the auth failure, destroys all sandboxes,
  and exits. No cooperation from the compromised machine is needed.

  Layer 2 — State cleanup (seconds): Clears credential state events,
  machine key, and machine status. Kicks the machine from all rooms.
  Publishes a credential revocation event for fleet-wide notification.

  Layer 3 — Token expiry (≤5 minutes): Outstanding service tokens expire
  via natural TTL. The daemon's emergency shutdown also pushes revocations
  to reachable services for faster invalidation.

After revocation, the machine name can be re-provisioned with
"bureau machine provision". Account deactivation may be permanent
depending on the homeserver — this is by design for emergency revocation.`,
		Usage: "bureau machine revoke <fleet-localpart> <machine-name> [flags]",
		Examples: []cli.Example{
			{
				Description: "Revoke a compromised machine",
				Command:     "bureau machine revoke bureau/fleet/prod worker-01 --credential-file ./bureau-creds --reason 'suspected compromise'",
			},
		},
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/machine/revoke"},
		Annotations:    cli.Destructive(),
		Run: func(args []string) error {
			if len(args) < 2 {
				return cli.Validation("fleet localpart and machine name are required\n\nUsage: bureau machine revoke <fleet-localpart> <machine-name> [flags]")
			}
			if len(args) > 2 {
				return cli.Validation("unexpected argument: %s", args[2])
			}
			if params.SessionConfig.CredentialFile == "" {
				return cli.Validation("--credential-file is required")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			credentials, err := cli.ReadCredentialFile(params.SessionConfig.CredentialFile)
			if err != nil {
				return cli.Internal("read credential file: %w", err)
			}

			homeserverURL := credentials["MATRIX_HOMESERVER_URL"]
			if homeserverURL == "" {
				return cli.Validation("credential file missing MATRIX_HOMESERVER_URL")
			}
			adminUserIDString := credentials["MATRIX_ADMIN_USER"]
			adminToken := credentials["MATRIX_ADMIN_TOKEN"]
			if adminUserIDString == "" || adminToken == "" {
				return cli.Validation("credential file missing MATRIX_ADMIN_USER or MATRIX_ADMIN_TOKEN")
			}

			adminUserID, err := ref.ParseUserID(adminUserIDString)
			if err != nil {
				return cli.Internal("parse admin user ID: %w", err)
			}

			server, err := ref.ServerFromUserID(adminUserIDString)
			if err != nil {
				return cli.Internal("cannot determine server name from admin user ID: %w", err)
			}

			fleet, err := ref.ParseFleet(args[0], server)
			if err != nil {
				return cli.Validation("%v", err)
			}
			machine, err := ref.NewMachine(fleet, args[1])
			if err != nil {
				return cli.Validation("invalid machine name: %v", err)
			}

			client, err := messaging.NewClient(messaging.ClientConfig{
				HomeserverURL: homeserverURL,
			})
			if err != nil {
				return cli.Internal("create matrix client: %w", err)
			}

			adminSession, err := client.SessionFromToken(adminUserID, adminToken)
			if err != nil {
				return cli.Internal("create admin session: %w", err)
			}
			defer adminSession.Close()

			return Revoke(ctx, adminSession, RevokeParams{
				Machine: machine,
				Reason:  params.Reason,
			})
		},
	}
}

// Revoke performs emergency credential revocation for a compromised machine.
// It executes three layers of defense:
//   - Layer 1: Deactivates the machine's Matrix account (daemon self-destructs)
//   - Layer 2: Clears credential state events, kicks from all rooms
//   - Layer 3: Outstanding service tokens expire via natural TTL
//
// The caller provides a DirectSession (required for account deactivation,
// password reset, and KickUser) and a context with an appropriate deadline.
func Revoke(ctx context.Context, adminSession *messaging.DirectSession, params RevokeParams) error {
	machine := params.Machine
	fleet := machine.Fleet()
	adminUserID := adminSession.UserID()
	machineUsername := machine.Localpart()
	machineUserID := machine.UserID()

	fmt.Fprintf(os.Stderr, "EMERGENCY REVOCATION: %s (%s)\n\n", machineUsername, machineUserID)

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
	fmt.Fprintf(os.Stderr, "Layer 1: Deactivating machine account...\n")
	if err := adminSession.DeactivateUser(ctx, machineUserID, false); err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeUnrecognized) {
			fmt.Fprintf(os.Stderr, "  Deactivate endpoint not supported, falling back to password reset...\n")
			randomPassword, passwordErr := generateRandomPassword()
			if passwordErr != nil {
				accountDeactivated = false
				fmt.Fprintf(os.Stderr, "  WARNING: could not generate random password: %v\n", passwordErr)
			} else if resetErr := adminSession.ResetUserPassword(ctx, machineUserID, randomPassword, true); resetErr != nil {
				accountDeactivated = false
				fmt.Fprintf(os.Stderr, "  WARNING: password reset failed: %v\n", resetErr)
			} else {
				fmt.Fprintf(os.Stderr, "  Password reset with token invalidation — daemon will self-destruct on next sync\n")
			}
		} else {
			accountDeactivated = false
			fmt.Fprintf(os.Stderr, "  WARNING: account deactivation failed: %v\n", err)
		}
		if !accountDeactivated {
			fmt.Fprintf(os.Stderr, "  Continuing with state cleanup (Layer 2)...\n")
		}
	} else {
		fmt.Fprintf(os.Stderr, "  Account deactivated — daemon will self-destruct on next sync\n")
	}

	// Layer 2: Clean up state events and kick from rooms.
	fmt.Fprintf(os.Stderr, "\nLayer 2: Clearing state and revoking access...\n")

	// Resolve global Bureau rooms (template, pipeline, system) for kick.
	namespace := fleet.Namespace()
	globalRooms, failedRooms, resolveErrors := resolveGlobalRooms(ctx, adminSession, namespace)
	for index, room := range failedRooms {
		fmt.Fprintf(os.Stderr, "  Warning: could not resolve %s: %v\n", room.displayName, resolveErrors[index])
	}

	// Resolve the fleet-scoped rooms for state event cleanup and kicking.
	machineRoomID, serviceRoomID, fleetRoomID, err := resolveFleetRooms(ctx, adminSession, fleet)
	if err != nil {
		return cli.NotFound("fleet rooms could not be resolved — cannot clear machine state: %w", err)
	}

	// Clear machine_key and machine_status.
	_, err = adminSession.SendStateEvent(ctx, machineRoomID, schema.EventTypeMachineKey, machineUsername, map[string]any{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Warning: could not clear machine_key: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "  Cleared machine_key\n")
	}

	_, err = adminSession.SendStateEvent(ctx, machineRoomID, schema.EventTypeMachineStatus, machineUsername, map[string]any{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Warning: could not clear machine_status: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "  Cleared machine_status\n")
	}

	// Resolve the config room and collect affected principals.
	var principals []string
	var credentialKeys []string
	configAlias := machine.RoomAlias()
	configRoomID, err := adminSession.ResolveAlias(ctx, configAlias)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			fmt.Fprintf(os.Stderr, "  Config room %s does not exist (skipping)\n", configAlias)
		} else {
			return cli.NotFound("resolve config room %q: %w", configAlias, err)
		}
	} else {
		// Clear machine_config.
		_, err = adminSession.SendStateEvent(ctx, configRoomID, schema.EventTypeMachineConfig, machineUsername, map[string]any{})
		if err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: could not clear machine_config: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "  Cleared machine_config\n")
		}

		// Clear all credentials and collect the affected principal names.
		cleared, err := clearConfigRoomCredentials(ctx, adminSession, configRoomID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: %v\n", err)
		} else {
			principals = cleared
			credentialKeys = cleared
		}

		err = adminSession.KickUser(ctx, configRoomID, machineUserID, "machine credentials revoked")
		if err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: could not kick from config room: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "  Kicked from config room\n")
		}
	}

	for _, room := range globalRooms {
		err = adminSession.KickUser(ctx, room.roomID, machineUserID, "machine credentials revoked")
		if err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: could not kick from %s: %v\n", room.alias, err)
		} else {
			fmt.Fprintf(os.Stderr, "  Kicked from %s\n", room.alias)
		}
	}

	// Kick from all fleet-scoped rooms.
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
			fmt.Fprintf(os.Stderr, "  Warning: could not kick from %s: %v\n", room.name, err)
		} else {
			fmt.Fprintf(os.Stderr, "  Kicked from %s\n", room.name)
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
		fmt.Fprintf(os.Stderr, "  Warning: could not publish revocation event: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "  Published credential revocation event to machine room\n")
	}

	// Print summary.
	fmt.Fprintf(os.Stderr, "\n--- Revocation Summary ---\n")
	fmt.Fprintf(os.Stderr, "Machine:            %s\n", machineUsername)
	fmt.Fprintf(os.Stderr, "Account deactivated: %v\n", accountDeactivated)
	fmt.Fprintf(os.Stderr, "Principals affected: %d\n", len(principals))
	for _, name := range principals {
		fmt.Fprintf(os.Stderr, "  - %s\n", name)
	}
	if !accountDeactivated {
		fmt.Fprintf(os.Stderr, "\nWARNING: Account deactivation failed. The daemon may still be\n")
		fmt.Fprintf(os.Stderr, "running. Credentials have been cleared from state, but the\n")
		fmt.Fprintf(os.Stderr, "daemon's in-memory copy persists until it restarts.\n")
	}
	fmt.Fprintf(os.Stderr, "\nOutstanding service tokens will expire within 5 minutes (TTL).\n")

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
