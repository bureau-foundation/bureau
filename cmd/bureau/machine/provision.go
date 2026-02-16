// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/bootstrap"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/secret"
	"github.com/bureau-foundation/bureau/messaging"
)

// provisionParams holds the parameters for the machine provision command.
// Credential file paths are excluded from MCP schema since they involve reading
// secrets from files.
type provisionParams struct {
	CredentialFile string `json:"-"            flag:"credential-file" desc:"path to Bureau credential file from 'bureau matrix setup' (required)"`
	ServerName     string `json:"server_name"  flag:"server-name"     desc:"Matrix server name for constructing user IDs" default:"bureau.local"`
	FleetRoom      string `json:"fleet_room"   flag:"fleet-room"      desc:"fleet room alias or ID to invite machine to (optional)"`
	OutputPath     string `json:"-"            flag:"output"          desc:"path to write bootstrap config (default: stdout)"`
}

func provisionCommand() *cli.Command {
	var params provisionParams

	return &cli.Command{
		Name:    "provision",
		Summary: "Create a machine account and bootstrap config",
		Description: `Provision a new machine for the Bureau fleet.

This creates the machine's Matrix account with a random one-time password,
sets up its per-machine config room, and invites it to the global rooms
(machines, services). The output is a bootstrap config file that should
be transferred to the new machine.

On the new machine, start the launcher with --bootstrap-file to complete
registration. The launcher will log in with the one-time password, generate
its keypair, rotate the password to a locally-derived value, and publish
its key. After rotation, the one-time password in the bootstrap config is
useless.

This is more secure than passing the registration token to every machine,
because the registration token derives admin access to the entire deployment.
The one-time password only grants access to a single machine account and
is immediately rotated.`,
		Usage: "bureau machine provision <machine-name> [flags]",
		Examples: []cli.Example{
			{
				Description: "Provision and write config to a file",
				Command:     "bureau machine provision machine/worker-01 --credential-file ./bureau-creds --output bootstrap.json",
			},
			{
				Description: "Provision and pipe config to scp",
				Command:     "bureau machine provision machine/gpu-box --credential-file ./bureau-creds | ssh user@host 'cat > /tmp/bootstrap.json'",
			},
		},
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/machine/provision"},
		Annotations:    cli.Create(),
		Run: func(args []string) error {
			if len(args) < 1 {
				return cli.Validation("machine name is required\n\nUsage: bureau machine provision <machine-name> [flags]")
			}
			machineName := args[0]
			if len(args) > 1 {
				return cli.Validation("unexpected argument: %s", args[1])
			}
			if params.CredentialFile == "" {
				return cli.Validation("--credential-file is required")
			}
			if err := principal.ValidateLocalpart(machineName); err != nil {
				return cli.Validation("invalid machine name: %w", err)
			}

			return runProvision(machineName, params.CredentialFile, params.ServerName, params.FleetRoom, params.OutputPath)
		},
	}
}

func runProvision(machineName, credentialFile, serverName, fleetRoom, outputPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Read the credential file for admin access and the registration token.
	credentials, err := cli.ReadCredentialFile(credentialFile)
	if err != nil {
		return cli.Internal("read credential file: %w", err)
	}

	registrationToken := credentials["MATRIX_REGISTRATION_TOKEN"]
	if registrationToken == "" {
		return cli.Validation("credential file missing MATRIX_REGISTRATION_TOKEN")
	}
	homeserverURL := credentials["MATRIX_HOMESERVER_URL"]
	if homeserverURL == "" {
		return cli.Validation("credential file missing MATRIX_HOMESERVER_URL")
	}
	adminUserID := credentials["MATRIX_ADMIN_USER"]
	adminToken := credentials["MATRIX_ADMIN_TOKEN"]
	if adminUserID == "" || adminToken == "" {
		return cli.Validation("credential file missing MATRIX_ADMIN_USER or MATRIX_ADMIN_TOKEN")
	}

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: homeserverURL,
	})
	if err != nil {
		return cli.Internal("create matrix client: %w", err)
	}

	// Generate a random one-time password (32 bytes = 64 hex chars).
	// The raw bytes are zeroed after hex encoding, and the hex bytes
	// are moved into an mmap-backed buffer.
	passwordBytes := make([]byte, 32)
	if _, err := rand.Read(passwordBytes); err != nil {
		return cli.Internal("generate random password: %w", err)
	}
	hexBytes := []byte(hex.EncodeToString(passwordBytes))
	secret.Zero(passwordBytes)
	oneTimePassword, err := secret.NewFromBytes(hexBytes)
	if err != nil {
		return cli.Internal("protecting one-time password: %w", err)
	}
	defer oneTimePassword.Close()

	registrationTokenBuffer, err := secret.NewFromString(registrationToken)
	if err != nil {
		return cli.Internal("protecting registration token: %w", err)
	}
	defer registrationTokenBuffer.Close()

	// Register the machine account. Use the one-time password (NOT a
	// derived password from the registration token). The launcher will
	// rotate this password on first boot.
	machineUserID := principal.MatrixUserID(machineName, serverName)
	fmt.Fprintf(os.Stderr, "Registering machine account %s...\n", machineUserID)

	_, registerError := client.Register(ctx, messaging.RegisterRequest{
		Username:          machineName,
		Password:          oneTimePassword,
		RegistrationToken: registrationTokenBuffer,
	})
	if registerError != nil {
		if messaging.IsMatrixError(registerError, messaging.ErrCodeUserInUse) {
			return cli.Conflict("machine account %s already exists (use 'bureau machine decommission' first to re-provision)", machineUserID)
		}
		return cli.Internal("register machine account: %w", registerError)
	}
	fmt.Fprintf(os.Stderr, "  Account created: %s\n", machineUserID)

	// Get an admin session for room management.
	adminSession, err := client.SessionFromToken(adminUserID, adminToken)
	if err != nil {
		return cli.Internal("create admin session: %w", err)
	}
	defer adminSession.Close()

	// Invite the machine to the global rooms.
	machineAlias := principal.RoomAlias("bureau/machine", serverName)
	machineRoomID, err := adminSession.ResolveAlias(ctx, machineAlias)
	if err != nil {
		return cli.NotFound("resolve machine room %q: %w", machineAlias, err)
	}
	if err := adminSession.InviteUser(ctx, machineRoomID, machineUserID); err != nil {
		if !messaging.IsMatrixError(err, messaging.ErrCodeForbidden) {
			return cli.Internal("invite machine to machine room: %w", err)
		}
		fmt.Fprintf(os.Stderr, "  Already invited to %s\n", machineAlias)
	} else {
		fmt.Fprintf(os.Stderr, "  Invited to %s\n", machineAlias)
	}

	serviceAlias := principal.RoomAlias("bureau/service", serverName)
	serviceRoomID, err := adminSession.ResolveAlias(ctx, serviceAlias)
	if err != nil {
		return cli.NotFound("resolve service room %q: %w", serviceAlias, err)
	}
	if err := adminSession.InviteUser(ctx, serviceRoomID, machineUserID); err != nil {
		if !messaging.IsMatrixError(err, messaging.ErrCodeForbidden) {
			return cli.Internal("invite machine to service room: %w", err)
		}
		fmt.Fprintf(os.Stderr, "  Already invited to %s\n", serviceAlias)
	} else {
		fmt.Fprintf(os.Stderr, "  Invited to %s\n", serviceAlias)
	}

	// System room: daemons need this for token signing key retrieval.
	systemAlias := principal.RoomAlias(schema.RoomAliasSystem, serverName)
	systemRoomID, err := adminSession.ResolveAlias(ctx, systemAlias)
	if err != nil {
		return cli.NotFound("resolve system room %q: %w", systemAlias, err)
	}
	if err := adminSession.InviteUser(ctx, systemRoomID, machineUserID); err != nil {
		if !messaging.IsMatrixError(err, messaging.ErrCodeForbidden) {
			return cli.Internal("invite machine to system room: %w", err)
		}
		fmt.Fprintf(os.Stderr, "  Already invited to %s\n", systemAlias)
	} else {
		fmt.Fprintf(os.Stderr, "  Invited to %s\n", systemAlias)
	}

	// Fleet room: needed for HA leases and service definitions.
	if fleetRoom != "" {
		fleetRoomID, err := resolveRoomTarget(ctx, adminSession, fleetRoom, serverName)
		if err != nil {
			return cli.NotFound("resolve fleet room %q: %w", fleetRoom, err)
		}
		if err := adminSession.InviteUser(ctx, fleetRoomID, machineUserID); err != nil {
			if !messaging.IsMatrixError(err, messaging.ErrCodeForbidden) {
				return cli.Internal("invite machine to fleet room: %w", err)
			}
			fmt.Fprintf(os.Stderr, "  Already invited to fleet room %s\n", fleetRoom)
		} else {
			fmt.Fprintf(os.Stderr, "  Invited to fleet room %s\n", fleetRoom)
		}
	}

	// Create the per-machine config room. The admin creates it (not the
	// machine) so the admin has PL 100 from the start. The machine account
	// is invited.
	configAlias := principal.RoomAlias("bureau/config/"+machineName, serverName)
	configAliasLocalpart := principal.RoomAliasLocalpart(configAlias)

	fmt.Fprintf(os.Stderr, "Creating config room %s...\n", configAlias)
	configRoom, createError := adminSession.CreateRoom(ctx, messaging.CreateRoomRequest{
		Name:       "Config: " + machineName,
		Topic:      "Machine configuration and credentials for " + machineName,
		Alias:      configAliasLocalpart,
		Preset:     "private_chat",
		Invite:     []string{machineUserID},
		Visibility: "private",
	})
	if createError != nil {
		if messaging.IsMatrixError(createError, messaging.ErrCodeRoomInUse) {
			fmt.Fprintf(os.Stderr, "  Config room already exists\n")
		} else {
			return cli.Internal("create config room: %w", createError)
		}
	} else {
		// Set power levels: admin=100, machine=50. Machine can invite
		// and write layouts but cannot modify config or credentials.
		_, err = adminSession.SendStateEvent(ctx, configRoom.RoomID, "m.room.power_levels", "",
			schema.ConfigRoomPowerLevels(adminUserID, machineUserID))
		if err != nil {
			return cli.Internal("set config room power levels: %w", err)
		}
		fmt.Fprintf(os.Stderr, "  Config room created: %s\n", configRoom.RoomID)
	}

	// Write the bootstrap config.
	config := &bootstrap.Config{
		HomeserverURL: homeserverURL,
		ServerName:    serverName,
		MachineName:   machineName,
		Password:      oneTimePassword.String(),
	}

	if outputPath != "" {
		if err := bootstrap.WriteConfig(outputPath, config); err != nil {
			return cli.Internal("write bootstrap config: %w", err)
		}
		fmt.Fprintf(os.Stderr, "\nBootstrap config written to %s\n", outputPath)
	} else {
		if err := bootstrap.WriteToStdout(config); err != nil {
			return cli.Internal("write bootstrap config to stdout: %w", err)
		}
	}

	fmt.Fprintf(os.Stderr, "\nTransfer the bootstrap config to the new machine and run:\n")
	fmt.Fprintf(os.Stderr, "  bureau-launcher --bootstrap-file <config> --first-boot-only\n")
	fmt.Fprintf(os.Stderr, "\nThe one-time password will be rotated on first boot.\n")

	return nil
}

// resolveRoomTarget resolves a room target to a room ID. Accepts:
//   - "#alias:server" — resolved via the homeserver
//   - "!roomid:server" — returned as-is
func resolveRoomTarget(ctx context.Context, session *messaging.Session, target, serverName string) (string, error) {
	if strings.HasPrefix(target, "#") {
		roomID, err := session.ResolveAlias(ctx, target)
		if err != nil {
			return "", fmt.Errorf("resolve alias %q: %w", target, err)
		}
		return roomID, nil
	}
	if strings.HasPrefix(target, "!") {
		return target, nil
	}
	return "", fmt.Errorf("room must be an alias (#...) or room ID (!...): got %q", target)
}
