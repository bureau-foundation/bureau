// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package model

import "github.com/bureau-foundation/bureau/cmd/bureau/cli"

// Command returns the "model" subcommand group.
func Command() *cli.Command {
	return &cli.Command{
		Name:    "model",
		Summary: "Interact with the model inference service",
		Description: `Send completion and embedding requests to the Bureau model service,
list available model aliases, and inspect account quota usage.

The model service routes requests to configured providers (OpenRouter,
local llama.cpp, etc.) based on model aliases. Agents request models
by alias; the model service resolves to a concrete provider and model
at request time.

Commands connect to the model service's Unix socket. Inside a sandbox,
the socket and token paths default to the standard provisioned
locations. Outside a sandbox, use --service mode (requires 'bureau
login') or explicit --socket and --token-file flags.`,
		Subcommands: []*cli.Command{
			catalogCommand(),
			completeCommand(),
			embedCommand(),
			listCommand(),
			statusCommand(),
			syncCommand(),
		},
		Examples: []cli.Example{
			{
				Description: "Send a completion request",
				Command:     "bureau model complete --model codex 'Explain this error message'",
			},
			{
				Description: "Stream a completion to stdout",
				Command:     "echo 'Review this code' | bureau model complete --model codex",
			},
			{
				Description: "List available model aliases",
				Command:     "bureau model list --service",
			},
			{
				Description: "Check quota usage",
				Command:     "bureau model status --service",
			},
		},
	}
}
