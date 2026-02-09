// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"testing"
)

func TestGlobFilter_Check(t *testing.T) {
	tests := []struct {
		name    string
		filter  GlobFilter
		args    []string
		wantErr bool
	}{
		{
			name:    "empty filter allows everything",
			filter:  GlobFilter{},
			args:    []string{"anything", "at", "all"},
			wantErr: false,
		},
		{
			name: "exact match allowed",
			filter: GlobFilter{
				Allowed: []string{"pr list"},
			},
			args:    []string{"pr", "list"},
			wantErr: false,
		},
		{
			name: "wildcard match allowed",
			filter: GlobFilter{
				Allowed: []string{"pr *"},
			},
			args:    []string{"pr", "list"},
			wantErr: false,
		},
		{
			name: "wildcard match with multiple args",
			filter: GlobFilter{
				Allowed: []string{"pr *"},
			},
			args:    []string{"pr", "create", "--title", "Fix bug"},
			wantErr: false,
		},
		{
			name: "no match denied",
			filter: GlobFilter{
				Allowed: []string{"pr *"},
			},
			args:    []string{"issue", "list"},
			wantErr: true,
		},
		{
			name: "blocked takes precedence over allowed",
			filter: GlobFilter{
				Allowed: []string{"*"},
				Blocked: []string{"repo delete *"},
			},
			args:    []string{"repo", "delete", "foo/bar"},
			wantErr: true,
		},
		{
			name: "blocked pattern exact match",
			filter: GlobFilter{
				Blocked: []string{"auth login"},
			},
			args:    []string{"auth", "login"},
			wantErr: true,
		},
		{
			name: "similar command not blocked",
			filter: GlobFilter{
				Blocked: []string{"auth login"},
			},
			args:    []string{"auth", "status"},
			wantErr: false,
		},
		{
			name: "multiple allowed patterns",
			filter: GlobFilter{
				Allowed: []string{"pr *", "issue *", "repo view *"},
			},
			args:    []string{"issue", "create", "--title", "Bug"},
			wantErr: false,
		},
		{
			name: "middle wildcard",
			filter: GlobFilter{
				Allowed: []string{"api repos/*/pulls/*"},
			},
			args:    []string{"api", "repos/foo/bar/pulls/123"},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.filter.Check(tt.args)
			if (err != nil) != tt.wantErr {
				t.Errorf("GlobFilter.Check() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		pattern string
		str     string
		want    bool
	}{
		// Exact matches
		{"hello", "hello", true},
		{"hello", "world", false},
		{"hello", "hello world", false},

		// Single wildcard
		{"*", "", true},
		{"*", "anything", true},
		{"hello *", "hello world", true},
		{"hello *", "hello", false},
		{"* world", "hello world", true},
		{"* world", "world", false}, // No space before "world"

		// Multiple wildcards
		{"*hello*", "say hello there", true},
		{"*hello*", "hello", true},
		{"*hello*", "say hi", false},

		// Prefix/suffix wildcards
		{"pr *", "pr list", true},
		{"pr *", "pr", false},
		{"* --help", "gh --help", true},

		// Path-like patterns
		{"repos/*/pulls/*", "repos/foo/pulls/123", true},
		{"repos/*/pulls/*", "repos/foo/bar/pulls/123", true},
		{"repos/*/pulls/*", "repos/foo/issues/123", false},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+"_"+tt.str, func(t *testing.T) {
			if got := matchGlob(tt.pattern, tt.str); got != tt.want {
				t.Errorf("matchGlob(%q, %q) = %v, want %v", tt.pattern, tt.str, got, tt.want)
			}
		})
	}
}

func TestAllowAllFilter(t *testing.T) {
	filter := &AllowAllFilter{}

	// Should allow everything
	if err := filter.Check([]string{"dangerous", "command"}); err != nil {
		t.Errorf("AllowAllFilter.Check() = %v, want nil", err)
	}
}

func TestDenyAllFilter(t *testing.T) {
	filter := &DenyAllFilter{Reason: "testing"}

	// Should deny everything
	err := filter.Check([]string{"any", "command"})
	if err == nil {
		t.Error("DenyAllFilter.Check() = nil, want error")
	}
	if err.Error() != "service disabled: testing" {
		t.Errorf("DenyAllFilter.Check() error = %q, want %q", err.Error(), "service disabled: testing")
	}
}
