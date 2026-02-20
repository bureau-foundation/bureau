// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package credential

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	libcred "github.com/bureau-foundation/bureau/lib/credential"
	"github.com/bureau-foundation/bureau/lib/ref"
)

// credentialProvisionParams holds the parameters for credential provision.
type credentialProvisionParams struct {
	cli.SessionConfig
	cli.JSONOutput
	MachineName string `json:"machine"     flag:"machine"     desc:"machine localpart (e.g., machine/workstation) (required)"`
	Principal   string `json:"principal"   flag:"principal"   desc:"principal localpart (e.g., iree/amdgpu/pm) (required)"`
	ServerName  string `json:"server_name" flag:"server-name" desc:"Matrix server name" default:"bureau.local"`
	EscrowKey   string `json:"escrow_key"  flag:"escrow-key"  desc:"operator escrow age public key (optional)"`
	FromFile    string `json:"-"           flag:"from-file"   desc:"read credentials from JSON file instead of stdin"`
}

// credentialProvisionResult is the JSON output for credential provision.
type credentialProvisionResult struct {
	EventID      string   `json:"event_id"      desc:"published state event ID"`
	ConfigRoomID string   `json:"config_room_id" desc:"config room where credentials were published"`
	PrincipalID  string   `json:"principal_id"   desc:"full Matrix user ID of the principal"`
	EncryptedFor []string `json:"encrypted_for"  desc:"identities that can decrypt the bundle"`
	Keys         []string `json:"keys"           desc:"credential names in the bundle"`
}

func provisionCommand() *cli.Command {
	var params credentialProvisionParams

	return &cli.Command{
		Name:    "provision",
		Summary: "Encrypt and publish credentials for a principal",
		Description: `Encrypt a credential bundle with the machine's age public key and
publish it as an m.bureau.credentials state event to the machine's
config room.

The credential bundle is a JSON object mapping credential names to
values (e.g., {"MATRIX_TOKEN": "syt_...", "API_KEY": "sk-..."}). It
can be provided via --from-file or piped to stdin.

The machine's public key is fetched from the m.bureau.machine_key
state event in #bureau/machine. The machine must have completed its
first boot (published its key) before credentials can be provisioned.

Optionally, provide --escrow-key to encrypt the bundle to both the
machine and an operator escrow key for recovery.`,
		Usage: "bureau credential provision --machine <name> --principal <name> [flags]",
		Examples: []cli.Example{
			{
				Description: "Provision from a JSON file",
				Command:     "bureau credential provision --credential-file ./creds --machine machine/worker-01 --principal iree/amdgpu/pm --from-file ./secrets.json",
			},
			{
				Description: "Provision from stdin",
				Command:     "echo '{\"MATRIX_TOKEN\":\"syt_...\"}' | bureau credential provision --credential-file ./creds --machine machine/worker-01 --principal iree/amdgpu/pm",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &credentialProvisionResult{} },
		RequiredGrants: []string{"command/credential/provision"},
		Annotations:    cli.Create(),
		Run: func(args []string) error {
			if len(args) > 0 {
				return cli.Validation("unexpected argument: %s", args[0])
			}
			if params.MachineName == "" {
				return cli.Validation("--machine is required")
			}
			if params.Principal == "" {
				return cli.Validation("--principal is required")
			}

			machine, err := ref.ParseMachine(params.MachineName, params.ServerName)
			if err != nil {
				return cli.Validation("invalid machine name: %v", err)
			}
			principalEntity, err := ref.ParseEntityLocalpart(params.Principal, params.ServerName)
			if err != nil {
				return cli.Validation("invalid principal name: %v", err)
			}

			// Read the credential JSON from file or stdin.
			credentials, err := readCredentialInput(params.FromFile)
			if err != nil {
				return err
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			session, err := params.SessionConfig.Connect(ctx)
			if err != nil {
				return err
			}

			result, err := libcred.Provision(ctx, session, libcred.ProvisionParams{
				Machine:     machine,
				Principal:   principalEntity,
				EscrowKey:   params.EscrowKey,
				Credentials: credentials,
			})
			if err != nil {
				return cli.Internal("provision credentials: %w", err)
			}

			if done, err := params.EmitJSON(credentialProvisionResult{
				EventID:      result.EventID,
				ConfigRoomID: result.ConfigRoomID,
				PrincipalID:  result.PrincipalID,
				EncryptedFor: result.EncryptedFor,
				Keys:         result.Keys,
			}); done {
				return err
			}

			fmt.Fprintf(os.Stderr, "Provisioned credentials for %s on %s\n", params.Principal, params.MachineName)
			fmt.Fprintf(os.Stderr, "  Keys: %s\n", strings.Join(result.Keys, ", "))
			fmt.Fprintf(os.Stderr, "  Encrypted for: %s\n", strings.Join(result.EncryptedFor, ", "))
			fmt.Fprintf(os.Stderr, "  Config room: %s\n", result.ConfigRoomID)
			fmt.Fprintf(os.Stderr, "  Event: %s\n", result.EventID)
			return nil
		},
	}
}

// readCredentialInput reads and parses a credential JSON object from
// either a file (if fromFile is non-empty) or stdin.
func readCredentialInput(fromFile string) (map[string]string, error) {
	var reader io.Reader
	if fromFile != "" {
		file, err := os.Open(fromFile)
		if err != nil {
			return nil, cli.Validation("opening credential file %q: %w", fromFile, err)
		}
		defer file.Close()
		reader = file
	} else {
		reader = os.Stdin
	}

	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, cli.Internal("reading credential input: %w", err)
	}
	if len(data) == 0 {
		return nil, cli.Validation("no credential data provided (pipe JSON to stdin or use --from-file)")
	}

	var credentials map[string]string
	if err := json.Unmarshal(data, &credentials); err != nil {
		return nil, cli.Validation("parsing credential JSON: %w", err)
	}
	if len(credentials) == 0 {
		return nil, cli.Validation("credential JSON is empty")
	}

	return credentials, nil
}
