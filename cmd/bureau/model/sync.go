// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/modeldiscovery"
	modelschema "github.com/bureau-foundation/bureau/lib/schema/model"
)

type syncParams struct {
	ModelConnection
	cli.JSONOutput
	Provider       string `flag:"provider,p" desc:"Provider to discover models from (e.g., 'openrouter')" json:"provider"`
	BureauProvider string `flag:"bureau-provider" desc:"Bureau provider name for alias registration (state key in m.bureau.model_provider). Defaults to --provider value." json:"bureau_provider"`
	Match          string `flag:"match,m" desc:"Glob pattern to filter model IDs" json:"match"`
	Exclude        string `flag:"exclude,e" desc:"Comma-separated exclusion glob patterns" json:"exclude"`
	MinContext     int    `flag:"min-context" desc:"Minimum context window in tokens" default:"0" json:"min_context"`
	MaxInputPrice  int64  `flag:"max-input-price" desc:"Maximum input price in microdollars per Mtok" default:"0" json:"max_input_price"`
	AliasPrefix    string `flag:"alias-prefix" desc:"Prefix for generated alias names (e.g., 'qwen3/')" json:"alias_prefix"`
	Capabilities   string `flag:"capabilities" desc:"Comma-separated capability tags for generated aliases" json:"capabilities"`
	DryRun         bool   `flag:"dry-run,n" desc:"Show what would change without applying" json:"dry_run"`
}

type syncResult struct {
	DryRun     bool                         `json:"dry_run"`
	Operations []modelschema.AliasOperation `json:"operations"`
	Created    int                          `json:"created,omitempty"`
	Updated    int                          `json:"updated,omitempty"`
	Deleted    int                          `json:"deleted,omitempty"`
	Errors     []string                     `json:"errors,omitempty"`
}

func syncCommand() *cli.Command {
	var params syncParams

	return &cli.Command{
		Name:    "sync",
		Summary: "Sync model aliases from provider catalog",
		Description: `Discover models from a provider's API, apply filter rules, and
create or update Bureau model alias state events. This is the primary
way to populate the model service's alias registry at scale.

The command fetches the provider's model catalog, filters it using
--match, --exclude, and metadata constraints, generates alias
configurations with pricing from the catalog, and publishes them as
m.bureau.model_alias Matrix state events via the model service.

Use --dry-run to preview what would change before applying.

Requires a model service connection (--service mode from the host,
or inside a sandbox with the model service socket available).`,
		Usage: "bureau model sync [flags]",
		Examples: []cli.Example{
			{
				Description: "Preview syncing Anthropic models",
				Command:     "bureau model sync --service --provider openrouter --match 'anthropic/*' --dry-run",
			},
			{
				Description: "Sync all Qwen3 models with a prefix",
				Command:     "bureau model sync --service --provider openrouter --match 'qwenlm/qwen3*' --exclude '**/*:free' --alias-prefix qwen3/ --capabilities code,streaming",
			},
			{
				Description: "Sync cheap models with large context",
				Command:     "bureau model sync --service --provider openrouter --match '**' --max-input-price 2000000 --min-context 100000 --alias-prefix cheap/",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &syncResult{} },
		Annotations:    cli.Create(),
		RequiredGrants: []string{"command/model/sync"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if params.Provider == "" {
				return cli.Validation("--provider is required").
					WithHint("Specify the provider to discover models from: --provider openrouter")
			}
			if params.Match == "" {
				return cli.Validation("--match is required").
					WithHint("Specify a glob pattern for model IDs: --match 'anthropic/*'")
			}

			// Resolve the catalog provider.
			catalogProvider, err := resolveProvider(params.Provider)
			if err != nil {
				return err
			}

			bureauProvider := params.BureauProvider
			if bureauProvider == "" {
				bureauProvider = params.Provider
			}

			logger.Info("fetching model catalog", "provider", params.Provider)

			// Fetch the catalog.
			entries, err := catalogProvider.Fetch(ctx)
			if err != nil {
				return cli.Transient("fetching catalog: %w", err)
			}
			logger.Info("catalog fetched", "total_models", len(entries))

			// Apply filter rules.
			rule := &modeldiscovery.Rule{
				Match:                            params.Match,
				MinContextLength:                 params.MinContext,
				MaxInputPriceMicrodollarsPerMtok: params.MaxInputPrice,
			}
			if params.Exclude != "" {
				rule.Exclude = strings.Split(params.Exclude, ",")
			}

			filtered := modeldiscovery.ApplyRules(entries, []modeldiscovery.Rule{*rule})
			logger.Info("filter applied", "matched_models", len(filtered))

			if len(filtered) == 0 {
				fmt.Fprintln(os.Stderr, "No models match the specified filters. Nothing to sync.")
				return nil
			}

			// Generate alias operations.
			var capabilities []string
			if params.Capabilities != "" {
				capabilities = strings.Split(params.Capabilities, ",")
			}

			template := modeldiscovery.AliasTemplate{
				Provider:     bureauProvider,
				Prefix:       params.AliasPrefix,
				Capabilities: capabilities,
			}

			operations := modeldiscovery.GenerateAliases(filtered, template)

			// Fetch existing aliases to determine create vs update.
			client, err := params.connect()
			if err != nil {
				return err
			}

			callCtx, cancel := callContext(ctx)
			defer cancel()

			var existingAliases modelschema.ModelListResponse
			if err := client.Call(callCtx, modelschema.ActionList, nil, &existingAliases); err != nil {
				return cli.Internal("fetching existing aliases: %w", err)
			}

			existingSet := make(map[string]bool, len(existingAliases.Aliases))
			for _, alias := range existingAliases.Aliases {
				existingSet[alias.Alias] = true
			}

			// Classify operations as create or update.
			for i := range operations {
				if existingSet[operations[i].Alias] {
					operations[i].Action = "update"
				}
			}

			// Sort for deterministic output.
			sort.Slice(operations, func(i, j int) bool {
				return operations[i].Alias < operations[j].Alias
			})

			if params.DryRun {
				result := syncResult{
					DryRun:     true,
					Operations: operations,
				}
				for _, operation := range operations {
					switch operation.Action {
					case "create":
						result.Created++
					case "update":
						result.Updated++
					}
				}

				if done, err := params.EmitJSON(result); done {
					return err
				}

				printSyncDiff(operations)
				fmt.Fprintf(os.Stderr, "\n%d create, %d update (dry run — no changes applied)\n",
					result.Created, result.Updated)
				return nil
			}

			// Apply operations via the model service.
			syncRequest := modelschema.SyncRequest{Operations: operations}
			var syncResponse modelschema.SyncResponse

			syncCtx, syncCancel := context.WithTimeout(ctx, 120*1000*1000*1000) // 120 seconds for large syncs
			defer syncCancel()

			if err := client.Call(syncCtx, modelschema.ActionSync, syncRequest, &syncResponse); err != nil {
				return cli.Internal("sync failed: %w", err)
			}

			result := syncResult{
				Created:    syncResponse.Created,
				Updated:    syncResponse.Updated,
				Deleted:    syncResponse.Deleted,
				Errors:     syncResponse.Errors,
				Operations: operations,
			}

			if done, err := params.EmitJSON(result); done {
				return err
			}

			fmt.Fprintf(os.Stderr, "Sync complete: %d created, %d updated, %d deleted\n",
				syncResponse.Created, syncResponse.Updated, syncResponse.Deleted)

			if len(syncResponse.Errors) > 0 {
				fmt.Fprintf(os.Stderr, "\n%d errors:\n", len(syncResponse.Errors))
				for _, syncError := range syncResponse.Errors {
					fmt.Fprintf(os.Stderr, "  - %s\n", syncError)
				}
			}

			return nil
		},
	}
}

// printSyncDiff displays the planned alias operations in a table.
func printSyncDiff(operations []modelschema.AliasOperation) {
	writer := tabwriter.NewWriter(os.Stdout, 2, 0, 3, ' ', 0)
	fmt.Fprintf(writer, "ACTION\tALIAS\tPROVIDER\tMODEL\tINPUT $/Mtok\tOUTPUT $/Mtok\n")
	for _, operation := range operations {
		inputPrice := "-"
		outputPrice := "-"
		provider := "-"
		providerModel := "-"
		if operation.Content != nil {
			inputPrice = formatMicrodollars(operation.Content.Pricing.InputPerMtokMicrodollars)
			outputPrice = formatMicrodollars(operation.Content.Pricing.OutputPerMtokMicrodollars)
			provider = operation.Content.Provider
			providerModel = operation.Content.ProviderModel
		}

		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n",
			operation.Action,
			operation.Alias,
			provider,
			providerModel,
			inputPrice,
			outputPrice,
		)
	}
	writer.Flush()
}
