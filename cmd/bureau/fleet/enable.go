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
				return fmt.Errorf("unexpected argument: %s", args[0])
			}
			if params.Name == "" {
				return fmt.Errorf("--name is required")
			}
			if params.Host == "" {
				return fmt.Errorf("--host is required")
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
			return fmt.Errorf("resolving local machine identity: %w", err)
		}
		host = resolved
		fmt.Fprintf(os.Stderr, "Resolved --host=local to %s\n", host)
	}

	if err := principal.ValidateLocalpart(host); err != nil {
		return fmt.Errorf("invalid host: %w", err)
	}

	// Derive the service principal localpart from the name.
	servicePrincipal := "service/fleet/" + params.Name
	if err := principal.ValidateLocalpart(servicePrincipal); err != nil {
		return fmt.Errorf("invalid service principal %q: %w", servicePrincipal, err)
	}

	serviceUserID := principal.MatrixUserID(servicePrincipal, params.ServerName)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Read credentials for registration token and admin session.
	if params.SessionConfig.CredentialFile == "" {
		return fmt.Errorf("--credential-file is required for service account registration")
	}
	credentials, err := cli.ReadCredentialFile(params.SessionConfig.CredentialFile)
	if err != nil {
		return fmt.Errorf("reading credentials: %w", err)
	}

	// Connect as admin for state event publishing.
	adminSession, err := params.SessionConfig.Connect(ctx)
	if err != nil {
		return fmt.Errorf("connecting admin session: %w", err)
	}
	defer adminSession.Close()

	// Step 1: Register the fleet controller Matrix account (idempotent).
	if err := registerFleetServiceAccount(ctx, credentials, servicePrincipal, params.ServerName); err != nil {
		return fmt.Errorf("registering service account: %w", err)
	}

	// Step 2: Publish PrincipalAssignment to the machine config room.
	if err := publishFleetAssignment(ctx, adminSession, host, servicePrincipal, params.Name, params.ServerName); err != nil {
		return fmt.Errorf("publishing principal assignment: %w", err)
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
		return fmt.Errorf("credential file missing MATRIX_REGISTRATION_TOKEN")
	}

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: homeserverURL,
	})
	if err != nil {
		return fmt.Errorf("creating matrix client: %w", err)
	}

	// Derive password from registration token (same deterministic
	// derivation as bureau matrix user create for agent accounts).
	tokenBuffer, err := secret.NewFromString(registrationToken)
	if err != nil {
		return fmt.Errorf("protecting registration token: %w", err)
	}
	defer tokenBuffer.Close()

	passwordBuffer, err := cli.DeriveAdminPassword(tokenBuffer)
	if err != nil {
		return fmt.Errorf("deriving password: %w", err)
	}
	defer passwordBuffer.Close()

	registrationTokenBuffer, err := secret.NewFromString(registrationToken)
	if err != nil {
		return fmt.Errorf("protecting registration token: %w", err)
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
		return fmt.Errorf("registering %s: %w", servicePrincipal, registerErr)
	}
	defer session.Close()

	fmt.Fprintf(os.Stderr, "Registered service account %s\n", session.UserID())
	return nil
}

// publishFleetAssignment adds the fleet controller to the machine's
// MachineConfig via read-modify-write. AutoStart is false because the
// fleet controller binary is externally managed.
func publishFleetAssignment(ctx context.Context, session *messaging.Session, host, servicePrincipal, name, serverName string) error {
	configRoomAlias := principal.RoomAlias("bureau/config/"+host, serverName)
	configRoomID, err := session.ResolveAlias(ctx, configRoomAlias)
	if err != nil {
		return fmt.Errorf("resolving config room %s: %w (has the machine been registered?)", configRoomAlias, err)
	}

	// Read existing MachineConfig.
	var config schema.MachineConfig
	existingContent, err := session.GetStateEvent(ctx, configRoomID, schema.EventTypeMachineConfig, host)
	if err == nil {
		if unmarshalErr := json.Unmarshal(existingContent, &config); unmarshalErr != nil {
			return fmt.Errorf("parsing existing machine config: %w", unmarshalErr)
		}
	} else if !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
		return fmt.Errorf("reading machine config: %w", err)
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
		return fmt.Errorf("publishing machine config: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Published PrincipalAssignment for %s to %s\n", servicePrincipal, host)
	return nil
}
