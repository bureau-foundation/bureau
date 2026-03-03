// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/credential"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	fleetschema "github.com/bureau-foundation/bureau/lib/schema/fleet"
	"github.com/bureau-foundation/bureau/lib/secret"
	"github.com/bureau-foundation/bureau/lib/templatedef"
	"github.com/bureau-foundation/bureau/messaging"
)

// serviceCreateParams holds the parameters for the service create command.
// This command requires the credential file (not just a session) because
// it needs the registration token for Matrix account creation.
type serviceCreateParams struct {
	cli.SessionConfig
	Machine         string `json:"machine"           flag:"machine"           desc:"target machine localpart (required)"`
	Name            string `json:"name"              flag:"name"              desc:"service principal localpart (required)"`
	Fleet           string `json:"fleet"             flag:"fleet"             desc:"fleet prefix (e.g., bureau/fleet/prod) (required)"`
	ServerName      string `json:"server_name"       flag:"server-name"       desc:"Matrix server name (auto-detected from machine.conf)"`
	AutoStart       bool   `json:"auto_start"        flag:"auto-start"        desc:"start sandbox automatically" default:"true"`
	NoFleetRegister bool   `json:"no_fleet_register" flag:"no-fleet-register" desc:"skip publishing a FleetServiceContent definition to the fleet room"`
	Failover        string `json:"failover"          flag:"failover"          desc:"failover strategy: none (default), migrate, alert"`
	Priority        int    `json:"priority"          flag:"priority"          desc:"fleet placement priority (0=critical, 10=production, 50=development, 100=batch)" default:"50"`

	cli.JSONOutput
}

// serviceCreateResult is the JSON output for service create.
type serviceCreateResult struct {
	PrincipalUserID string      `json:"principal_user_id"            desc:"full Matrix user ID of the created service"`
	MachineName     string      `json:"machine"                      desc:"machine the service was assigned to"`
	TemplateRef     string      `json:"template"                     desc:"template reference used for the service"`
	ConfigRoomID    ref.RoomID  `json:"config_room_id"               desc:"config room where the assignment was published"`
	ConfigEventID   ref.EventID `json:"config_event_id"              desc:"event ID of the MachineConfig state event"`
	FleetRegistered bool        `json:"fleet_registered"             desc:"whether a FleetServiceContent event was published"`
	FleetRoomID     ref.RoomID  `json:"fleet_room_id,omitempty"      desc:"fleet config room where the service definition was published"`
}

func createCommand() *cli.Command {
	var params serviceCreateParams

	return &cli.Command{
		Name:    "create",
		Summary: "Register and deploy a service on a machine",
		Description: `Create a new service principal and deploy it to a machine.

This performs the full deployment sequence in one operation:
  - Validates that the template exists in Matrix
  - Registers a new Matrix account for the service
  - Encrypts and publishes credentials to the machine's config room
  - Invites the service to the config room
  - Publishes the MachineConfig assignment
  - Publishes a FleetServiceContent definition to the fleet room

The daemon detects the config change via /sync and creates the service's
sandbox. If --auto-start is true (the default), the sandbox starts
immediately.

The fleet registration makes the service visible to the fleet controller
for tracking, status reporting, and future re-placement. By default the
service is registered with Replicas={Min:1, Max:1} and Failover="none"
(pinned to the specified machine). Use --no-fleet-register to skip this
step, or --failover and --priority to customize the fleet definition.

The template must already exist (use "bureau template push" to publish
it first). The machine must be provisioned and have published its age
public key.

The --credential-file is the key=value file from "bureau matrix setup".
It provides the admin session for Matrix operations and the registration
token for creating the service's account.`,
		Usage: "bureau service create <template-ref> --machine <machine> --name <name> --credential-file <path>",
		Examples: []cli.Example{
			{
				Description: "Create a ticket service",
				Command:     "bureau service create bureau/template:ticket-service --machine machine/workstation --name service/ticket --credential-file ./creds",
			},
			{
				Description: "Create without auto-start",
				Command:     "bureau service create bureau/template:artifact-cache --machine machine/workstation --name service/artifact-cache --credential-file ./creds --auto-start=false",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &serviceCreateResult{} },
		RequiredGrants: []string{"command/service/create"},
		Annotations:    cli.Create(),
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) < 1 {
				return cli.Validation("template reference is required\n\nUsage: bureau service create <template-ref> --machine <machine> --name <name> --credential-file <path>")
			}
			if len(args) > 1 {
				return cli.Validation("unexpected argument: %s", args[1])
			}
			if params.SessionConfig.CredentialFile == "" {
				return cli.Validation("--credential-file is required")
			}
			if params.Machine == "" {
				return cli.Validation("--machine is required")
			}
			if params.Name == "" {
				return cli.Validation("--name is required")
			}
			if err := principal.ValidateLocalpart(params.Name); err != nil {
				return cli.Validation("invalid service name: %v", err)
			}

			templateRef, err := schema.ParseTemplateRef(args[0])
			if err != nil {
				return cli.Validation("invalid template reference: %v", err)
			}

			return runCreate(ctx, logger, templateRef, params)
		},
	}
}

func runCreate(ctx context.Context, logger *slog.Logger, templateRef schema.TemplateRef, params serviceCreateParams) error {
	params.ServerName = cli.ResolveServerName(params.ServerName)
	params.Fleet = cli.ResolveFleet(params.Fleet)

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// Connect for admin operations (room management, invites, config
	// publishing). Uses SessionConfig for consistency with machine and
	// agent commands.
	adminSession, err := params.SessionConfig.Connect(ctx)
	if err != nil {
		return err
	}
	defer adminSession.Close()

	// The credential file also contains the registration token,
	// which SessionConfig.Connect() doesn't extract. Read it
	// separately for account registration.
	credentials, err := cli.ReadCredentialFile(params.SessionConfig.CredentialFile)
	if err != nil {
		return cli.Internal("read credential file: %w", err)
	}

	registrationToken := credentials["MATRIX_REGISTRATION_TOKEN"]
	if registrationToken == "" {
		return cli.Validation("credential file missing MATRIX_REGISTRATION_TOKEN").
			WithHint("The credential file must contain MATRIX_REGISTRATION_TOKEN. " +
				"Re-run 'bureau matrix setup' to regenerate the credential file.")
	}

	registrationTokenBuffer, err := secret.NewFromString(registrationToken)
	if err != nil {
		return cli.Internal("protecting registration token: %w", err)
	}
	defer registrationTokenBuffer.Close()

	// Account registration (client.Register) is unauthenticated
	// and needs a Client, not a Session. Create a separate Client
	// for this single HTTP POST.
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

	serverName, err := ref.ParseServerName(params.ServerName)
	if err != nil {
		return cli.Validation("invalid --server-name %q: %w", params.ServerName, err)
	}

	// Parse the machine ref at the CLI boundary. Fleet is derived from the
	// machine ref since every machine belongs to exactly one fleet.
	machine, err := ref.ParseMachine(params.Machine, serverName)
	if err != nil {
		return cli.Validation("invalid machine: %v", err)
	}
	fleet := machine.Fleet()

	// Resolve the fleet machine room for credential provisioning.
	machineRoomAlias := fleet.MachineRoomAlias()
	machineRoomID, err := adminSession.ResolveAlias(ctx, machineRoomAlias)
	if err != nil {
		return cli.NotFound("fleet machine room %s not found: %w", machineRoomAlias, err).
			WithHint("Run 'bureau machine list' to see machines, or 'bureau machine provision' to register one.")
	}

	logger.Info("creating service", "name", params.Name, "machine", params.Machine, "template", templateRef.String())

	principalEntity, err := ref.NewEntityFromAccountLocalpart(fleet, params.Name)
	if err != nil {
		return cli.Validation("invalid service name: %v", err)
	}

	result, err := principal.Create(ctx, client, adminSession, registrationTokenBuffer, credential.AsProvisionFunc(), principal.CreateParams{
		Machine:     machine,
		Principal:   principalEntity,
		TemplateRef: templateRef,
		ValidateTemplate: func(ctx context.Context, templateReference schema.TemplateRef, server ref.ServerName) error {
			_, err := templatedef.Fetch(ctx, adminSession, templateReference, server)
			return err
		},
		HomeserverURL: homeserverURL,
		AutoStart:     params.AutoStart,
		MachineRoomID: machineRoomID,
	})
	if err != nil {
		return cli.Internal("create service: %w", err)
	}

	// Register the service with the fleet controller by publishing a
	// FleetServiceContent state event to the fleet config room. The
	// fleet controller discovers the service via /sync and tracks it
	// for status reporting and future re-placement.
	var fleetRegistered bool
	var fleetRoomID ref.RoomID
	if !params.NoFleetRegister {
		fleetRoomID, err = registerServiceWithFleet(ctx, adminSession, fleet, machine, params.Name, templateRef, params, logger)
		if err != nil {
			return err
		}
		fleetRegistered = true
	}

	if done, err := params.EmitJSON(serviceCreateResult{
		PrincipalUserID: result.PrincipalUserID.String(),
		MachineName:     result.Machine.Localpart(),
		TemplateRef:     result.TemplateRef.String(),
		ConfigRoomID:    result.ConfigRoomID,
		ConfigEventID:   result.ConfigEventID,
		FleetRegistered: fleetRegistered,
		FleetRoomID:     fleetRoomID,
	}); done {
		return err
	}

	logger.Info("service created",
		"principal", result.PrincipalUserID,
		"machine", result.Machine.Localpart(),
		"template", result.TemplateRef.String(),
		"config_room", result.ConfigRoomID,
		"config_event", result.ConfigEventID,
		"fleet_registered", fleetRegistered,
	)

	return nil
}

// registerServiceWithFleet publishes a FleetServiceContent state event to
// the fleet config room, making the service visible to the fleet controller.
// The state key is the service's account localpart (e.g., "service/ticket"),
// matching the fleet controller's expectation.
func registerServiceWithFleet(ctx context.Context, session messaging.Session, fleet ref.Fleet, machine ref.Machine, accountLocalpart string, templateRef schema.TemplateRef, params serviceCreateParams, logger *slog.Logger) (ref.RoomID, error) {
	fleetRoomID, err := session.ResolveAlias(ctx, fleet.RoomAlias())
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			return ref.RoomID{}, cli.NotFound("fleet config room %s not found", fleet.RoomAlias()).
				WithHint("Has the fleet been created? Run 'bureau fleet create' or 'bureau fleet setup' first.")
		}
		return ref.RoomID{}, cli.Transient("resolving fleet config room %s: %w", fleet.RoomAlias(), err)
	}

	failover := fleetschema.FailoverNone
	if params.Failover != "" {
		failover = fleetschema.FailoverStrategy(params.Failover)
		if !failover.IsKnown() {
			return ref.RoomID{}, cli.Validation("--failover must be 'none', 'migrate', or 'alert', got %q", params.Failover)
		}
	}

	content := fleetschema.FleetServiceContent{
		Template: templateRef.String(),
		Replicas: fleetschema.ReplicaSpec{Min: 1, Max: 1},
		Failover: failover,
		Priority: params.Priority,
		Placement: fleetschema.PlacementConstraints{
			PreferredMachines: []string{machine.Localpart()},
		},
	}

	_, err = session.SendStateEvent(ctx, fleetRoomID, schema.EventTypeFleetService, accountLocalpart, content)
	if err != nil {
		return ref.RoomID{}, cli.Internal("publishing fleet service definition: %w", err)
	}

	logger.Info("registered service with fleet",
		"fleet_room", fleetRoomID,
		"state_key", accountLocalpart,
		"failover", failover,
		"priority", params.Priority,
	)

	return fleetRoomID, nil
}
