// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/credential"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/secret"
	"github.com/bureau-foundation/bureau/lib/templatedef"
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
	Service       ref.Service `json:"service"         desc:"fleet controller service reference"`
	Machine       ref.Machine `json:"machine"         desc:"machine hosting the fleet controller"`
	ConfigRoomID  ref.RoomID  `json:"config_room_id"  desc:"config room where the assignment was published"`
	ConfigEventID ref.EventID `json:"config_event_id" desc:"event ID of the MachineConfig state event"`
	BindingCount  int         `json:"binding_count"   desc:"number of machine config rooms updated with fleet bindings"`
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
  - Validates the fleet-controller template exists in Matrix
  - Registers a Matrix account for the fleet controller service
  - Provisions encrypted credentials to the target machine
  - Publishes a PrincipalAssignment to the target machine's config room
  - Publishes fleet service bindings to all machine config rooms in the
    fleet, making the controller discoverable via RequiredServices

The fleet controller uses the "fleet-controller" template (published
by "bureau matrix setup"). The daemon manages the fleet controller's
sandbox lifecycle — starting it automatically when the assignment is
published and restarting it on crash.

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
		RequiredGrants: []string{"command/fleet/enable"},
		Annotations:    cli.Create(),
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) == 0 {
				return cli.Validation("fleet localpart is required (e.g., bureau/fleet/prod)")
			}
			if len(args) > 1 {
				return cli.Validation("expected exactly one argument (fleet localpart), got %d", len(args))
			}
			if params.Host == "" {
				return cli.Validation("--host is required (machine name within the fleet, e.g., workstation)")
			}
			return runEnable(ctx, logger, args[0], &params)
		},
	}
}

func runEnable(ctx context.Context, logger *slog.Logger, fleetLocalpart string, params *enableParams) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// Read credentials for registration token and admin session.
	if params.SessionConfig.CredentialFile == "" {
		return cli.Validation("--credential-file is required for service account registration")
	}
	credentials, err := cli.ReadCredentialFile(params.SessionConfig.CredentialFile)
	if err != nil {
		return cli.Internal("reading credentials: %w", err)
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

	// Connect as admin for state event publishing and credential
	// provisioning.
	adminSession, err := params.SessionConfig.Connect(ctx)
	if err != nil {
		return cli.Internal("connecting admin session: %w", err)
	}
	defer adminSession.Close()

	// Account registration (client.Register) is unauthenticated and
	// needs a Client, not a Session.
	homeserverURL, err := params.SessionConfig.ResolveHomeserverURL()
	if err != nil {
		return err
	}

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: homeserverURL,
	})
	if err != nil {
		return cli.Internal("creating matrix client: %w", err)
	}

	// Derive server name from the connected session's identity.
	server, err := ref.ServerFromUserID(adminSession.UserID().String())
	if err != nil {
		return cli.Internal("cannot determine server name from session: %w", err)
	}

	// Parse and validate the fleet localpart.
	fleet, err := ref.ParseFleet(fleetLocalpart, server)
	if err != nil {
		return cli.Validation("%v", err)
	}

	// Resolve the machine within the fleet.
	machine, err := resolveHostMachine(fleet, params.Host, logger)
	if err != nil {
		return err
	}
	logger = logger.With("machine", machine.Localpart())

	// The fleet controller service is always named "fleet" within its
	// fleet.
	serviceRef, err := ref.NewService(fleet, "fleet")
	if err != nil {
		return cli.Internal("constructing fleet controller service ref: %w", err)
	}

	// Resolve the fleet machine room for credential provisioning.
	machineRoomAlias := fleet.MachineRoomAlias()
	machineRoomID, err := adminSession.ResolveAlias(ctx, machineRoomAlias)
	if err != nil {
		return cli.Internal("resolve fleet machine room %q: %w", machineRoomAlias, err)
	}

	// The fleet-controller template is published by "bureau matrix
	// setup". It inherits from service-base which provides the proxy
	// bootstrap environment variables.
	templateRef := schema.TemplateRef{
		Room:     "bureau/template",
		Template: "fleet-controller",
	}

	// Deploy the fleet controller through the standard principal
	// deployment path: register account, provision encrypted
	// credentials, invite to config room, publish MachineConfig
	// assignment. The daemon detects the assignment via /sync and
	// creates the fleet controller's sandbox with AutoStart:true.
	createResult, err := principal.Create(ctx, client, adminSession, registrationTokenBuffer, credential.AsProvisionFunc(), principal.CreateParams{
		Machine:     machine,
		Principal:   serviceRef.Entity(),
		TemplateRef: templateRef,
		ValidateTemplate: func(ctx context.Context, templateReference schema.TemplateRef, serverName ref.ServerName) error {
			_, err := templatedef.Fetch(ctx, adminSession, templateReference, serverName)
			return err
		},
		HomeserverURL: homeserverURL,
		AutoStart:     true,
		MachineRoomID: machineRoomID,
		Labels: map[string]string{
			"role":    "service",
			"service": "fleet",
		},
	})
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeUserInUse) {
			// The fleet controller account already exists. This is
			// expected when re-running fleet enable to update
			// bindings after adding machines to the fleet.
			logger.Info("fleet controller already deployed", "user_id", serviceRef.UserID().String())
		} else {
			return cli.Internal("deploying fleet controller: %w", err)
		}
	} else {
		logger.Info("fleet controller deployed",
			"user_id", createResult.PrincipalUserID,
			"config_room", createResult.ConfigRoomID,
			"config_event", createResult.ConfigEventID,
		)
	}

	// Publish fleet service bindings to all machine config rooms. This
	// binds the "fleet" service role to the fleet controller so agents
	// with required_services: ["fleet"] can discover it.
	bindingCount, err := publishFleetBindings(ctx, adminSession, fleet, serviceRef, logger)
	if err != nil {
		return cli.Internal("publishing fleet bindings: %w", err)
	}

	result := enableResult{
		Service:      serviceRef,
		Machine:      machine,
		BindingCount: bindingCount,
	}
	if createResult != nil {
		result.ConfigRoomID = createResult.ConfigRoomID
		result.ConfigEventID = createResult.ConfigEventID
	}

	if done, emitErr := params.EmitJSON(result); done {
		return emitErr
	}

	logger.Info("fleet controller enabled",
		"service", serviceRef.Localpart(),
		"service_user_id", serviceRef.UserID(),
		"machine_user_id", machine.UserID(),
		"template", templateRef,
		"bindings", bindingCount,
	)

	return nil
}

// resolveHostMachine constructs a Machine ref from the fleet and host flag.
// The host is either a bare machine name (e.g., "workstation") or the
// literal "local" which auto-detects from the launcher's session file.
func resolveHostMachine(fleet ref.Fleet, host string, logger *slog.Logger) (ref.Machine, error) {
	machineName := host
	if host == "local" {
		resolved, err := resolveLocalMachineName()
		if err != nil {
			return ref.Machine{}, cli.Internal("resolving local machine identity: %w", err)
		}
		machineName = resolved
		logger.Info("resolved --host=local", "machine_name", machineName)
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

// publishFleetBindings publishes an m.bureau.room_service state event with
// state key "fleet" in every active machine's config room. This binds the
// "fleet" service role to the fleet controller so agents with
// required_services: ["fleet"] can discover the fleet controller socket.
//
// Active machines are discovered from m.bureau.machine_key state events in
// the fleet's machine room. Machines with empty key content (decommissioned)
// are skipped. Returns the number of config rooms successfully updated.
func publishFleetBindings(ctx context.Context, session messaging.Session, fleet ref.Fleet, serviceRef ref.Service, logger *slog.Logger) (int, error) {
	machineRoomID, err := session.ResolveAlias(ctx, fleet.MachineRoomAlias())
	if err != nil {
		return 0, cli.NotFound("resolving fleet machine room %s: %w", fleet.MachineRoomAlias(), err)
	}

	events, err := session.GetRoomState(ctx, machineRoomID)
	if err != nil {
		return 0, cli.Internal("reading machine room state: %w", err)
	}

	binding := schema.RoomServiceContent{Principal: serviceRef.Entity()}
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
			logger.Warn("cannot parse machine state key", "state_key", *event.StateKey, "error", extractErr)
			continue
		}
		machine, refErr := ref.NewMachine(fleet, machineName)
		if refErr != nil {
			logger.Warn("invalid machine ref", "state_key", *event.StateKey, "error", refErr)
			continue
		}

		configRoomID, resolveErr := session.ResolveAlias(ctx, machine.RoomAlias())
		if resolveErr != nil {
			logger.Warn("cannot resolve config room", "machine", machine.Localpart(), "error", resolveErr)
			continue
		}

		_, sendErr := session.SendStateEvent(ctx, configRoomID, schema.EventTypeRoomService, "fleet", binding)
		if sendErr != nil {
			logger.Warn("cannot publish fleet binding", "machine", machine.Localpart(), "error", sendErr)
			continue
		}

		logger.Info("published fleet binding", "machine", machine.Localpart())
		count++
	}

	return count, nil
}
