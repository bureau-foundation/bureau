// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/credential"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/secret"
	"github.com/bureau-foundation/bureau/lib/template"
	"github.com/bureau-foundation/bureau/messaging"
)

// serviceCreateParams holds the parameters for the service create command.
// This command requires the credential file (not just a session) because
// it needs the registration token for Matrix account creation.
type serviceCreateParams struct {
	CredentialFile string `json:"-"            flag:"credential-file" desc:"path to Bureau credential file from 'bureau matrix setup' (required)"`
	Machine        string `json:"machine"      flag:"machine"         desc:"target machine localpart (required)"`
	Name           string `json:"name"         flag:"name"            desc:"service principal localpart (required)"`
	Fleet          string `json:"fleet"        flag:"fleet"           desc:"fleet prefix (e.g., bureau/fleet/prod) (required)"`
	ServerName     string `json:"server_name"  flag:"server-name"     desc:"Matrix server name" default:"bureau.local"`
	AutoStart      bool   `json:"auto_start"   flag:"auto-start"      desc:"start sandbox automatically" default:"true"`

	cli.JSONOutput
}

// serviceCreateResult is the JSON output for service create.
type serviceCreateResult struct {
	PrincipalUserID string     `json:"principal_user_id" desc:"full Matrix user ID of the created service"`
	MachineName     string     `json:"machine"           desc:"machine the service was assigned to"`
	TemplateRef     string     `json:"template"          desc:"template reference used for the service"`
	ConfigRoomID    ref.RoomID `json:"config_room_id"    desc:"config room where the assignment was published"`
	ConfigEventID   string     `json:"config_event_id"   desc:"event ID of the MachineConfig state event"`
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
		Run: func(args []string) error {
			if len(args) < 1 {
				return cli.Validation("template reference is required\n\nUsage: bureau service create <template-ref> --machine <machine> --name <name> --credential-file <path>")
			}
			if len(args) > 1 {
				return cli.Validation("unexpected argument: %s", args[1])
			}
			if params.CredentialFile == "" {
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

	// Read the credential file for admin access and the registration token.
	credentials, err := cli.ReadCredentialFile(params.CredentialFile)
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

	registrationTokenBuffer, err := secret.NewFromString(registrationToken)
	if err != nil {
		return cli.Internal("protecting registration token: %w", err)
	}
	defer registrationTokenBuffer.Close()

	parsedAdminUserID, err := ref.ParseUserID(adminUserID)
	if err != nil {
		return cli.Internal("parse admin user ID: %w", err)
	}

	adminSession, err := client.SessionFromToken(parsedAdminUserID, adminToken)
	if err != nil {
		return cli.Internal("create admin session: %w", err)
	}
	defer adminSession.Close()

	// Parse the machine ref at the CLI boundary. Fleet is derived from the
	// machine ref since every machine belongs to exactly one fleet.
	machine, err := ref.ParseMachine(params.Machine, params.ServerName)
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

	fmt.Fprintf(os.Stderr, "Creating service %s on %s (template %s)...\n", params.Name, params.Machine, templateRef)

	principalEntity, err := ref.NewEntityFromAccountLocalpart(fleet, params.Name)
	if err != nil {
		return cli.Validation("invalid service name: %v", err)
	}

	result, err := principal.Create(ctx, client, adminSession, registrationTokenBuffer, credential.AsProvisionFunc(), principal.CreateParams{
		Machine:     machine,
		Principal:   principalEntity,
		TemplateRef: templateRef,
		ValidateTemplate: func(ctx context.Context, ref schema.TemplateRef, serverName string) error {
			_, err := template.Fetch(ctx, adminSession, ref, serverName)
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
