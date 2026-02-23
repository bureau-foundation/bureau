// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"fmt"
	"log/slog"
	"os"
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

// serviceCreateParams holds the parameters for the service create command.
// This command requires the credential file (not just a session) because
// it needs the registration token for Matrix account creation.
type serviceCreateParams struct {
	cli.SessionConfig
	Machine    string `json:"machine"      flag:"machine"         desc:"target machine localpart (required)"`
	Name       string `json:"name"         flag:"name"            desc:"service principal localpart (required)"`
	Fleet      string `json:"fleet"        flag:"fleet"           desc:"fleet prefix (e.g., bureau/fleet/prod) (required)"`
	ServerName string `json:"server_name"  flag:"server-name"     desc:"Matrix server name" default:"bureau.local"`
	AutoStart  bool   `json:"auto_start"   flag:"auto-start"      desc:"start sandbox automatically" default:"true"`

	cli.JSONOutput
}

// serviceCreateResult is the JSON output for service create.
type serviceCreateResult struct {
	PrincipalUserID string      `json:"principal_user_id" desc:"full Matrix user ID of the created service"`
	MachineName     string      `json:"machine"           desc:"machine the service was assigned to"`
	TemplateRef     string      `json:"template"          desc:"template reference used for the service"`
	ConfigRoomID    ref.RoomID  `json:"config_room_id"    desc:"config room where the assignment was published"`
	ConfigEventID   ref.EventID `json:"config_event_id"   desc:"event ID of the MachineConfig state event"`
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

The daemon detects the config change via /sync and creates the service's
sandbox. If --auto-start is true (the default), the sandbox starts
immediately.

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
		Run: func(_ context.Context, args []string, _ *slog.Logger) error {
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

			return runCreate(templateRef, params)
		},
	}
}

func runCreate(templateRef schema.TemplateRef, params serviceCreateParams) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Connect for admin operations (room management, invites, config
	// publishing). Uses SessionConfig for consistency with machine and
	// agent commands.
	adminSession, err := params.SessionConfig.Connect(ctx)
	if err != nil {
		return cli.Internal("connect: %w", err)
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
		return cli.Validation("credential file missing MATRIX_REGISTRATION_TOKEN")
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
		return fmt.Errorf("invalid --server-name: %w", err)
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
		return cli.Internal("resolve fleet machine room %q: %w", machineRoomAlias, err)
	}

	logger := cli.NewCommandLogger().With("command", "service/create", "name", params.Name, "template", templateRef.String())
	logger.Info("creating service", "machine", params.Machine)

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

	if done, err := params.EmitJSON(serviceCreateResult{
		PrincipalUserID: result.PrincipalUserID.String(),
		MachineName:     result.Machine.Localpart(),
		TemplateRef:     result.TemplateRef.String(),
		ConfigRoomID:    result.ConfigRoomID,
		ConfigEventID:   result.ConfigEventID,
	}); done {
		return err
	}

	fmt.Fprintf(os.Stderr, "  Principal: %s\n", result.PrincipalUserID)
	fmt.Fprintf(os.Stderr, "  Machine:   %s\n", result.Machine.Localpart())
	fmt.Fprintf(os.Stderr, "  Template:  %s\n", result.TemplateRef.String())
	fmt.Fprintf(os.Stderr, "  Config:    %s\n", result.ConfigRoomID)
	fmt.Fprintf(os.Stderr, "  Event:     %s\n", result.ConfigEventID)
	fmt.Fprintf(os.Stderr, "\nThe daemon will create the sandbox on its next reconciliation cycle.\n")

	return nil
}
