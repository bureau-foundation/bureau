// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package modeldiscovery

import (
	"context"
	"time"
)

// CatalogEntry is a normalized representation of a model discovered
// from a provider's API. Provider-specific fetchers convert their
// native response formats into this common type.
type CatalogEntry struct {
	// ID is the provider-specific model identifier. For OpenRouter,
	// this is "provider/model-name" (e.g., "anthropic/claude-opus-4.6").
	// The ID is the value that goes in the API's "model" field.
	ID string

	// Name is a human-readable display name (e.g., "Anthropic: Claude
	// Opus 4.6"). Used for catalog listing output.
	Name string

	// ContextLength is the maximum context window in tokens.
	ContextLength int

	// InputModalities lists the input types the model accepts
	// (e.g., ["text", "image", "file"]).
	InputModalities []string

	// OutputModalities lists the output types the model produces
	// (e.g., ["text"], ["text", "image"]).
	OutputModalities []string

	// InputPriceMicrodollarsPerMtok is the input token cost in
	// Bureau's native unit: microdollars per million tokens.
	InputPriceMicrodollarsPerMtok int64

	// OutputPriceMicrodollarsPerMtok is the output token cost.
	OutputPriceMicrodollarsPerMtok int64

	// SupportedParameters lists the API parameters this model
	// accepts (e.g., "tools", "reasoning", "structured_outputs",
	// "temperature"). Used for capability-based filtering.
	SupportedParameters []string

	// Created is when the model was first available on the provider.
	Created time.Time

	// ExpirationDate is the scheduled deprecation date in ISO 8601
	// format (e.g., "2026-06-01"). Empty if no deprecation is
	// scheduled.
	ExpirationDate string

	// Description is a human-readable summary of the model.
	Description string
}

// CatalogProvider fetches the available model catalog from a provider.
// Implementations handle the provider-specific API and normalize the
// response into [CatalogEntry] values.
type CatalogProvider interface {
	// Name returns the provider identifier (e.g., "openrouter").
	Name() string

	// Fetch retrieves the current model catalog. Returns all models
	// the provider makes available, regardless of the caller's
	// account or API key permissions.
	Fetch(ctx context.Context) ([]CatalogEntry, error)
}
