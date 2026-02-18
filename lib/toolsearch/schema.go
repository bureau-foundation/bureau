// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package toolsearch

import "encoding/json"

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

	for name, property := range schema.Properties {
		names = append(names, name)
		descriptions = append(descriptions, property.Description)
	}

	return names, descriptions
}
