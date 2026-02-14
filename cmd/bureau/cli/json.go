// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"os"
)

// WriteJSON marshals value as indented JSON and writes it to stdout.
// This is the standard way for commands to produce machine-readable
// output when --json is set.
//
// Callers that output slices should initialize them with make([]T, 0)
// rather than a nil var declaration, so that empty results serialize
// as [] instead of null.
func WriteJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
