// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package credential

import (
	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
)

// Command returns the "credential" parent command with all subcommands.
func Command() *cli.Command {
	return &cli.Command{
		Name:    "credential",
		Summary: "Manage credential bundles",
		Description: `Provision, list, and audit credential bundles for Bureau machines.

Credential bundles are age-encrypted JSON objects containing secrets
(Matrix tokens, API keys, etc.) for a specific principal on a specific
machine. They are published as m.bureau.credentials state events in
per-machine config rooms.

The "provision" subcommand encrypts credentials with the machine's public
key and publishes them to the machine's config room. The machine's
launcher decrypts them on boot.

The "list" subcommand shows all provisioned credential bundles for a
machine, including key names and encryption recipients (without
revealing the actual secrets).`,
		Subcommands: []*cli.Command{
			provisionCommand(),
			listCommand(),
		},
		Examples: []cli.Example{
			{
				Description: "Provision credentials for a principal",
				Command:     "bureau credential provision --credential-file ./creds --machine machine/worker-01 --principal iree/amdgpu/pm --from-file ./secrets.json",
			},
			{
				Description: "List credential bundles for a machine",
				Command:     "bureau credential list --credential-file ./creds --machine machine/worker-01",
			},
		},
	}
}
