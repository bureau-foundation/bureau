// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package agentdriver

import (
	"slices"
	"sort"
	"testing"
)

func TestExtractMentions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "no mentions",
			body: "hello world",
			want: nil,
		},
		{
			name: "single mention at start",
			body: "@agent/worker:bureau.local fix the build",
			want: []string{"@agent/worker:bureau.local"},
		},
		{
			name: "single mention mid-sentence",
			body: "hey @agent/worker:bureau.local fix the build",
			want: []string{"@agent/worker:bureau.local"},
		},
		{
			name: "single mention at end",
			body: "you should ask @agent/worker:bureau.local",
			want: []string{"@agent/worker:bureau.local"},
		},
		{
			name: "multiple mentions",
			body: "@agent/worker:bureau.local and @agent/pm:bureau.local please coordinate",
			want: []string{"@agent/worker:bureau.local", "@agent/pm:bureau.local"},
		},
		{
			name: "duplicate mentions deduplicated",
			body: "@agent/worker:bureau.local then @agent/worker:bureau.local again",
			want: []string{"@agent/worker:bureau.local"},
		},
		{
			name: "mention followed by comma",
			body: "ask @agent/worker:bureau.local, they know",
			want: []string{"@agent/worker:bureau.local"},
		},
		{
			name: "mention followed by period",
			body: "done by @agent/worker:bureau.local.",
			want: []string{"@agent/worker:bureau.local"},
		},
		{
			name: "mention in parentheses",
			body: "(cc @agent/pm:bureau.local)",
			want: []string{"@agent/pm:bureau.local"},
		},
		{
			name: "server with port",
			body: "@agent/worker:localhost:8448 check this",
			want: []string{"@agent/worker:localhost:8448"},
		},
		{
			name: "fleet-scoped agent localpart",
			body: "@bureau/fleet/prod/agent/sysadmin:bureau.local status",
			want: []string{"@bureau/fleet/prod/agent/sysadmin:bureau.local"},
		},
		{
			name: "localpart with dots and equals",
			body: "ask @user.name=test:matrix.org about it",
			want: []string{"@user.name=test:matrix.org"},
		},
		{
			name: "email address not matched",
			body: "send to user@example.com please",
			want: nil,
		},
		{
			name: "longer server name is valid mention",
			body: "not @foo:bar.local.evil.com but okay",
			want: []string{"@foo:bar.local.evil.com"},
		},
		{
			name: "no at signs",
			body: "just a regular message with no at signs",
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := extractMentions(tt.body)
			if !equalStringSlices(tt.want, got) {
				t.Errorf("extractMentions(%q) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}

func TestExtractEventMentions(t *testing.T) {
	t.Parallel()

	t.Run("structured mentions only", func(t *testing.T) {
		t.Parallel()
		content := map[string]any{
			"m.mentions": map[string]any{
				"user_ids": []any{"@agent/worker:bureau.local"},
			},
		}
		got := extractEventMentions(content, "no mentions in body")
		want := []string{"@agent/worker:bureau.local"}
		if !equalStringSlices(want, got) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("body mentions only", func(t *testing.T) {
		t.Parallel()
		content := map[string]any{}
		got := extractEventMentions(content, "hey @agent/pm:bureau.local do this")
		want := []string{"@agent/pm:bureau.local"}
		if !equalStringSlices(want, got) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("both sources deduplicated", func(t *testing.T) {
		t.Parallel()
		content := map[string]any{
			"m.mentions": map[string]any{
				"user_ids": []any{"@agent/worker:bureau.local"},
			},
		}
		got := extractEventMentions(content, "hey @agent/worker:bureau.local and @agent/pm:bureau.local")
		want := []string{"@agent/pm:bureau.local", "@agent/worker:bureau.local"}
		sort.Strings(got)
		if !slices.Equal(want, got) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("no mentions anywhere", func(t *testing.T) {
		t.Parallel()
		content := map[string]any{}
		got := extractEventMentions(content, "broadcast message")
		if len(got) != 0 {
			t.Errorf("expected no mentions, got %v", got)
		}
	})
}

// equalStringSlices compares two string slices for equality, treating
// nil and empty as equivalent.
func equalStringSlices(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return slices.Equal(a, b)
}
