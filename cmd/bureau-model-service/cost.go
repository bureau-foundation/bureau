// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import "github.com/bureau-foundation/bureau/lib/schema/model"

// calculateCost computes the cost of a model request in microdollars
// from the token usage and the model's pricing table.
//
// The formula:
//
//	cost = (input_tokens * input_price_per_mtok / 1_000_000) +
//	       (output_tokens * output_price_per_mtok / 1_000_000)
//
// Both pricing values are in microdollars per million tokens. The
// division by 1,000,000 converts from per-million-token rate to
// per-token cost. Integer arithmetic means sub-microdollar fractions
// are truncated (not rounded), which slightly undercounts cost. This
// is acceptable: quotas are guardrails, and truncation favors the user.
func calculateCost(usage *model.Usage, pricing model.Pricing) int64 {
	if usage == nil {
		return 0
	}

	inputCost := usage.InputTokens * pricing.InputPerMtokMicrodollars / 1_000_000
	outputCost := usage.OutputTokens * pricing.OutputPerMtokMicrodollars / 1_000_000

	return inputCost + outputCost
}
