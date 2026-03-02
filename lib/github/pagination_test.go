// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package github

import "testing"

func TestParseLinkNext(t *testing.T) {
	tests := []struct {
		name     string
		header   string
		expected string
	}{
		{
			name:     "empty header",
			header:   "",
			expected: "",
		},
		{
			name:     "next and last",
			header:   `<https://api.github.com/repos/owner/repo/issues?page=2>; rel="next", <https://api.github.com/repos/owner/repo/issues?page=5>; rel="last"`,
			expected: "https://api.github.com/repos/owner/repo/issues?page=2",
		},
		{
			name:     "only last",
			header:   `<https://api.github.com/repos/owner/repo/issues?page=1>; rel="last"`,
			expected: "",
		},
		{
			name:     "next only",
			header:   `<https://api.github.com/repos/owner/repo/issues?page=3>; rel="next"`,
			expected: "https://api.github.com/repos/owner/repo/issues?page=3",
		},
		{
			name:     "full four-link header",
			header:   `<https://api.github.com/repos/owner/repo/issues?page=1>; rel="prev", <https://api.github.com/repos/owner/repo/issues?page=3>; rel="next", <https://api.github.com/repos/owner/repo/issues?page=5>; rel="last", <https://api.github.com/repos/owner/repo/issues?page=1>; rel="first"`,
			expected: "https://api.github.com/repos/owner/repo/issues?page=3",
		},
		{
			name:     "url with query parameters",
			header:   `<https://api.github.com/repos/owner/repo/issues?state=open&per_page=30&page=2>; rel="next"`,
			expected: "https://api.github.com/repos/owner/repo/issues?state=open&per_page=30&page=2",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := parseLinkNext(test.header)
			if got != test.expected {
				t.Errorf("got %q, want %q", got, test.expected)
			}
		})
	}
}
