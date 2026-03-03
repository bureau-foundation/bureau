// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package modeldiscovery

import (
	"fmt"
	"math"
	"strconv"
)

// PerTokenToMicrodollarsPerMtok converts a per-token price string
// (as returned by OpenRouter and other providers) to Bureau's native
// pricing unit: microdollars per million tokens.
//
// The conversion:
//
//	microdollars_per_mtok = price_per_token × 1,000,000 (tokens→Mtok) × 1,000,000 (dollars→µ$)
//	                     = price_per_token × 1e12
//
// Examples:
//
//	"0.000005"   →  5,000,000  (Claude Sonnet input: $5/Mtok)
//	"0.000025"   → 25,000,000  (Claude Opus output: $25/Mtok)
//	"0.00000125" →  1,250,000  (Gemini Pro input: $1.25/Mtok)
//	"0"          →          0  (free models)
//
// Returns an error for unparseable strings. Returns 0 for "-1"
// (OpenRouter's sentinel for dynamic/variable pricing).
func PerTokenToMicrodollarsPerMtok(perToken string) (int64, error) {
	if perToken == "" || perToken == "0" {
		return 0, nil
	}
	// OpenRouter uses "-1" to mean dynamic/variable pricing
	// (only on the openrouter/auto meta-model).
	if perToken == "-1" {
		return 0, nil
	}

	value, err := strconv.ParseFloat(perToken, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing per-token price %q: %w", perToken, err)
	}

	if value < 0 {
		return 0, fmt.Errorf("negative per-token price %q", perToken)
	}
	if value == 0 {
		return 0, nil
	}

	// Multiply by 1e12 to convert from $/token to µ$/Mtok.
	// Round to nearest integer — sub-microdollar precision is
	// meaningless for quota enforcement.
	microdollars := value * 1e12
	return int64(math.Round(microdollars)), nil
}
