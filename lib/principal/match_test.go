// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package principal

import "testing"

func TestMatchPattern(t *testing.T) {
	tests := []struct {
		name      string
		pattern   string
		localpart string
		want      bool
	}{
		// Exact matches.
		{"exact match", "bureau-admin", "bureau-admin", true},
		{"exact mismatch", "bureau-admin", "bureau-operator", false},
		{"exact with slashes", "iree/amdgpu/pm", "iree/amdgpu/pm", true},
		{"exact with slashes mismatch", "iree/amdgpu/pm", "iree/amdgpu/codegen", false},

		// Universal match.
		{"double star matches anything", "**", "bureau-admin", true},
		{"double star matches nested", "**", "iree/amdgpu/pm", true},
		{"double star matches deeply nested", "**", "a/b/c/d/e", true},

		// Single-segment wildcard (does not cross /).
		{"star matches single segment", "iree/*", "iree/pm", true},
		{"star does not cross slash", "iree/*", "iree/amdgpu/pm", false},
		{"star at end", "service/*", "service/stt", true},
		{"star in middle", "iree/*/pm", "iree/amdgpu/pm", true},
		{"star in middle no match", "iree/*/pm", "iree/amdgpu/codegen", false},
		{"star in middle too deep", "iree/*/pm", "iree/amdgpu/sub/pm", false},

		// Suffix double star: "prefix/**".
		{"suffix doublestar matches child", "iree/**", "iree/pm", true},
		{"suffix doublestar matches grandchild", "iree/**", "iree/amdgpu/pm", true},
		{"suffix doublestar matches deep", "iree/**", "iree/amdgpu/sub/deep", true},
		{"suffix doublestar matches exact prefix", "iree/**", "iree", true},
		{"suffix doublestar no match different prefix", "iree/**", "home/user", false},
		{"suffix doublestar no match partial prefix", "iree/**", "ireex/pm", false},
		{"suffix doublestar multi-level prefix", "iree/amdgpu/**", "iree/amdgpu/pm", true},
		{"suffix doublestar multi-level prefix deep", "iree/amdgpu/**", "iree/amdgpu/sub/pm", true},
		{"suffix doublestar multi-level prefix no match", "iree/amdgpu/**", "iree/nvidia/pm", false},

		// Prefix double star: "**/suffix".
		{"prefix doublestar matches child", "**/pm", "iree/pm", true},
		{"prefix doublestar matches grandchild", "**/pm", "iree/amdgpu/pm", true},
		{"prefix doublestar matches exact", "**/pm", "pm", true},
		{"prefix doublestar no match", "**/pm", "iree/codegen", false},
		{"prefix doublestar multi-level suffix", "**/amdgpu/pm", "iree/amdgpu/pm", true},

		// Interior double star: "prefix/**/suffix".
		{"interior doublestar zero segments", "iree/**/pm", "iree/pm", true},
		{"interior doublestar one segment", "iree/**/pm", "iree/amdgpu/pm", true},
		{"interior doublestar two segments", "iree/**/pm", "iree/amdgpu/sub/pm", true},
		{"interior doublestar no match suffix", "iree/**/pm", "iree/amdgpu/codegen", false},
		{"interior doublestar no match prefix", "iree/**/pm", "home/amdgpu/pm", false},
		{"interior doublestar rejects empty segment", "iree/**/pm", "iree//pm", false},

		// Question mark wildcard.
		{"question mark matches single char", "iree/amdgpu/p?", "iree/amdgpu/pm", true},
		{"question mark does not match slash", "iree?amdgpu/pm", "iree/amdgpu/pm", false},
		{"question mark too short", "iree/amdgpu/p?", "iree/amdgpu/p", false},

		// Edge cases.
		{"empty pattern", "", "", true},
		{"empty pattern nonempty input", "", "x", false},
		{"empty input nonempty pattern", "x", "", false},
		{"malformed bracket pattern denies", "[invalid", "x", false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := MatchPattern(test.pattern, test.localpart)
			if got != test.want {
				t.Errorf("MatchPattern(%q, %q) = %v, want %v",
					test.pattern, test.localpart, got, test.want)
			}
		})
	}
}

func TestMatchAnyPattern(t *testing.T) {
	tests := []struct {
		name      string
		patterns  []string
		localpart string
		want      bool
	}{
		{
			"empty patterns denies",
			nil,
			"bureau-admin",
			false,
		},
		{
			"single exact match",
			[]string{"bureau-admin"},
			"bureau-admin",
			true,
		},
		{
			"no match in list",
			[]string{"bureau-admin", "iree/**"},
			"home/user",
			false,
		},
		{
			"second pattern matches",
			[]string{"bureau-admin", "iree/**"},
			"iree/amdgpu/pm",
			true,
		},
		{
			"multiple patterns first wins",
			[]string{"**", "iree/**"},
			"anything/at/all",
			true,
		},
		{
			"realistic admin + team pattern",
			[]string{"bureau-admin", "iree/**"},
			"bureau-admin",
			true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := MatchAnyPattern(test.patterns, test.localpart)
			if got != test.want {
				t.Errorf("MatchAnyPattern(%v, %q) = %v, want %v",
					test.patterns, test.localpart, got, test.want)
			}
		})
	}
}
