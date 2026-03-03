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
)

type catalogParams struct {
	cli.JSONOutput
	Match            string `flag:"match,m" desc:"Glob pattern to filter model IDs (e.g., 'anthropic/*', 'qwenlm/qwen3*')"`
	Exclude          string `flag:"exclude,e" desc:"Comma-separated glob patterns to exclude (e.g., '**/*:free,**/*preview*')"`
	MinContext       int    `flag:"min-context" desc:"Minimum context window in tokens" default:"0"`
	MaxInputPrice    int64  `flag:"max-input-price" desc:"Maximum input price in microdollars per Mtok (0 = no limit)" default:"0"`
	RequireTools     bool   `flag:"require-tools" desc:"Only show models that support tool use"`
	RequireReasoning bool   `flag:"require-reasoning" desc:"Only show models that support reasoning"`
	SortBy           string `flag:"sort,s" desc:"Sort by: name, input-price, output-price, context" default:"name"`
}

type catalogResult struct {
	Provider string                        `json:"provider"`
	Count    int                           `json:"count"`
	Models   []modeldiscovery.CatalogEntry `json:"models"`
}

func catalogCommand() *cli.Command {
	var params catalogParams

	return &cli.Command{
		Name:    "catalog",
		Summary: "Browse available models from a provider",
		Description: `Fetch the model catalog from a provider's API and display available
models with pricing, context length, and capabilities. This is a
read-only exploration command — it does not create aliases or modify
any Bureau configuration.

Currently supports OpenRouter (no authentication required). The
OpenRouter catalog includes 300+ models from all major providers
with detailed pricing and capability metadata.

Use --match and --exclude to filter models by ID pattern, and
--min-context and --max-input-price for metadata filtering.`,
		Usage: "bureau model catalog <provider> [flags]",
		Examples: []cli.Example{
			{
				Description: "List all available models on OpenRouter",
				Command:     "bureau model catalog openrouter",
			},
			{
				Description: "Show Anthropic models only",
				Command:     "bureau model catalog openrouter --match 'anthropic/*'",
			},
			{
				Description: "Cheap models with large context",
				Command:     "bureau model catalog openrouter --max-input-price 2000000 --min-context 100000",
			},
			{
				Description: "Models with tool use, excluding free variants",
				Command:     "bureau model catalog openrouter --require-tools --exclude '**/*:free'",
			},
			{
				Description: "Output as JSON for scripting",
				Command:     "bureau model catalog openrouter --match 'qwenlm/*' --json",
			},
		},
		Params:      func() any { return &params },
		Output:      func() any { return &catalogResult{} },
		Annotations: cli.ReadOnly(),
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) == 0 {
				return cli.Validation("provider name is required (e.g., 'openrouter')").
					WithHint("Currently supported providers: openrouter")
			}

			providerName := args[0]
			provider, err := resolveProvider(providerName)
			if err != nil {
				return err
			}

			logger.Info("fetching model catalog", "provider", providerName)

			entries, err := provider.Fetch(ctx)
			if err != nil {
				return cli.Internal("fetching catalog: %w", err)
			}

			logger.Info("catalog fetched", "provider", providerName, "total_models", len(entries))

			// Apply filters if any are set.
			entries = applyCatalogFilters(entries, &params)

			// Sort entries.
			sortCatalogEntries(entries, params.SortBy)

			result := catalogResult{
				Provider: providerName,
				Count:    len(entries),
				Models:   entries,
			}

			if done, err := params.EmitJSON(result); done {
				return err
			}

			if len(entries) == 0 {
				fmt.Fprintln(os.Stderr, "No models match the specified filters.")
				return nil
			}

			writer := tabwriter.NewWriter(os.Stdout, 2, 0, 3, ' ', 0)
			fmt.Fprintf(writer, "MODEL ID\tINPUT $/Mtok\tOUTPUT $/Mtok\tCONTEXT\tMODALITY\n")
			for _, entry := range entries {
				inputPrice := formatMicrodollars(entry.InputPriceMicrodollarsPerMtok)
				outputPrice := formatMicrodollars(entry.OutputPriceMicrodollarsPerMtok)
				context := formatContextLength(entry.ContextLength)
				modality := formatModalities(entry.InputModalities, entry.OutputModalities)

				fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n",
					entry.ID,
					inputPrice,
					outputPrice,
					context,
					modality,
				)
			}
			writer.Flush()

			fmt.Fprintf(os.Stderr, "\n%d models shown\n", len(entries))
			return nil
		},
	}
}

// resolveProvider returns a CatalogProvider for the given name.
func resolveProvider(name string) (modeldiscovery.CatalogProvider, error) {
	switch name {
	case "openrouter":
		return &modeldiscovery.OpenRouterProvider{}, nil
	default:
		return nil, cli.Validation("unknown provider %q", name).
			WithHint("Currently supported providers: openrouter")
	}
}

// applyCatalogFilters constructs a Rule from the CLI params and
// applies it to the catalog entries.
func applyCatalogFilters(entries []modeldiscovery.CatalogEntry, params *catalogParams) []modeldiscovery.CatalogEntry {
	rule := buildFilterRule(params)
	if rule == nil {
		return entries
	}

	var filtered []modeldiscovery.CatalogEntry
	for _, entry := range entries {
		if rule.Matches(entry) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

// buildFilterRule constructs a Rule from CLI params. Returns nil if
// no filters are set (show everything).
func buildFilterRule(params *catalogParams) *modeldiscovery.Rule {
	hasFilter := params.Match != "" ||
		params.Exclude != "" ||
		params.MinContext > 0 ||
		params.MaxInputPrice > 0 ||
		params.RequireTools ||
		params.RequireReasoning

	if !hasFilter {
		return nil
	}

	rule := &modeldiscovery.Rule{
		Match:                            params.Match,
		MinContextLength:                 params.MinContext,
		MaxInputPriceMicrodollarsPerMtok: params.MaxInputPrice,
	}

	// Default match to "**" (everything) when other filters are set
	// but no match pattern is specified.
	if rule.Match == "" {
		rule.Match = "**"
	}

	if params.Exclude != "" {
		rule.Exclude = strings.Split(params.Exclude, ",")
	}

	if params.RequireTools {
		rule.RequiredParameters = append(rule.RequiredParameters, "tools")
	}
	if params.RequireReasoning {
		rule.RequiredParameters = append(rule.RequiredParameters, "reasoning")
	}

	return rule
}

// sortCatalogEntries sorts entries by the given key.
func sortCatalogEntries(entries []modeldiscovery.CatalogEntry, sortBy string) {
	switch sortBy {
	case "input-price":
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].InputPriceMicrodollarsPerMtok < entries[j].InputPriceMicrodollarsPerMtok
		})
	case "output-price":
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].OutputPriceMicrodollarsPerMtok < entries[j].OutputPriceMicrodollarsPerMtok
		})
	case "context":
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].ContextLength > entries[j].ContextLength
		})
	default: // "name" or anything else
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].ID < entries[j].ID
		})
	}
}

// formatContextLength formats a token count as a human-readable string.
func formatContextLength(tokens int) string {
	if tokens >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(tokens)/1_000_000)
	}
	if tokens >= 1_000 {
		return fmt.Sprintf("%dK", tokens/1_000)
	}
	return fmt.Sprintf("%d", tokens)
}

// formatModalities formats input/output modalities as a compact
// string like "text+image→text".
func formatModalities(input, output []string) string {
	if len(input) == 0 && len(output) == 0 {
		return "-"
	}
	return strings.Join(input, "+") + "→" + strings.Join(output, "+")
}
