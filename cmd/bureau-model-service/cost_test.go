// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema/model"
)

func TestCalculateCostNilUsage(t *testing.T) {
	pricing := model.Pricing{
		InputPerMtokMicrodollars:  2_500_000,
		OutputPerMtokMicrodollars: 10_000_000,
	}
	cost := calculateCost(nil, pricing)
	if cost != 0 {
		t.Errorf("expected 0 for nil usage, got %d", cost)
	}
}

func TestCalculateCostZeroTokens(t *testing.T) {
	usage := &model.Usage{InputTokens: 0, OutputTokens: 0}
	pricing := model.Pricing{
		InputPerMtokMicrodollars:  2_500_000,
		OutputPerMtokMicrodollars: 10_000_000,
	}
	cost := calculateCost(usage, pricing)
	if cost != 0 {
		t.Errorf("expected 0 for zero tokens, got %d", cost)
	}
}

func TestCalculateCostTypicalCompletion(t *testing.T) {
	// 1000 input tokens at $2.50/Mtok = 2500 microdollars
	// 500 output tokens at $10/Mtok = 5000 microdollars
	// Total: 7500 microdollars
	usage := &model.Usage{InputTokens: 1_000, OutputTokens: 500}
	pricing := model.Pricing{
		InputPerMtokMicrodollars:  2_500_000,
		OutputPerMtokMicrodollars: 10_000_000,
	}
	cost := calculateCost(usage, pricing)
	if cost != 7500 {
		t.Errorf("expected 7500, got %d", cost)
	}
}

func TestCalculateCostEmbedding(t *testing.T) {
	// 5000 input tokens at $0.10/Mtok = 500 microdollars
	// Zero output tokens.
	usage := &model.Usage{InputTokens: 5_000, OutputTokens: 0}
	pricing := model.Pricing{
		InputPerMtokMicrodollars: 100_000,
	}
	cost := calculateCost(usage, pricing)
	if cost != 500 {
		t.Errorf("expected 500, got %d", cost)
	}
}

func TestCalculateCostLocalModel(t *testing.T) {
	// Local models have zero pricing — cost should be zero regardless
	// of token count.
	usage := &model.Usage{InputTokens: 100_000, OutputTokens: 50_000}
	pricing := model.Pricing{}
	cost := calculateCost(usage, pricing)
	if cost != 0 {
		t.Errorf("expected 0 for free model, got %d", cost)
	}
}

func TestCalculateCostTruncatesSubMicrodollar(t *testing.T) {
	// 1 input token at $2.50/Mtok = 0.0025 microdollars → truncates to 0.
	// Integer arithmetic: 1 * 2_500_000 / 1_000_000 = 2 (not 2.5).
	// This tests that we don't accidentally round up.
	usage := &model.Usage{InputTokens: 1, OutputTokens: 0}
	pricing := model.Pricing{
		InputPerMtokMicrodollars: 2_500_000,
	}
	cost := calculateCost(usage, pricing)
	if cost != 2 {
		t.Errorf("expected 2 (integer truncation), got %d", cost)
	}
}

func TestCalculateCostLargeVolume(t *testing.T) {
	// 1 million input tokens at $2.50/Mtok = 2,500,000 microdollars ($2.50)
	// 1 million output tokens at $10/Mtok = 10,000,000 microdollars ($10.00)
	// Total: 12,500,000 microdollars ($12.50)
	usage := &model.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}
	pricing := model.Pricing{
		InputPerMtokMicrodollars:  2_500_000,
		OutputPerMtokMicrodollars: 10_000_000,
	}
	cost := calculateCost(usage, pricing)
	if cost != 12_500_000 {
		t.Errorf("expected 12500000, got %d", cost)
	}
}
