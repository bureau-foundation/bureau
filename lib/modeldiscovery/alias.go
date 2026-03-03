// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package modeldiscovery

import (
	"strings"

	"github.com/bureau-foundation/bureau/lib/schema/model"
)

// AliasTemplate controls how discovered catalog entries are converted
// to Bureau model aliases.
type AliasTemplate struct {
	// Provider is the Bureau provider name that serves these models.
	// This is the state key in m.bureau.model_provider — it must
	// match a registered provider. Required.
	Provider string

	// Prefix is prepended to the generated alias name. For example,
	// prefix "qwen3/" with model ID "qwenlm/qwen3-32b" produces
	// alias "qwen3/qwen3-32b". Empty prefix uses the bare model
	// name (after stripping the provider prefix from the ID).
	Prefix string

	// Capabilities are tags attached to every generated alias
	// (e.g., "code", "streaming"). These complement the model's
	// own capabilities from the catalog.
	Capabilities []string
}

// GenerateAlias converts a catalog entry into a Bureau alias name and
// ModelAliasContent using the given template.
//
// The alias name is constructed by:
//  1. Stripping the provider prefix from the model ID (e.g.,
//     "qwenlm/qwen3-32b" → "qwen3-32b")
//  2. Prepending the template prefix (e.g., "qwen3/" + "qwen3-32b"
//     → "qwen3/qwen3-32b")
//
// If the model ID has no "/" separator, the full ID is used as the
// bare name.
func GenerateAlias(entry CatalogEntry, template AliasTemplate) (string, model.ModelAliasContent) {
	// Strip the provider prefix from the model ID to get the bare
	// model name. E.g., "anthropic/claude-sonnet-4.6" → "claude-sonnet-4.6"
	bareName := entry.ID
	if slashIndex := strings.IndexByte(entry.ID, '/'); slashIndex >= 0 {
		bareName = entry.ID[slashIndex+1:]
	}

	aliasName := template.Prefix + bareName

	content := model.ModelAliasContent{
		Provider:      template.Provider,
		ProviderModel: entry.ID,
		Pricing: model.Pricing{
			InputPerMtokMicrodollars:  entry.InputPriceMicrodollarsPerMtok,
			OutputPerMtokMicrodollars: entry.OutputPriceMicrodollarsPerMtok,
		},
		Capabilities: template.Capabilities,
	}

	return aliasName, content
}

// GenerateAliases converts a slice of catalog entries into alias
// operations suitable for the model/sync action. Each entry produces
// one "create" operation. The caller is responsible for diffing
// against existing aliases to determine which operations should be
// "update" or "delete" instead.
func GenerateAliases(entries []CatalogEntry, template AliasTemplate) []model.AliasOperation {
	operations := make([]model.AliasOperation, len(entries))
	for i, entry := range entries {
		aliasName, content := GenerateAlias(entry, template)
		contentCopy := content
		operations[i] = model.AliasOperation{
			Action:  "create",
			Alias:   aliasName,
			Content: &contentCopy,
		}
	}
	return operations
}
