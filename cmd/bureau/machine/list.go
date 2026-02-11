// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

func listCommand() *cli.Command {
	var (
		session    cli.SessionConfig
		serverName string
	)

	return &cli.Command{
		Name:    "list",
		Summary: "List fleet machines",
		Description: `List all machines that have published keys to the Bureau fleet.

Shows each machine's name, public key, and last status heartbeat
(if available). Reads from the #bureau/machines room state.`,
		Usage: "bureau machine list [flags]",
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("list", pflag.ContinueOnError)
			session.AddFlags(flagSet)
			flagSet.StringVar(&serverName, "server-name", "bureau.local", "Matrix server name")
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("unexpected argument: %s", args[0])
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			matrixSession, err := session.Connect(ctx)
			if err != nil {
				return err
			}
			defer matrixSession.Close()

			return runList(ctx, matrixSession, serverName)
		},
	}
}

// machineInfo collects the key and status for a single machine.
type machineInfo struct {
	name      string
	publicKey string
	algorithm string
	lastSeen  string
}

func runList(ctx context.Context, session *messaging.Session, serverName string) error {
	machinesAlias := principal.RoomAlias("bureau/machines", serverName)
	machinesRoomID, err := session.ResolveAlias(ctx, machinesAlias)
	if err != nil {
		return fmt.Errorf("resolve machines room %q: %w", machinesAlias, err)
	}

	events, err := session.GetRoomState(ctx, machinesRoomID)
	if err != nil {
		return fmt.Errorf("get machines room state: %w", err)
	}

	// Index machine keys and statuses by state_key (machine name).
	machines := make(map[string]*machineInfo)

	for _, event := range events {
		if event.StateKey == nil {
			continue
		}
		stateKey := *event.StateKey

		switch event.Type {
		case schema.EventTypeMachineKey:
			var key schema.MachineKey
			contentBytes, err := json.Marshal(event.Content)
			if err != nil {
				continue
			}
			if err := json.Unmarshal(contentBytes, &key); err != nil {
				continue
			}
			if key.PublicKey == "" {
				// Empty content means the key was cleared (decommissioned).
				continue
			}
			info, exists := machines[stateKey]
			if !exists {
				info = &machineInfo{name: stateKey}
				machines[stateKey] = info
			}
			info.publicKey = key.PublicKey
			info.algorithm = key.Algorithm

		case schema.EventTypeMachineStatus:
			var status schema.MachineStatus
			contentBytes, err := json.Marshal(event.Content)
			if err != nil {
				continue
			}
			if err := json.Unmarshal(contentBytes, &status); err != nil {
				continue
			}
			info, exists := machines[stateKey]
			if !exists {
				info = &machineInfo{name: stateKey}
				machines[stateKey] = info
			}
			// Use LastActivityAt from the status content if available;
			// it's more meaningful than the event's origin_server_ts
			// because it reflects actual daemon activity, not just
			// the most recent heartbeat.
			if status.LastActivityAt != "" {
				info.lastSeen = status.LastActivityAt
			} else if status.Principal != "" {
				// Status exists but no activity yet — mark as online.
				info.lastSeen = "(online, no activity)"
			}
		}
	}

	if len(machines) == 0 {
		fmt.Fprintln(os.Stderr, "No machines found in the fleet.")
		return nil
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "MACHINE\tKEY\tLAST SEEN")

	for _, info := range machines {
		keyDisplay := info.publicKey
		if len(keyDisplay) > 20 {
			keyDisplay = keyDisplay[:20] + "..."
		}
		lastSeen := info.lastSeen
		if lastSeen == "" {
			lastSeen = "-"
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\n", info.name, keyDisplay, lastSeen)
	}
	writer.Flush()

	return nil
}
