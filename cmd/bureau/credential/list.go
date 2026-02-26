// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package credential

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	libcred "github.com/bureau-foundation/bureau/lib/credential"
	"github.com/bureau-foundation/bureau/lib/ref"
)

// credentialListParams holds the parameters for credential list.
type credentialListParams struct {
	cli.SessionConfig
	cli.JSONOutput
	MachineName string `json:"machine"     flag:"machine"     desc:"machine localpart (e.g., machine/workstation) (required)"`
	ServerName  string `json:"server_name" flag:"server-name" desc:"Matrix server name (auto-detected from machine.conf)"`
}

// credentialListEntry is a single entry in the JSON output.
type credentialListEntry struct {
	StateKey      string     `json:"state_key"       desc:"principal localpart"`
	Principal     ref.UserID `json:"principal"       desc:"full Matrix user ID"`
	EncryptedFor  []string   `json:"encrypted_for"   desc:"identities that can decrypt"`
	Keys          []string   `json:"keys"            desc:"credential names"`
	ProvisionedBy ref.UserID `json:"provisioned_by"  desc:"who provisioned"`
	ProvisionedAt string     `json:"provisioned_at"  desc:"when provisioned"`
}

func listCommand() *cli.Command {
	var params credentialListParams

	return &cli.Command{
		Name:    "list",
		Summary: "List credential bundles for a machine",
		Description: `List all provisioned credential bundles in a machine's config room.

Shows key names, encryption recipients, and provisioning metadata for
each credential bundle — without revealing the actual encrypted values.
Useful for auditing which principals have credentials on a machine.`,
		Usage: "bureau credential list --machine <name> [flags]",
		Examples: []cli.Example{
			{
				Description: "List credentials for a machine",
				Command:     "bureau credential list --credential-file ./creds --machine machine/worker-01",
			},
			{
				Description: "List credentials as JSON",
				Command:     "bureau credential list --credential-file ./creds --machine machine/worker-01 --json",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &[]credentialListEntry{} },
		RequiredGrants: []string{"command/credential/list"},
		Annotations:    cli.ReadOnly(),
		Run: func(ctx context.Context, args []string, _ *slog.Logger) error {
			if len(args) > 0 {
				return cli.Validation("unexpected argument: %s", args[0])
			}
			if params.MachineName == "" {
				return cli.Validation("--machine is required")
			}

			params.ServerName = cli.ResolveServerName(params.ServerName)

			serverName, err := ref.ParseServerName(params.ServerName)
			if err != nil {
				return fmt.Errorf("invalid --server-name: %w", err)
			}

			machine, err := ref.ParseMachine(params.MachineName, serverName)
			if err != nil {
				return cli.Validation("invalid machine name: %v", err)
			}

			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()

			session, err := params.SessionConfig.Connect(ctx)
			if err != nil {
				return err
			}

			result, err := libcred.List(ctx, session, machine)
			if err != nil {
				return cli.Internal("list credentials: %w", err)
			}

			entries := make([]credentialListEntry, len(result.Bundles))
			for i, bundle := range result.Bundles {
				entries[i] = credentialListEntry{
					StateKey:      bundle.StateKey,
					Principal:     bundle.Principal,
					EncryptedFor:  bundle.EncryptedFor,
					Keys:          bundle.Keys,
					ProvisionedBy: bundle.ProvisionedBy,
					ProvisionedAt: bundle.ProvisionedAt,
				}
			}

			if done, err := params.EmitJSON(entries); done {
				return err
			}

			if len(entries) == 0 {
				fmt.Fprintf(os.Stdout, "No credential bundles found for %s\n", params.MachineName)
				return nil
			}

			writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(writer, "PRINCIPAL\tKEYS\tENCRYPTED FOR\tPROVISIONED BY")
			for _, entry := range entries {
				fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n",
					entry.StateKey,
					strings.Join(entry.Keys, ", "),
					strings.Join(entry.EncryptedFor, ", "),
					entry.ProvisionedBy,
				)
			}
			return writer.Flush()
		},
	}
}
