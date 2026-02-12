// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

func decommissionCommand() *cli.Command {
	var (
		credentialFile string
		serverName     string
	)

	return &cli.Command{
		Name:    "decommission",
		Summary: "Remove a machine from the fleet",
		Description: `Decommission a machine by cleaning up its state from the Bureau fleet.

This removes the machine's key and status from the machine room, clears
its config room state events (machine_config, credentials), and kicks
the machine account from all Bureau rooms.

After decommission, the machine name can be re-provisioned with
"bureau machine provision". The machine's Matrix account remains on the
homeserver but is kicked from all Bureau rooms and its keys are cleared.`,
		Usage: "bureau machine decommission <machine-name> [flags]",
		Examples: []cli.Example{
			{
				Description: "Remove a worker machine",
				Command:     "bureau machine decommission machine/worker-01 --credential-file ./bureau-creds",
			},
		},
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("decommission", pflag.ContinueOnError)
			flagSet.StringVar(&credentialFile, "credential-file", "", "path to Bureau credential file from 'bureau matrix setup' (required)")
			flagSet.StringVar(&serverName, "server-name", "bureau.local", "Matrix server name")
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) < 1 {
				return fmt.Errorf("machine name is required\n\nUsage: bureau machine decommission <machine-name> [flags]")
			}
			machineName := args[0]
			if len(args) > 1 {
				return fmt.Errorf("unexpected argument: %s", args[1])
			}
			if credentialFile == "" {
				return fmt.Errorf("--credential-file is required")
			}
			if err := principal.ValidateLocalpart(machineName); err != nil {
				return fmt.Errorf("invalid machine name: %w", err)
			}

			return runDecommission(machineName, credentialFile, serverName)
		},
	}
}

func runDecommission(machineName, credentialFile, serverName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	credentials, err := cli.ReadCredentialFile(credentialFile)
	if err != nil {
		return fmt.Errorf("read credential file: %w", err)
	}

	homeserverURL := credentials["MATRIX_HOMESERVER_URL"]
	if homeserverURL == "" {
		return fmt.Errorf("credential file missing MATRIX_HOMESERVER_URL")
	}
	adminUserID := credentials["MATRIX_ADMIN_USER"]
	adminToken := credentials["MATRIX_ADMIN_TOKEN"]
	if adminUserID == "" || adminToken == "" {
		return fmt.Errorf("credential file missing MATRIX_ADMIN_USER or MATRIX_ADMIN_TOKEN")
	}

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: homeserverURL,
	})
	if err != nil {
		return fmt.Errorf("create matrix client: %w", err)
	}

	adminSession, err := client.SessionFromToken(adminUserID, adminToken)
	if err != nil {
		return fmt.Errorf("create admin session: %w", err)
	}
	defer adminSession.Close()

	machineUserID := principal.MatrixUserID(machineName, serverName)
	fmt.Fprintf(os.Stderr, "Decommissioning %s (%s)...\n", machineName, machineUserID)

	// Clear machine_key and machine_status state events in the machine room.
	// Sending empty content effectively "deletes" state events in Matrix.
	machineAlias := principal.RoomAlias("bureau/machine", serverName)
	machineRoomID, err := adminSession.ResolveAlias(ctx, machineAlias)
	if err != nil {
		return fmt.Errorf("resolve machine room %q: %w", machineAlias, err)
	}

	_, err = adminSession.SendStateEvent(ctx, machineRoomID, schema.EventTypeMachineKey, machineName, map[string]any{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Warning: could not clear machine_key: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "  Cleared machine_key from %s\n", machineAlias)
	}

	_, err = adminSession.SendStateEvent(ctx, machineRoomID, schema.EventTypeMachineStatus, machineName, map[string]any{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Warning: could not clear machine_status: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "  Cleared machine_status from %s\n", machineAlias)
	}

	// Clean up the config room: clear machine_config and all credentials.
	configAlias := principal.RoomAlias("bureau/config/"+machineName, serverName)
	configRoomID, err := adminSession.ResolveAlias(ctx, configAlias)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			fmt.Fprintf(os.Stderr, "  Config room %s does not exist (skipping)\n", configAlias)
		} else {
			return fmt.Errorf("resolve config room %q: %w", configAlias, err)
		}
	} else {
		// Clear machine_config.
		_, err = adminSession.SendStateEvent(ctx, configRoomID, schema.EventTypeMachineConfig, machineName, map[string]any{})
		if err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: could not clear machine_config: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "  Cleared machine_config from config room\n")
		}

		// Find and clear all credentials state events in the config room.
		clearConfigRoomCredentials(ctx, adminSession, configRoomID)

		// Kick the machine from the config room.
		err = adminSession.KickUser(ctx, configRoomID, machineUserID, "machine decommissioned")
		if err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: could not kick from config room: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "  Kicked from config room\n")
		}
	}

	// Kick from global rooms.
	err = adminSession.KickUser(ctx, machineRoomID, machineUserID, "machine decommissioned")
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Warning: could not kick from machine room: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "  Kicked from %s\n", machineAlias)
	}

	serviceAlias := principal.RoomAlias("bureau/service", serverName)
	serviceRoomID, err := adminSession.ResolveAlias(ctx, serviceAlias)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Warning: could not resolve service room: %v\n", err)
	} else {
		err = adminSession.KickUser(ctx, serviceRoomID, machineUserID, "machine decommissioned")
		if err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: could not kick from service room: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "  Kicked from %s\n", serviceAlias)
		}
	}

	fmt.Fprintf(os.Stderr, "\nMachine %s decommissioned.\n", machineName)
	fmt.Fprintf(os.Stderr, "To re-provision, run: bureau machine provision %s --credential-file <creds>\n", machineName)

	return nil
}

// clearConfigRoomCredentials finds all m.bureau.credentials state events
// in the config room and clears them by sending empty content.
func clearConfigRoomCredentials(ctx context.Context, session *messaging.Session, roomID string) {
	events, err := session.GetRoomState(ctx, roomID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Warning: could not read config room state: %v\n", err)
		return
	}

	credentialsCleared := 0
	for _, event := range events {
		if event.Type != schema.EventTypeCredentials {
			continue
		}
		if event.StateKey == nil {
			continue
		}

		// Check if the credentials have already been cleared (empty content).
		contentBytes, err := json.Marshal(event.Content)
		if err != nil {
			continue
		}
		if string(contentBytes) == "{}" || string(contentBytes) == "null" {
			continue
		}

		_, err = session.SendStateEvent(ctx, roomID, schema.EventTypeCredentials, *event.StateKey, map[string]any{})
		if err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: could not clear credentials for %s: %v\n", *event.StateKey, err)
		} else {
			credentialsCleared++
		}
	}

	if credentialsCleared > 0 {
		fmt.Fprintf(os.Stderr, "  Cleared %d credential(s) from config room\n", credentialsCleared)
	}
}
