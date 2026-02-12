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

		// Suffix double star with wildcards in prefix.
		{"suffix doublestar wildcard prefix exact", "team-*/**", "team-a", true},
		{"suffix doublestar wildcard prefix child", "team-*/**", "team-a/thing", true},
		{"suffix doublestar wildcard prefix deep", "team-*/**", "team-a/deep/nested", true},
		{"suffix doublestar wildcard prefix no match", "team-*/**", "other/thing", false},
		{"suffix doublestar wildcard multi-seg prefix", "service/*/worker/**", "service/api/worker", true},
		{"suffix doublestar wildcard multi-seg prefix deep", "service/*/worker/**", "service/api/worker/deep", true},
		{"suffix doublestar wildcard multi-seg prefix wrong service", "service/*/worker/**", "service/api/manager/deep", false},
		{"suffix doublestar question mark prefix", "gpu-?/**", "gpu-a", true},
		{"suffix doublestar question mark prefix child", "gpu-?/**", "gpu-a/task", true},
		{"suffix doublestar question mark prefix too long", "gpu-?/**", "gpu-ab/task", false},

		// Prefix double star: "**/suffix".
		{"prefix doublestar matches child", "**/pm", "iree/pm", true},
		{"prefix doublestar matches grandchild", "**/pm", "iree/amdgpu/pm", true},
		{"prefix doublestar matches exact", "**/pm", "pm", true},
		{"prefix doublestar no match", "**/pm", "iree/codegen", false},
		{"prefix doublestar multi-level suffix", "**/amdgpu/pm", "iree/amdgpu/pm", true},

		// Prefix double star with wildcards in suffix.
		{"prefix doublestar wildcard suffix exact", "**/build-*", "build-x", true},
		{"prefix doublestar wildcard suffix child", "**/build-*", "iree/build-y", true},
		{"prefix doublestar wildcard suffix deep", "**/build-*", "iree/deep/build-z", true},
		{"prefix doublestar wildcard suffix no match", "**/build-*", "iree/notbuild", false},
		{"prefix doublestar wildcard multi-seg suffix", "**/sub/worker-?", "team/sub/worker-a", true},
		{"prefix doublestar wildcard multi-seg suffix deep", "**/sub/worker-?", "team/deep/sub/worker-b", true},
		{"prefix doublestar wildcard multi-seg suffix exact", "**/sub/worker-?", "sub/worker-c", true},
		{"prefix doublestar wildcard multi-seg suffix no match", "**/sub/worker-?", "sub/worker-ab", false},

		// Interior double star: "prefix/**/suffix".
		{"interior doublestar zero segments", "iree/**/pm", "iree/pm", true},
		{"interior doublestar one segment", "iree/**/pm", "iree/amdgpu/pm", true},
		{"interior doublestar two segments", "iree/**/pm", "iree/amdgpu/sub/pm", true},
		{"interior doublestar no match suffix", "iree/**/pm", "iree/amdgpu/codegen", false},
		{"interior doublestar no match prefix", "iree/**/pm", "home/amdgpu/pm", false},
		{"interior doublestar rejects empty segment", "iree/**/pm", "iree//pm", false},

		// Interior double star with wildcards in prefix and suffix.
		{"interior wildcard prefix and suffix zero seg", "team-*/**/build-?", "team-a/build-x", true},
		{"interior wildcard prefix and suffix one seg", "team-*/**/build-?", "team-a/sub/build-x", true},
		{"interior wildcard prefix and suffix deep", "team-*/**/build-?", "team-a/deep/sub/build-x", true},
		{"interior wildcard prefix no match", "team-*/**/build-?", "other/sub/build-x", false},
		{"interior wildcard suffix no match", "team-*/**/build-?", "team-a/sub/deploy-x", false},
		{"interior wildcard suffix too long", "team-*/**/build-?", "team-a/sub/build-xy", false},

		// Interior double star with multi-segment prefix/suffix.
		{"interior multi-seg prefix suffix zero seg", "a/b/**/c/d", "a/b/c/d", true},
		{"interior multi-seg prefix suffix one seg", "a/b/**/c/d", "a/b/x/c/d", true},
		{"interior multi-seg prefix suffix two seg", "a/b/**/c/d", "a/b/x/y/c/d", true},
		{"interior multi-seg prefix mismatch", "a/b/**/c/d", "a/x/c/d", false},
		{"interior multi-seg suffix mismatch", "a/b/**/c/d", "a/b/x/c/e", false},

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
