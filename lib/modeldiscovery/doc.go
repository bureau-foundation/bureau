// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package modeldiscovery fetches available models from inference
// providers (OpenRouter, OpenAI, Anthropic) and evaluates selection
// rules to generate Bureau model alias configurations.
//
// The package provides a provider-agnostic catalog representation
// ([CatalogEntry]) and a rule engine ([Rule]) for filtering discovered
// models by ID pattern, pricing, context length, and capabilities.
// Provider-specific fetchers implement [CatalogProvider] to normalize
// each provider's model list API into the common catalog format.
//
// CLI commands use this package to browse available models and sync
// discovery rules into model alias state events.
package modeldiscovery
