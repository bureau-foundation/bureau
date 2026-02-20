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
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/secret"
	"github.com/bureau-foundation/bureau/messaging"
)

// enableParams holds the parameters for the fleet enable command.
type enableParams struct {
	cli.SessionConfig
	cli.JSONOutput
	Host string `json:"host" flag:"host" desc:"machine name within the fleet (e.g., workstation); use 'local' to auto-detect from the launcher session"`
}

// enableResult is the JSON output of the fleet enable command.
type enableResult struct {
	Service ref.Service `json:"service" desc:"fleet controller service reference"`
	Machine ref.Machine `json:"machine" desc:"machine hosting the fleet controller"`
}

func enableCommand() *cli.Command {
	var params enableParams

	return &cli.Command{
		Name:    "enable",
		Summary: "Bootstrap a fleet controller on a machine",
		Description: `Bootstrap the fleet controller for a fleet.

The argument is a fleet localpart in the form "namespace/fleet/name"
(e.g., "bureau/fleet/prod"). The server name is derived from the
connected session's identity.

This operator command:
  - Registers a Matrix account for the fleet controller service
  - Publishes a PrincipalAssignment to the target machine's config room
  - Publishes fleet service bindings to all machine config rooms in the
    fleet, making the controller discoverable via RequiredServices

The fleet controller service is always named "fleet" within its fleet:
if the fleet is bureau/fleet/prod, the service account is
@bureau/fleet/prod/service/fleet:server.

The --host flag specifies which machine in the fleet should run the
controller. This is the bare machine name (e.g., "workstation"), not
the full localpart — the fleet from the positional argument provides
the scope. Use "local" to auto-detect from the launcher's session file.`,
		Usage: "bureau fleet enable <namespace/fleet/name> --host <machine-name> [flags]",
		Examples: []cli.Example{
			{
				Description: "Enable fleet controller on workstation",
				Command:     "bureau fleet enable bureau/fleet/prod --host workstation --credential-file ./creds",
			},
			{
				Description: "Enable on the local machine (auto-detect)",
				Command:     "bureau fleet enable bureau/fleet/prod --host local --credential-file ./creds",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &enableResult{} },
		Annotations:    cli.Idempotent(),
		RequiredGrants: []string{"command/fleet/enable"},
		Run: func(args []string) error {
			if len(args) == 0 {
				return cli.Validation("fleet localpart is required (e.g., bureau/fleet/prod)")
			}
			if len(args) > 1 {
				return cli.Validation("expected exactly one argument (fleet localpart), got %d", len(args))
			}
			if params.Host == "" {
				return cli.Validation("--host is required (machine name within the fleet, e.g., workstation)")
			}
			return runEnable(args[0], &params)
		},
	}
}

func runEnable(fleetLocalpart string, params *enableParams) error {
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

	// Derive server name from the connected session's identity.
	server, err := ref.ServerFromUserID(adminSession.UserID())
	if err != nil {
		return cli.Internal("cannot determine server name from session: %w", err)
	}

	// Parse and validate the fleet localpart.
	fleet, err := ref.ParseFleet(fleetLocalpart, server)
	if err != nil {
		return cli.Validation("%v", err)
	}

	// Resolve the machine within the fleet.
	machine, err := resolveHostMachine(fleet, params.Host)
	if err != nil {
		return err
	}

	// The fleet controller service is always named "fleet" within its fleet.
	service, err := ref.NewService(fleet, "fleet")
	if err != nil {
		return cli.Internal("constructing fleet controller service ref: %w", err)
	}

	// Step 1: Register the fleet controller Matrix account (idempotent).
	if err := registerServiceAccount(ctx, credentials, service); err != nil {
		return cli.Internal("registering service account: %w", err)
	}

	// Step 2: Publish PrincipalAssignment to the machine config room.
	if err := publishFleetAssignment(ctx, adminSession, machine, service); err != nil {
		return cli.Internal("publishing principal assignment: %w", err)
	}

	// Step 3: Publish fleet service binding to all machine config rooms.
	bindingCount, err := publishFleetBindings(ctx, adminSession, fleet, service)
	if err != nil {
		return cli.Internal("publishing fleet bindings: %w", err)
	}

	result := enableResult{
		Service: service,
		Machine: machine,
	}

	if done, emitErr := params.EmitJSON(result); done {
		return emitErr
	}

	fmt.Fprintf(os.Stderr, "\nFleet controller enabled:\n")
	fmt.Fprintf(os.Stderr, "  Service:  %s (%s)\n", service.Localpart(), service.UserID())
	fmt.Fprintf(os.Stderr, "  Machine:  %s (%s)\n", machine.Localpart(), machine.UserID())
	fmt.Fprintf(os.Stderr, "  Bindings: %d config room(s)\n", bindingCount)
	fmt.Fprintf(os.Stderr, "\nThe daemon will start the fleet controller and register it in\n")
	fmt.Fprintf(os.Stderr, "the service room.\n")

	return nil
}

// resolveHostMachine constructs a Machine ref from the fleet and host flag.
// The host is either a bare machine name (e.g., "workstation") or the
// literal "local" which auto-detects from the launcher's session file.
func resolveHostMachine(fleet ref.Fleet, host string) (ref.Machine, error) {
	machineName := host
	if host == "local" {
		resolved, err := resolveLocalMachineName()
		if err != nil {
			return ref.Machine{}, cli.Internal("resolving local machine identity: %w", err)
		}
		machineName = resolved
		fmt.Fprintf(os.Stderr, "Resolved --host=local to %s\n", machineName)
	}
	machine, err := ref.NewMachine(fleet, machineName)
	if err != nil {
		return ref.Machine{}, cli.Validation("invalid host machine: %w", err)
	}
	return machine, nil
}

// resolveLocalMachineName reads the launcher's session file and extracts
// the bare machine name. The launcher's user ID may use fleet-scoped
// format (@namespace/fleet/name/machine/name:server) or legacy format
// (@machine/name:server). Both are handled: the function finds the
// "machine" entity-type segment and returns everything after it.
func resolveLocalMachineName() (string, error) {
	localpart, err := cli.ResolveLocalMachine()
	if err != nil {
		return "", err
	}
	name, extractErr := extractMachineName(localpart)
	if extractErr != nil {
		return "", extractErr
	}
	return name, nil
}

// extractMachineName extracts the bare machine name from a machine
// localpart. Handles both fleet-scoped format
// (namespace/fleet/name/machine/entityName) and legacy format
// (machine/entityName) via ref.ExtractEntityName.
func extractMachineName(localpart string) (string, error) {
	entityType, entityName, err := ref.ExtractEntityName(localpart)
	if err != nil {
		return "", err
	}
	if entityType != "machine" {
		return "", fmt.Errorf("expected machine entity, got %q in localpart %q", entityType, localpart)
	}
	return entityName, nil
}

// registerServiceAccount registers a Matrix account for a service.
// Idempotent: M_USER_IN_USE is silently ignored.
func registerServiceAccount(ctx context.Context, credentials map[string]string, service ref.Service) error {
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
		Username:          service.Localpart(),
		Password:          passwordBuffer,
		RegistrationToken: registrationTokenBuffer,
	})
	if registerErr != nil {
		if messaging.IsMatrixError(registerErr, messaging.ErrCodeUserInUse) {
			fmt.Fprintf(os.Stderr, "Service account %s already exists.\n", service.UserID())
			return nil
		}
		return cli.Internal("registering %s: %w", service.UserID(), registerErr)
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
// the fleet's machine room. Machines with empty key content (decommissioned)
// are skipped. Returns the number of config rooms successfully updated.
func publishFleetBindings(ctx context.Context, session messaging.Session, fleet ref.Fleet, service ref.Service) (int, error) {
	machineRoomID, err := session.ResolveAlias(ctx, fleet.MachineRoomAlias())
	if err != nil {
		return 0, cli.NotFound("resolving fleet machine room %s: %w", fleet.MachineRoomAlias(), err)
	}

	events, err := session.GetRoomState(ctx, machineRoomID)
	if err != nil {
		return 0, cli.Internal("reading machine room state: %w", err)
	}

	binding := schema.RoomServiceContent{Principal: service.Entity()}
	count := 0

	for _, event := range events {
		if event.Type != schema.EventTypeMachineKey || event.StateKey == nil || *event.StateKey == "" {
			continue
		}
		// Skip decommissioned machines (empty key field).
		if keyValue, _ := event.Content["key"].(string); keyValue == "" {
			continue
		}

		// The state key is a machine localpart (fleet-scoped or legacy).
		// Extract the bare machine name and construct a typed ref within
		// this fleet so we can derive the config room alias.
		machineName, extractErr := extractMachineName(*event.StateKey)
		if extractErr != nil {
			fmt.Fprintf(os.Stderr, "  WARNING: cannot parse machine state key %q: %v\n", *event.StateKey, extractErr)
			continue
		}
		machine, refErr := ref.NewMachine(fleet, machineName)
		if refErr != nil {
			fmt.Fprintf(os.Stderr, "  WARNING: invalid machine ref for %q: %v\n", *event.StateKey, refErr)
			continue
		}

		configRoomID, resolveErr := session.ResolveAlias(ctx, machine.RoomAlias())
		if resolveErr != nil {
			fmt.Fprintf(os.Stderr, "  WARNING: cannot resolve config room for %s: %v\n", machine.Localpart(), resolveErr)
			continue
		}

		_, sendErr := session.SendStateEvent(ctx, configRoomID, schema.EventTypeRoomService, "fleet", binding)
		if sendErr != nil {
			fmt.Fprintf(os.Stderr, "  WARNING: cannot publish fleet binding in %s: %v\n", machine.Localpart(), sendErr)
			continue
		}

		fmt.Fprintf(os.Stderr, "  Published fleet binding in config room for %s\n", machine.Localpart())
		count++
	}

	return count, nil
}

// publishFleetAssignment adds the fleet controller to the machine's
// MachineConfig via read-modify-write. AutoStart is false because the
// fleet controller binary is externally managed.
func publishFleetAssignment(ctx context.Context, session messaging.Session, machine ref.Machine, service ref.Service) error {
	configRoomID, err := session.ResolveAlias(ctx, machine.RoomAlias())
	if err != nil {
		return cli.NotFound("resolving config room %s: %w (has the machine been registered?)", machine.RoomAlias(), err)
	}

	// Read existing MachineConfig.
	var config schema.MachineConfig
	existingContent, err := session.GetStateEvent(ctx, configRoomID, schema.EventTypeMachineConfig, machine.Localpart())
	if err == nil {
		if unmarshalErr := json.Unmarshal(existingContent, &config); unmarshalErr != nil {
			return cli.Internal("parsing existing machine config: %w", unmarshalErr)
		}
	} else if !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
		return cli.Internal("reading machine config: %w", err)
	}

	// Check if the principal already exists.
	for _, existing := range config.Principals {
		if existing.Principal.Localpart() == service.Localpart() {
			fmt.Fprintf(os.Stderr, "Principal %s already in MachineConfig, skipping.\n", service.Localpart())
			return nil
		}
	}

	// Convert the Service ref to a generic Entity for the PrincipalAssignment.
	principalEntity, err := ref.ParseEntityLocalpart(service.Localpart(), service.Server())
	if err != nil {
		return cli.Internal("converting service ref to entity: %w", err)
	}

	config.Principals = append(config.Principals, schema.PrincipalAssignment{
		Principal: principalEntity,
		AutoStart: false,
		Labels: map[string]string{
			"role":    "service",
			"service": "fleet",
		},
	})

	_, err = session.SendStateEvent(ctx, configRoomID, schema.EventTypeMachineConfig, machine.Localpart(), config)
	if err != nil {
		return cli.Internal("publishing machine config: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Published PrincipalAssignment for %s to %s\n", service.Localpart(), machine.Localpart())
	return nil
}
