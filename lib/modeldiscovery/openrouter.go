// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package modeldiscovery

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// OpenRouterDefaultEndpoint is the public models listing endpoint.
// No authentication required.
const OpenRouterDefaultEndpoint = "https://openrouter.ai/api/v1/models"

// OpenRouterProvider fetches the model catalog from OpenRouter's
// public API. The endpoint returns all models available on the
// platform with pricing, capabilities, and metadata.
type OpenRouterProvider struct {
	// Endpoint overrides the default API URL. Used for testing
	// with a local server. When empty, uses OpenRouterDefaultEndpoint.
	Endpoint string

	// Client is the HTTP client for API requests. When nil, uses
	// http.DefaultClient.
	Client *http.Client
}

// Name implements CatalogProvider.
func (provider *OpenRouterProvider) Name() string {
	return "openrouter"
}

// Fetch implements CatalogProvider. Retrieves the full model catalog
// from OpenRouter and converts it to normalized CatalogEntry values.
func (provider *OpenRouterProvider) Fetch(ctx context.Context) ([]CatalogEntry, error) {
	endpoint := provider.Endpoint
	if endpoint == "" {
		endpoint = OpenRouterDefaultEndpoint
	}
	client := provider.Client
	if client == nil {
		client = http.DefaultClient
	}

	request, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	request.Header.Set("Accept", "application/json")

	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetching models from %s: %w", endpoint, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("models endpoint returned HTTP %d: %s", response.StatusCode, string(body))
	}

	// Limit response body to 50 MB — the current response is ~2 MB
	// but the catalog grows over time.
	var envelope openRouterModelsResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, 50*1024*1024))
	if err := decoder.Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decoding models response: %w", err)
	}

	entries := make([]CatalogEntry, 0, len(envelope.Data))
	for _, raw := range envelope.Data {
		entry, err := convertOpenRouterModel(&raw)
		if err != nil {
			// Skip models with invalid pricing rather than failing
			// the entire catalog. Log-worthy but not fatal — the
			// catalog is best-effort.
			continue
		}
		entries = append(entries, entry)
	}

	return entries, nil
}

// convertOpenRouterModel converts an OpenRouter model object to a
// normalized CatalogEntry.
func convertOpenRouterModel(raw *openRouterModel) (CatalogEntry, error) {
	inputPrice, err := PerTokenToMicrodollarsPerMtok(raw.Pricing.Prompt)
	if err != nil {
		return CatalogEntry{}, fmt.Errorf("model %s: input pricing: %w", raw.ID, err)
	}
	outputPrice, err := PerTokenToMicrodollarsPerMtok(raw.Pricing.Completion)
	if err != nil {
		return CatalogEntry{}, fmt.Errorf("model %s: output pricing: %w", raw.ID, err)
	}

	var created time.Time
	if raw.Created > 0 {
		created = time.Unix(int64(raw.Created), 0)
	}

	return CatalogEntry{
		ID:                             raw.ID,
		Name:                           raw.Name,
		ContextLength:                  raw.ContextLength,
		InputModalities:                raw.Architecture.InputModalities,
		OutputModalities:               raw.Architecture.OutputModalities,
		InputPriceMicrodollarsPerMtok:  inputPrice,
		OutputPriceMicrodollarsPerMtok: outputPrice,
		SupportedParameters:            raw.SupportedParameters,
		Created:                        created,
		ExpirationDate:                 raw.ExpirationDate,
		Description:                    raw.Description,
	}, nil
}

// --- OpenRouter API response types ---

type openRouterModelsResponse struct {
	Data []openRouterModel `json:"data"`
}

type openRouterModel struct {
	ID                  string                 `json:"id"`
	Name                string                 `json:"name"`
	Created             float64                `json:"created"`
	Description         string                 `json:"description"`
	ContextLength       int                    `json:"context_length"`
	Architecture        openRouterArchitecture `json:"architecture"`
	Pricing             openRouterPricing      `json:"pricing"`
	TopProvider         openRouterTopProvider  `json:"top_provider"`
	SupportedParameters []string               `json:"supported_parameters"`
	ExpirationDate      string                 `json:"expiration_date"`
}

type openRouterArchitecture struct {
	Modality         string   `json:"modality"`
	InputModalities  []string `json:"input_modalities"`
	OutputModalities []string `json:"output_modalities"`
	Tokenizer        string   `json:"tokenizer"`
}

type openRouterPricing struct {
	Prompt     string `json:"prompt"`
	Completion string `json:"completion"`
	Image      string `json:"image,omitempty"`
	Audio      string `json:"audio,omitempty"`
}

type openRouterTopProvider struct {
	ContextLength       *int `json:"context_length"`
	MaxCompletionTokens *int `json:"max_completion_tokens"`
	IsModerated         bool `json:"is_moderated"`
}
