// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/bootstrap"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/secret"
	"github.com/bureau-foundation/bureau/messaging"
)

// ProvisionParams holds the parameters for machine provisioning.
type ProvisionParams struct {
	// Machine is the typed machine reference within the fleet.
	Machine ref.Machine

	// HomeserverURL is the Matrix homeserver URL, included in the
	// bootstrap config so the new machine knows where to connect.
	HomeserverURL string

	// RegistrationToken is the Matrix registration token for creating
	// the machine account.
	RegistrationToken *secret.Buffer
}

// ProvisionResult holds the result of machine provisioning.
type ProvisionResult struct {
	// Config is the bootstrap configuration for the new machine.
	Config *bootstrap.Config
}

// provisionParams holds the CLI-specific parameters for the machine provision command.
type provisionParams struct {
	cli.SessionConfig
	OutputPath string `json:"-" flag:"output" desc:"path to write bootstrap config (default: stdout)"`
}

func provisionCommand() *cli.Command {
	var params provisionParams

	return &cli.Command{
		Name:    "provision",
		Summary: "Create a machine account and bootstrap config",
		Description: `Provision a new machine for the Bureau fleet.

The first argument is a fleet localpart (e.g., "bureau/fleet/prod").
The second argument is the bare machine name within the fleet (e.g.,
"worker-01"). The server name is derived from the connected session's
identity. The full machine localpart is derived from these:
bureau/fleet/prod/machine/worker-01.

This creates the machine's Matrix account with a random one-time password,
sets up its per-machine config room, and invites it to the global rooms
(system, template, pipeline) and fleet-scoped rooms (machine, service,
fleet config). The output is a bootstrap config file that should be
transferred to the new machine.

On the new machine, start the launcher with --bootstrap-file to complete
registration. The launcher will log in with the one-time password, generate
its keypair, rotate the password to a locally-derived value, and publish
its key. After rotation, the one-time password in the bootstrap config is
useless.

This is more secure than passing the registration token to every machine,
because the registration token derives admin access to the entire deployment.
The one-time password only grants access to a single machine account and
is immediately rotated.

If a machine was previously decommissioned and the account already exists,
provision verifies it has been fully decommissioned (zero Bureau room
memberships, cleared state events) before re-provisioning. This prevents
accidental or intentional spoofing of active machines.`,
		Usage: "bureau machine provision <fleet-localpart> <machine-name> [flags]",
		Examples: []cli.Example{
			{
				Description: "Provision and write config to a file",
				Command:     "bureau machine provision bureau/fleet/prod worker-01 --credential-file ./bureau-creds --output bootstrap.json",
			},
			{
				Description: "Provision and pipe config to scp",
				Command:     "bureau machine provision bureau/fleet/prod gpu-box --credential-file ./bureau-creds | ssh user@host 'cat > /tmp/bootstrap.json'",
			},
		},
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/machine/provision"},
		Annotations:    cli.Create(),
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) < 2 {
				return cli.Validation("fleet localpart and machine name are required\n\nUsage: bureau machine provision <fleet-localpart> <machine-name> [flags]")
			}
			if len(args) > 2 {
				return cli.Validation("unexpected argument: %s", args[2])
			}
			if params.SessionConfig.CredentialFile == "" {
				return cli.Validation("--credential-file is required")
			}

			ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()

			// Connect for admin operations (room management, invites,
			// power levels). Uses SessionConfig for consistency with
			// decommission and revoke.
			genericSession, err := params.SessionConfig.Connect(ctx)
			if err != nil {
				return cli.Internal("connect: %w", err)
			}
			defer genericSession.Close()

			adminSession, ok := genericSession.(*messaging.DirectSession)
			if !ok {
				return cli.Internal("provision requires a direct session (credential file), not a proxy session")
			}

			// The credential file also contains the registration token,
			// which SessionConfig.Connect() doesn't extract. Read it
			// separately for account registration.
			credentials, err := cli.ReadCredentialFile(params.SessionConfig.CredentialFile)
			if err != nil {
				return cli.Internal("read credential file: %w", err)
			}

			registrationToken := credentials["MATRIX_REGISTRATION_TOKEN"]
			if registrationToken == "" {
				return cli.Validation("credential file missing MATRIX_REGISTRATION_TOKEN")
			}

			registrationTokenBuffer, err := secret.NewFromString(registrationToken)
			if err != nil {
				return cli.Internal("protecting registration token: %w", err)
			}
			defer registrationTokenBuffer.Close()

			// Account registration (client.Register) is unauthenticated
			// and needs a Client, not a Session. Create a separate
			// Client for this single HTTP POST.
			homeserverURL, err := params.SessionConfig.ResolveHomeserverURL()
			if err != nil {
				return err
			}

			client, err := messaging.NewClient(messaging.ClientConfig{
				HomeserverURL: homeserverURL,
			})
			if err != nil {
				return cli.Internal("create matrix client: %w", err)
			}

			server, err := ref.ServerFromUserID(adminSession.UserID().String())
			if err != nil {
				return cli.Internal("cannot determine server name from session: %w", err)
			}
			fleet, err := ref.ParseFleet(args[0], server)
			if err != nil {
				return cli.Validation("%v", err)
			}
			machine, err := ref.NewMachine(fleet, args[1])
			if err != nil {
				return cli.Validation("invalid machine name: %v", err)
			}

			logger = logger.With(
				"fleet", fleet.Localpart(),
				"machine", machine.Localpart(),
			)

			result, err := Provision(ctx, client, adminSession, ProvisionParams{
				Machine:           machine,
				HomeserverURL:     homeserverURL,
				RegistrationToken: registrationTokenBuffer,
			}, logger)
			if err != nil {
				return err
			}

			if params.OutputPath != "" {
				if err := bootstrap.WriteConfig(params.OutputPath, result.Config); err != nil {
					return cli.Internal("write bootstrap config: %w", err)
				}
				logger.Info("bootstrap config written", "path", params.OutputPath)
			} else {
				if err := bootstrap.WriteToStdout(result.Config); err != nil {
					return cli.Internal("write bootstrap config to stdout: %w", err)
				}
			}

			logger.Info("transfer the bootstrap config to the new machine and run: bureau-launcher --bootstrap-file <config> --first-boot-only")
			logger.Info("the one-time password will be rotated on first boot")

			return nil
		},
	}
}

// Provision creates a machine account, sets up its Bureau rooms, and returns
// a bootstrap config. The caller provides:
//   - A *messaging.Client for account registration
//   - A *messaging.DirectSession for admin operations (room management,
//     password reset during re-provisioning)
//   - A ProvisionParams with the fleet localpart, machine name, and
//     registration token
//
// This function does not read credential files or create sessions. The CLI
// wrapper handles credential acquisition; integration tests and other
// programmatic callers provide their own client and session.
func Provision(ctx context.Context, client *messaging.Client, adminSession *messaging.DirectSession, params ProvisionParams, logger *slog.Logger) (*ProvisionResult, error) {
	machine := params.Machine
	fleet := machine.Fleet()
	server := fleet.Server()
	adminUserID := adminSession.UserID()
	machineUsername := machine.Localpart()
	machineUserID := machine.UserID()

	// Generate a random one-time password (32 bytes = 64 hex chars).
	// The raw bytes are zeroed after hex encoding, and the hex bytes
	// are moved into an mmap-backed buffer.
	passwordBytes := make([]byte, 32)
	if _, err := rand.Read(passwordBytes); err != nil {
		return nil, cli.Internal("generate random password: %w", err)
	}
	hexBytes := []byte(hex.EncodeToString(passwordBytes))
	secret.Zero(passwordBytes)
	oneTimePassword, err := secret.NewFromBytes(hexBytes)
	if err != nil {
		return nil, cli.Internal("protecting one-time password: %w", err)
	}
	defer oneTimePassword.Close()

	// Register the machine account. Use the one-time password (NOT a
	// derived password from the registration token). The launcher will
	// rotate this password on first boot.
	logger.Info("registering machine account", "machine_user_id", machineUserID.String())

	_, registerError := client.Register(ctx, messaging.RegisterRequest{
		Username:          machineUsername,
		Password:          oneTimePassword,
		RegistrationToken: params.RegistrationToken,
	})
	if registerError != nil {
		if messaging.IsMatrixError(registerError, messaging.ErrCodeUserInUse) {
			// Account already exists. This is safe to proceed only if the
			// machine has been fully decommissioned: zero Bureau room
			// memberships and cleared state events. Otherwise it could be
			// an active machine or a partially decommissioned one.
			logger.Info("account already exists, verifying decommission status")
			if err := verifyFullDecommission(ctx, adminSession, machine, machineUsername, machineUserID, logger); err != nil {
				return nil, err
			}
			// Decommission verified. Reset the password to our one-time
			// password so the bootstrap file will work.
			resetErr := adminSession.ResetUserPassword(ctx, machineUserID, oneTimePassword.String(), true)
			if resetErr != nil {
				if messaging.IsMatrixError(resetErr, messaging.ErrCodeUnrecognized) {
					return nil, cli.Internal("this homeserver does not support admin password reset (Synapse admin API) — "+
						"the decommissioned machine %q cannot be reused; choose a new machine name", machineUsername)
				}
				return nil, cli.Internal("reset password for re-provisioned machine: %w", resetErr)
			}
			logger.Info("password reset for re-provisioning")
		} else {
			return nil, cli.Internal("register machine account: %w", registerError)
		}
	} else {
		logger.Info("account created", "machine_user_id", machineUserID.String())
	}

	// Resolve all global Bureau rooms and invite the machine to each.
	namespace := fleet.Namespace()
	globalRooms, _, resolveErrors := resolveGlobalRooms(ctx, adminSession, namespace)
	if len(resolveErrors) > 0 {
		// All global rooms must be resolvable for provisioning. Unlike
		// decommission (where best-effort is acceptable), a provision that
		// skips rooms would produce a machine that can't fully participate.
		return nil, cli.NotFound("cannot resolve all Bureau rooms: %v", resolveErrors[0])
	}

	for _, room := range globalRooms {
		if err := adminSession.InviteUser(ctx, room.roomID, machineUserID); err != nil {
			if !messaging.IsMatrixError(err, messaging.ErrCodeForbidden) {
				return nil, cli.Internal("invite machine to %s: %w", room.alias, err)
			}
			logger.Info("already invited to room", "room", room.alias)
		} else {
			logger.Info("invited to room", "room", room.alias)
		}
	}

	// Resolve and invite to fleet-scoped rooms.
	machineRoomID, serviceRoomID, fleetRoomID, err := resolveFleetRooms(ctx, adminSession, fleet)
	if err != nil {
		return nil, cli.NotFound("resolve fleet rooms: %w", err)
	}
	for _, fleetRoom := range []struct {
		id   ref.RoomID
		name string
	}{
		{machineRoomID, "fleet machine room"},
		{serviceRoomID, "fleet service room"},
		{fleetRoomID, "fleet config room"},
	} {
		if err := adminSession.InviteUser(ctx, fleetRoom.id, machineUserID); err != nil {
			if !messaging.IsMatrixError(err, messaging.ErrCodeForbidden) {
				return nil, cli.Internal("invite machine to %s: %w", fleetRoom.name, err)
			}
			logger.Info("already invited to room", "room", fleetRoom.name)
		} else {
			logger.Info("invited to room", "room", fleetRoom.name)
		}
	}

	// Create the per-machine config room. The admin creates it (not the
	// machine) so the admin has PL 100 from the start. The machine account
	// is invited. The config room alias uses the @→# convention: the
	// fleet-scoped localpart IS the room alias localpart.
	configAlias := machine.RoomAlias()

	logger.Info("creating config room", "alias", configAlias)
	configRoom, createError := adminSession.CreateRoom(ctx, messaging.CreateRoomRequest{
		Name:       "Config: " + machineUsername,
		Topic:      "Machine configuration and credentials for " + machineUsername,
		Alias:      machine.Localpart(),
		Preset:     "private_chat",
		Invite:     []string{machineUserID.String()},
		Visibility: "private",
	})
	if createError != nil {
		if messaging.IsMatrixError(createError, messaging.ErrCodeRoomInUse) {
			// Config room already exists from a previous provision. Resolve
			// it and re-invite the machine (it was kicked during decommission).
			logger.Info("config room already exists, re-inviting machine")
			configRoomID, err := adminSession.ResolveAlias(ctx, configAlias)
			if err != nil {
				return nil, cli.Internal("resolve existing config room %q: %w", configAlias, err)
			}
			if err := adminSession.InviteUser(ctx, configRoomID, machineUserID); err != nil {
				if !messaging.IsMatrixError(err, messaging.ErrCodeForbidden) {
					return nil, cli.Internal("re-invite machine to config room: %w", err)
				}
				logger.Info("already invited to config room")
			} else {
				logger.Info("re-invited to config room")
			}
		} else {
			return nil, cli.Internal("create config room: %w", createError)
		}
	} else {
		// Set power levels: admin=100, machine=50. Machine can write
		// MachineConfig, layouts, and invites but cannot modify
		// credentials, power levels, or room metadata.
		_, err = adminSession.SendStateEvent(ctx, configRoom.RoomID, "m.room.power_levels", "",
			schema.ConfigRoomPowerLevels(adminUserID, machineUserID))
		if err != nil {
			return nil, cli.Internal("set config room power levels: %w", err)
		}
		logger.Info("config room created", "room_id", configRoom.RoomID.String())
	}

	// Build the bootstrap config. The MachineName and FleetPrefix fields
	// use the fleet-scoped localpart format consumed by the launcher.
	config := &bootstrap.Config{
		HomeserverURL: params.HomeserverURL,
		ServerName:    server.String(),
		MachineName:   machine.Localpart(),
		Password:      oneTimePassword.String(),
		FleetPrefix:   fleet.Localpart(),
	}

	return &ProvisionResult{Config: config}, nil
}

// verifyFullDecommission checks that a machine account has been fully
// decommissioned: zero active memberships in all Bureau rooms, and cleared
// machine_key and machine_status state events.
//
// This is the security gate for re-provisioning. Without it, provision
// could be used to take over an active machine's identity by resetting
// its password and re-bootstrapping.
func verifyFullDecommission(ctx context.Context, session messaging.Session, machine ref.Machine, machineUsername string, machineUserID ref.UserID, logger *slog.Logger) error {
	// Resolve all global Bureau rooms.
	namespace := machine.Fleet().Namespace()
	globalRooms, failedRooms, _ := resolveGlobalRooms(ctx, session, namespace)
	if len(failedRooms) > 0 {
		// If we can't resolve all rooms, we can't verify full decommission.
		// Fail-safe: refuse re-provisioning.
		var failedNames []string
		for _, room := range failedRooms {
			failedNames = append(failedNames, room.displayName)
		}
		return cli.Internal("cannot verify decommission: unable to resolve rooms [%s] — cannot confirm machine has zero Bureau memberships",
			strings.Join(failedNames, ", "))
	}

	// Check for active memberships in all global rooms.
	activeRooms := checkMachineMembership(ctx, session, machineUserID, globalRooms)

	// Also check the config room (@→# convention).
	configAlias := machine.RoomAlias()
	configRoomID, err := session.ResolveAlias(ctx, configAlias)
	if err == nil {
		configResolved := resolvedRoom{
			machineRoom: machineRoom{displayName: "config room"},
			alias:       configAlias,
			roomID:      configRoomID,
		}
		configActive := checkMachineMembership(ctx, session, machineUserID, []resolvedRoom{configResolved})
		activeRooms = append(activeRooms, configActive...)
	}
	// Config room not existing is fine — it means decommission cleaned it up or
	// it was never created (half-baked provision).

	if len(activeRooms) > 0 {
		roomDescriptions := make([]string, len(activeRooms))
		for index, room := range activeRooms {
			roomDescriptions[index] = fmt.Sprintf("%s (%s)", room.displayName, room.roomID)
		}
		logger.Warn("machine has active Bureau room memberships",
			"count", len(activeRooms),
			"rooms", roomDescriptions,
		)
		return cli.Conflict("machine account %s exists and is not fully decommissioned — run 'bureau machine decommission %s %s' first",
			machineUserID, machine.Fleet().Localpart(), machine.Name())
	}

	// Check that machine_key and machine_status are cleared in the fleet
	// machine room.
	machineRoomID, _, _, err := resolveFleetRooms(ctx, session, machine.Fleet())
	if err != nil {
		return cli.Internal("resolve fleet rooms for decommission check: %w", err)
	}

	events, err := session.GetRoomState(ctx, machineRoomID)
	if err != nil {
		return cli.Internal("cannot read machine room state to verify decommission: %w", err)
	}

	for _, event := range events {
		if event.StateKey == nil || *event.StateKey != machineUsername {
			continue
		}
		if event.Type != schema.EventTypeMachineKey && event.Type != schema.EventTypeMachineStatus {
			continue
		}
		// Check if content is non-empty (not cleared).
		contentBytes, err := json.Marshal(event.Content)
		if err != nil {
			continue
		}
		content := string(contentBytes)
		if content != "{}" && content != "null" && content != "" {
			return cli.Conflict("machine account %s has active %s state event — run 'bureau machine decommission %s %s' first",
				machineUserID, event.Type, machine.Fleet().Localpart(), machine.Name())
		}
	}

	logger.Info("decommission verified, zero memberships and cleared state")
	return nil
}
