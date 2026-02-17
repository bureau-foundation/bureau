// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package context

import "testing"

func TestContextWindowForModel_KnownModels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		model    string
		expected int
	}{
		{"claude-opus-4-6", 200_000},
		{"claude-sonnet-4-5-20250929", 200_000},
		{"gpt-4o", 128_000},
		{"gpt-4", 8_192},
		{"deepseek-chat", 64_000},
		{"gemini-2.0-flash", 1_048_576},
		{"o3", 200_000},
	}

	for _, test := range tests {
		t.Run(test.model, func(t *testing.T) {
			t.Parallel()
			window := ContextWindowForModel(test.model)
			if window != test.expected {
				t.Errorf("ContextWindowForModel(%q) = %d, want %d", test.model, window, test.expected)
			}
		})
	}
}

func TestContextWindowForModel_UnknownModel(t *testing.T) {
	t.Parallel()

	window := ContextWindowForModel("totally-unknown-model-v99")
	if window != defaultContextWindow {
		t.Errorf("ContextWindowForModel(unknown) = %d, want %d", window, defaultContextWindow)
	}
}
