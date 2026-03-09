// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ref_test

import (
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
)

func TestValidatePathSegment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		segment string
		wantErr bool
	}{
		// Valid segments.
		{name: "simple", segment: "hello"},
		{name: "alphanumeric", segment: "abc123"},
		{name: "with-dash", segment: "my-name"},
		{name: "with-underscore", segment: "my_name"},
		{name: "with-dot-middle", segment: "v1.0.0"},
		{name: "single-char", segment: "a"},
		{name: "single-digit", segment: "0"},

		// Path traversal: empty.
		{name: "empty", segment: "", wantErr: true},

		// Path traversal: dot segments.
		{name: "single-dot", segment: ".", wantErr: true},
		{name: "double-dot", segment: "..", wantErr: true},

		// Hidden files/directories (leading dot).
		{name: "hidden-file", segment: ".hidden", wantErr: true},
		{name: "hidden-dotgit", segment: ".git", wantErr: true},
		{name: "hidden-dotenv", segment: ".env", wantErr: true},
		{name: "hidden-dotdotdot", segment: "...", wantErr: true},

		// Valid segments that contain dots but don't start with one.
		{name: "dot-in-middle", segment: "file.txt"},
		{name: "multiple-dots", segment: "archive.tar.gz"},
		{name: "ends-with-dot", segment: "trailing."},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := ref.ValidatePathSegment(test.segment, "test segment")
			if test.wantErr && err == nil {
				t.Errorf("ValidatePathSegment(%q) = nil, want error", test.segment)
			}
			if !test.wantErr && err != nil {
				t.Errorf("ValidatePathSegment(%q) = %v, want nil", test.segment, err)
			}
		})
	}
}

func TestValidatePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		path    string
		wantErr string // substring of error, empty means no error
	}{
		// Valid paths.
		{name: "simple", path: "alice"},
		{name: "two-segments", path: "machine/workstation"},
		{name: "deep-hierarchy", path: "bureau/fleet/prod/service/stt"},
		{name: "with-dots", path: "v1.0/release"},
		{name: "with-underscores", path: "my_agent/sub_task"},
		{name: "with-hyphens", path: "my-agent/sub-task"},
		{name: "with-equals", path: "key=value/test"},
		{name: "numeric", path: "agent42/task0"},
		{name: "single-char", path: "a"},
		{name: "all-allowed", path: "az09._=-/test"},

		// Character validation.
		{name: "uppercase", path: "Alice", wantErr: "invalid character"},
		{name: "space", path: "alice bob", wantErr: "invalid character"},
		{name: "at-sign", path: "@alice", wantErr: "invalid character"},
		{name: "colon", path: "alice:bob", wantErr: "invalid character"},
		{name: "hash", path: "#room", wantErr: "invalid character"},
		{name: "backslash", path: "path\\to", wantErr: "invalid character"},
		{name: "tab", path: "alice\tbob", wantErr: "invalid character"},
		{name: "tilde", path: "~alice", wantErr: "invalid character"},
		{name: "star", path: "iree/*", wantErr: "invalid character"},
		{name: "null-byte", path: "alice\x00bob", wantErr: "invalid character"},
		{name: "exclamation", path: "room!id", wantErr: "invalid character"},

		// Structural: leading/trailing slash.
		{name: "leading-slash", path: "/alice", wantErr: "must not start with /"},
		{name: "trailing-slash", path: "alice/", wantErr: "must not end with /"},
		{name: "only-slash", path: "/", wantErr: "must not start with /"},

		// Structural: empty segments (double slash).
		{name: "double-slash", path: "alice//bob", wantErr: "is empty"},
		{name: "triple-slash", path: "a///b", wantErr: "is empty"},

		// Path traversal via ValidatePathSegment.
		{name: "dotdot-segment", path: "alice/../bob", wantErr: "path traversal"},
		{name: "dotdot-only", path: "..", wantErr: "path traversal"},
		{name: "dotdot-start", path: "../alice", wantErr: "path traversal"},
		{name: "dotdot-end", path: "alice/..", wantErr: "path traversal"},
		{name: "dot-segment", path: "alice/./bob", wantErr: "path traversal"},

		// Hidden segments via ValidatePathSegment.
		{name: "hidden-first", path: ".hidden", wantErr: "starts with '.'"},
		{name: "hidden-middle", path: "alice/.hidden/bob", wantErr: "starts with '.'"},
		{name: "dotgit", path: "repo/.git/config", wantErr: "starts with '.'"},
		{name: "dotenv", path: "app/.env", wantErr: "starts with '.'"},
		{name: "triple-dot", path: "alice/.../bob", wantErr: "starts with '.'"},

		// Dots are fine when not leading a segment.
		{name: "dot-in-middle", path: "amd.gpu"},
		{name: "dot-at-end", path: "version1.0"},
		{name: "dots-in-segments", path: "a.b/c.d/e.f"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := ref.ValidatePath(test.path, "test path")
			if test.wantErr == "" {
				if err != nil {
					t.Errorf("ValidatePath(%q) = %v, want nil", test.path, err)
				}
			} else {
				if err == nil {
					t.Errorf("ValidatePath(%q) = nil, want error containing %q", test.path, test.wantErr)
				} else if !strings.Contains(err.Error(), test.wantErr) {
					t.Errorf("ValidatePath(%q) = %v, want error containing %q", test.path, err, test.wantErr)
				}
			}
		})
	}
}
