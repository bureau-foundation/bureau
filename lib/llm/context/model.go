// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package context

// modelRegistry maps model identifiers to their context window sizes
// in tokens. This is a best-effort lookup — models not in the registry
// fall back to defaultContextWindow.
//
// Values are from provider documentation as of early 2026. When a model
// family has multiple sizes (e.g., GPT-4 8k vs 32k), we use the most
// common default. The caller can always override via configuration.
var modelRegistry = map[string]int{
	// Anthropic Claude.
	"claude-opus-4-6":            200_000,
	"claude-sonnet-4-5-20250929": 200_000,
	"claude-haiku-4-5-20251001":  200_000,
	"claude-3-5-sonnet-20241022": 200_000,
	"claude-3-5-haiku-20241022":  200_000,
	"claude-3-opus-20240229":     200_000,

	// OpenAI GPT-4 family.
	"gpt-4o":      128_000,
	"gpt-4o-mini": 128_000,
	"gpt-4-turbo": 128_000,
	"gpt-4":       8_192,
	"gpt-4-32k":   32_768,

	// OpenAI reasoning models.
	"o1":      200_000,
	"o1-mini": 128_000,
	"o3":      200_000,
	"o3-mini": 200_000,

	// DeepSeek.
	"deepseek-chat":     64_000,
	"deepseek-reasoner": 64_000,

	// Google Gemini.
	"gemini-2.0-flash": 1_048_576,
	"gemini-2.0-pro":   1_048_576,
	"gemini-1.5-flash": 1_048_576,
	"gemini-1.5-pro":   2_097_152,

	// Mistral.
	"mistral-large-latest": 128_000,
	"mistral-small-latest": 32_000,

	// Meta Llama (common quantized deployments).
	"llama-3.1-405b": 128_000,
	"llama-3.1-70b":  128_000,
	"llama-3.1-8b":   128_000,
}

// defaultContextWindow is used when a model is not in the registry.
// 128k is a conservative middle ground — large enough for most modern
// models, small enough that we don't wildly overestimate capacity for
// older or smaller models. The caller should set an explicit context
// window via configuration when using models not in the registry.
const defaultContextWindow = 128_000

// ContextWindowForModel returns the context window size in tokens for
// the given model identifier. Returns defaultContextWindow (128k) if
// the model is not in the registry.
func ContextWindowForModel(model string) int {
	if window, found := modelRegistry[model]; found {
		return window
	}
	return defaultContextWindow
}
