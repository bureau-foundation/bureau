// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/secret"
	"github.com/bureau-foundation/bureau/messaging"
)

// enableParams holds the parameters for the fleet enable command.
type enableParams struct {
	cli.SessionConfig
	cli.JSONOutput
	Name       string `json:"name"        flag:"name"        desc:"fleet controller name suffix (e.g., prod) — becomes service/fleet/<name>"`
	Host       string `json:"host"        flag:"host"        desc:"machine localpart to run the fleet controller on (e.g., machine/workstation; use 'local' to auto-detect)"`
	ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name" default:"bureau.local"`
}

// enableResult is the JSON output of the enable command.
type enableResult struct {
	ServicePrincipal string `json:"service_principal" desc:"fleet controller principal localpart"`
	ServiceUserID    string `json:"service_user_id"   desc:"fleet controller Matrix user ID"`
	Machine          string `json:"machine"           desc:"machine hosting the fleet controller"`
}

func enableCommand() *cli.Command {
	var params enableParams

	return &cli.Command{
		Name:    "enable",
		Summary: "Bootstrap a fleet controller on a machine",
		Description: `Bootstrap the fleet controller for a Bureau deployment.

This operator command creates the fleet controller's service account,
publishes a PrincipalAssignment to the target machine's config room,
and waits for the daemon to register the service.

The command:
  - Registers a Matrix account for the fleet controller
  - Publishes a PrincipalAssignment to the machine's config room
  - The daemon starts the fleet controller and registers it in
    #bureau/service

Example:

  bureau fleet enable --name prod --host machine/workstation --credential-file ./creds

This creates service principal "service/fleet/prod", adds it to the
workstation's MachineConfig, and the daemon starts the fleet controller.`,
		Usage: "bureau fleet enable --name <name> --host <machine> [flags]",
		Examples: []cli.Example{
			{
				Description: "Enable the fleet controller on a workstation",
				Command:     "bureau fleet enable --name prod --host machine/workstation --credential-file ./creds",
			},
			{
				Description: "Enable on the local machine (auto-detect)",
				Command:     "bureau fleet enable --name prod --host local --credential-file ./creds",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &enableResult{} },
		Annotations:    cli.Idempotent(),
		RequiredGrants: []string{"command/fleet/enable"},
		Run: func(args []string) error {
			if len(args) > 0 {
				return cli.Validation("unexpected argument: %s", args[0])
			}
			if params.Name == "" {
				return cli.Validation("--name is required")
			}
			if params.Host == "" {
				return cli.Validation("--host is required")
			}
			return runEnable(&params)
		},
	}
}

func runEnable(params *enableParams) error {
	// Resolve "local" to the actual machine localpart.
	host := params.Host
	if host == "local" {
		resolved, err := cli.ResolveLocalMachine()
		if err != nil {
			return cli.Internal("resolving local machine identity: %w", err)
		}
		host = resolved
		fmt.Fprintf(os.Stderr, "Resolved --host=local to %s\n", host)
	}

	if err := principal.ValidateLocalpart(host); err != nil {
		return cli.Validation("invalid host: %w", err)
	}

	// Derive the service principal localpart from the name.
	servicePrincipal := "service/fleet/" + params.Name
	if err := principal.ValidateLocalpart(servicePrincipal); err != nil {
		return cli.Validation("invalid service principal %q: %w", servicePrincipal, err)
	}

	serviceUserID := principal.MatrixUserID(servicePrincipal, params.ServerName)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Read credentials for registration token and admin session.
	if params.SessionConfig.CredentialFile == "" {
		return cli.Validation("--credential-file is required for service account registration")
	}
	credentials, err := cli.ReadCredentialFile(params.SessionConfig.CredentialFile)
	if err != nil {
		return cli.Internal("reading credentials: %w", err)
	}

	// Connect as admin for state event publishing.
	adminSession, err := params.SessionConfig.Connect(ctx)
	if err != nil {
		return cli.Internal("connecting admin session: %w", err)
	}
	defer adminSession.Close()

	// Step 1: Register the fleet controller Matrix account (idempotent).
	if err := registerFleetServiceAccount(ctx, credentials, servicePrincipal, params.ServerName); err != nil {
		return cli.Internal("registering service account: %w", err)
	}

	// Step 2: Publish PrincipalAssignment to the machine config room.
	if err := publishFleetAssignment(ctx, adminSession, host, servicePrincipal, params.Name, params.ServerName); err != nil {
		return cli.Internal("publishing principal assignment: %w", err)
	}

	// Step 3: Publish fleet room_service binding in all machine config rooms.
	// This makes the fleet controller discoverable via RequiredServices for
	// any agent on any machine in the deployment.
	fleetPrefix := "bureau/fleet/" + params.Name
	bindingCount, err := publishFleetBindings(ctx, adminSession, serviceUserID, fleetPrefix, params.ServerName)
	if err != nil {
		return cli.Internal("publishing fleet bindings: %w", err)
	}

	if done, err := params.EmitJSON(enableResult{
		ServicePrincipal: servicePrincipal,
		ServiceUserID:    serviceUserID,
		Machine:          host,
	}); done {
		return err
	}

	fmt.Fprintf(os.Stderr, "\nFleet controller enabled:\n")
	fmt.Fprintf(os.Stderr, "  Service:  %s (%s)\n", servicePrincipal, serviceUserID)
	fmt.Fprintf(os.Stderr, "  Machine:  %s\n", host)
	fmt.Fprintf(os.Stderr, "  Bindings: %d config room(s)\n", bindingCount)
	fmt.Fprintf(os.Stderr, "\nThe daemon will start the fleet controller and register it in\n")
	fmt.Fprintf(os.Stderr, "the service room. The fleet controller will begin tracking machines\n")
	fmt.Fprintf(os.Stderr, "and processing FleetServiceContent events from the fleet room.\n")

	return nil
}

// registerFleetServiceAccount registers a Matrix account for the fleet
// controller. Idempotent: M_USER_IN_USE is silently ignored.
func registerFleetServiceAccount(ctx context.Context, credentials map[string]string, servicePrincipal, serverName string) error {
	homeserverURL := credentials["MATRIX_HOMESERVER_URL"]
	if homeserverURL == "" {
		homeserverURL = "http://localhost:6167"
	}

	registrationToken := credentials["MATRIX_REGISTRATION_TOKEN"]
	if registrationToken == "" {
		return cli.Validation("credential file missing MATRIX_REGISTRATION_TOKEN")
	}

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: homeserverURL,
	})
	if err != nil {
		return cli.Internal("creating matrix client: %w", err)
	}

	// Derive password from registration token (same deterministic
	// derivation as bureau matrix user create for agent accounts).
	tokenBuffer, err := secret.NewFromString(registrationToken)
	if err != nil {
		return cli.Internal("protecting registration token: %w", err)
	}
	defer tokenBuffer.Close()

	passwordBuffer, err := cli.DeriveAdminPassword(tokenBuffer)
	if err != nil {
		return cli.Internal("deriving password: %w", err)
	}
	defer passwordBuffer.Close()

	registrationTokenBuffer, err := secret.NewFromString(registrationToken)
	if err != nil {
		return cli.Internal("protecting registration token: %w", err)
	}
	defer registrationTokenBuffer.Close()

	session, registerErr := client.Register(ctx, messaging.RegisterRequest{
		Username:          servicePrincipal,
		Password:          passwordBuffer,
		RegistrationToken: registrationTokenBuffer,
	})
	if registerErr != nil {
		if messaging.IsMatrixError(registerErr, messaging.ErrCodeUserInUse) {
			fmt.Fprintf(os.Stderr, "Service account %s already exists.\n", principal.MatrixUserID(servicePrincipal, serverName))
			return nil
		}
		return cli.Internal("registering %s: %w", servicePrincipal, registerErr)
	}
	defer session.Close()

	fmt.Fprintf(os.Stderr, "Registered service account %s\n", session.UserID())
	return nil
}

// publishFleetBindings publishes an m.bureau.room_service state event with
// state key "fleet" in every active machine's config room. This binds the
// "fleet" service role to the fleet controller so agents with
// required_services: ["fleet"] can discover the fleet controller socket.
//
// Active machines are discovered from m.bureau.machine_key state events in
// the fleet's machine room (resolved from the fleet prefix). Machines with
// empty key content (decommissioned) are skipped. Returns the number of
// config rooms successfully updated.
func publishFleetBindings(ctx context.Context, session messaging.Session, serviceUserID, fleetPrefix, serverName string) (int, error) {
	namespace, fleetName, parseErr := principal.ParseFleetPrefix(fleetPrefix)
	if parseErr != nil {
		return 0, cli.Validation("invalid fleet prefix %q: %w", fleetPrefix, parseErr)
	}
	machineAliasLocalpart := schema.FleetMachineRoomAlias(namespace, fleetName)
	machineAlias := schema.FullRoomAlias(machineAliasLocalpart, serverName)
	machineRoomID, err := session.ResolveAlias(ctx, machineAlias)
	if err != nil {
		return 0, cli.NotFound("resolving fleet machine room %s: %w", machineAlias, err)
	}

	events, err := session.GetRoomState(ctx, machineRoomID)
	if err != nil {
		return 0, cli.Internal("reading machine room state: %w", err)
	}

	binding := schema.RoomServiceContent{Principal: serviceUserID}
	count := 0

	for _, event := range events {
		if event.Type != schema.EventTypeMachineKey || event.StateKey == nil || *event.StateKey == "" {
			continue
		}
		// Skip decommissioned machines (empty key field).
		if keyValue, _ := event.Content["key"].(string); keyValue == "" {
			continue
		}

		machineLocalpart := *event.StateKey
		configAlias := schema.FullRoomAlias(schema.ConfigRoomAlias(machineLocalpart), serverName)
		configRoomID, err := session.ResolveAlias(ctx, configAlias)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  WARNING: cannot resolve config room for %s: %v\n", machineLocalpart, err)
			continue
		}

		_, err = session.SendStateEvent(ctx, configRoomID, schema.EventTypeRoomService, "fleet", binding)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  WARNING: cannot publish fleet binding in %s: %v\n", machineLocalpart, err)
			continue
		}

		fmt.Fprintf(os.Stderr, "  Published fleet binding in config room for %s\n", machineLocalpart)
		count++
	}

	return count, nil
}

// publishFleetAssignment adds the fleet controller to the machine's
// MachineConfig via read-modify-write. AutoStart is false because the
// fleet controller binary is externally managed.
func publishFleetAssignment(ctx context.Context, session messaging.Session, host, servicePrincipal, name, serverName string) error {
	configRoomAlias := principal.RoomAlias("bureau/config/"+host, serverName)
	configRoomID, err := session.ResolveAlias(ctx, configRoomAlias)
	if err != nil {
		return cli.NotFound("resolving config room %s: %w (has the machine been registered?)", configRoomAlias, err)
	}

	// Read existing MachineConfig.
	var config schema.MachineConfig
	existingContent, err := session.GetStateEvent(ctx, configRoomID, schema.EventTypeMachineConfig, host)
	if err == nil {
		if unmarshalErr := json.Unmarshal(existingContent, &config); unmarshalErr != nil {
			return cli.Internal("parsing existing machine config: %w", unmarshalErr)
		}
	} else if !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
		return cli.Internal("reading machine config: %w", err)
	}

	// Check if the principal already exists.
	for _, existing := range config.Principals {
		if existing.Localpart == servicePrincipal {
			fmt.Fprintf(os.Stderr, "Principal %s already in MachineConfig, skipping.\n", servicePrincipal)
			return nil
		}
	}

	config.Principals = append(config.Principals, schema.PrincipalAssignment{
		Localpart: servicePrincipal,
		AutoStart: false,
		Labels: map[string]string{
			"role":    "service",
			"service": "fleet",
			"name":    name,
		},
	})

	_, err = session.SendStateEvent(ctx, configRoomID, schema.EventTypeMachineConfig, host, config)
	if err != nil {
		return cli.Internal("publishing machine config: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Published PrincipalAssignment for %s to %s\n", servicePrincipal, host)
	return nil
}
