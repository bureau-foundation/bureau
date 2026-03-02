// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	modelschema "github.com/bureau-foundation/bureau/lib/schema/model"
)

type listParams struct {
	ModelConnection
	cli.JSONOutput
}

// listEntry is a single alias in the list output. Mirrors the
// server-side modelEntry but uses json tags for CBOR decoding
// (the CBOR codec falls back to json tags).
type listEntry struct {
	Alias         string                 `json:"alias"          desc:"model alias name"`
	Provider      string                 `json:"provider"       desc:"provider name"`
	ProviderModel string                 `json:"provider_model" desc:"provider-specific model ID"`
	Capabilities  []string               `json:"capabilities"   desc:"model capabilities"`
	Pricing       modelschema.Pricing    `json:"pricing"        desc:"cost per million tokens in microdollars"`
	Endpoint      string                 `json:"endpoint"       desc:"provider endpoint URL"`
	AuthMethod    modelschema.AuthMethod `json:"auth_method"    desc:"authentication method"`
}

// listResult is the full response from the model/list action.
type listResult struct {
	Aliases   []listEntry `json:"aliases"   desc:"registered model aliases"`
	Providers int         `json:"providers" desc:"number of registered providers"`
	Accounts  int         `json:"accounts"  desc:"number of registered accounts"`
}

func listCommand() *cli.Command {
	var params listParams

	return &cli.Command{
		Name:    "list",
		Summary: "List available model aliases",
		Description: `Show all registered model aliases with their resolved provider,
model name, capabilities, and pricing. This is the primary way to
discover which models are available for completion and embedding
requests.`,
		Usage: "bureau model list [flags]",
		Examples: []cli.Example{
			{
				Description: "List models from the host",
				Command:     "bureau model list --service",
			},
			{
				Description: "List models as JSON",
				Command:     "bureau model list --json",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &listResult{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/model/list"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext(ctx)
			defer cancel()

			var result listResult
			if err := client.Call(ctx, modelschema.ActionList, nil, &result); err != nil {
				return err
			}

			if done, err := params.EmitJSON(result); done {
				return err
			}

			if len(result.Aliases) == 0 {
				logger.Info("no model aliases registered")
				return nil
			}

			// Sort by alias name for stable output.
			sort.Slice(result.Aliases, func(i, j int) bool {
				return result.Aliases[i].Alias < result.Aliases[j].Alias
			})

			writer := tabwriter.NewWriter(os.Stdout, 2, 0, 3, ' ', 0)
			fmt.Fprintf(writer, "ALIAS\tPROVIDER\tMODEL\tINPUT $/Mtok\tOUTPUT $/Mtok\tCAPABILITIES\n")
			for _, entry := range result.Aliases {
				inputPrice := formatMicrodollars(entry.Pricing.InputPerMtokMicrodollars)
				outputPrice := formatMicrodollars(entry.Pricing.OutputPerMtokMicrodollars)
				capabilities := "-"
				if len(entry.Capabilities) > 0 {
					capabilities = joinCapabilities(entry.Capabilities)
				}
				fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n",
					entry.Alias,
					entry.Provider,
					entry.ProviderModel,
					inputPrice,
					outputPrice,
					capabilities,
				)
			}
			writer.Flush()

			fmt.Fprintf(os.Stderr, "\n%d aliases, %d providers, %d accounts\n",
				len(result.Aliases), result.Providers, result.Accounts)

			return nil
		},
	}
}

// formatMicrodollars formats a microdollar amount as a dollar string.
// Returns "free" for zero.
func formatMicrodollars(microdollars int64) string {
	if microdollars == 0 {
		return "free"
	}
	return fmt.Sprintf("$%.2f", float64(microdollars)/1_000_000)
}

// joinCapabilities joins capability strings with commas, truncating
// if the result would be too long.
func joinCapabilities(capabilities []string) string {
	result := ""
	for index, capability := range capabilities {
		if index > 0 {
			result += ", "
		}
		result += capability
	}
	if len(result) > 40 {
		return result[:37] + "..."
	}
	return result
}
