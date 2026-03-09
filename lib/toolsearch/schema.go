// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package toolsearch

import (
	"encoding/json"
	"sort"
)

// ExtractArguments extracts parameter names and descriptions from a
// JSON Schema object (the inputSchema of a tool definition). Returns
// the top-level property names and their description strings.
//
// Returns empty slices for nil, empty, or malformed schemas. This is
// best-effort: tool schemas that are valid JSON but don't follow the
// expected structure (object with properties) are silently skipped.
func ExtractArguments(inputSchema json.RawMessage) (names, descriptions []string) {
	if len(inputSchema) == 0 {
		return nil, nil
	}

	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}

	if err := json.Unmarshal(inputSchema, &schema); err != nil {
		return nil, nil
	}

	// Sort property names for deterministic output. The caller pairs
	// names[i] with descriptions[i], so both slices must be in the
	// same stable order.
	sortedNames := make([]string, 0, len(schema.Properties))
	for name := range schema.Properties {
		sortedNames = append(sortedNames, name)
	}
	sort.Strings(sortedNames)

	names = make([]string, 0, len(sortedNames))
	descriptions = make([]string, 0, len(sortedNames))
	for _, name := range sortedNames {
		names = append(names, name)
		descriptions = append(descriptions, schema.Properties[name].Description)
	}

	return names, descriptions
}
