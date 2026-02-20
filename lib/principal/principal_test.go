// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package principal

import (
	"strings"
	"testing"
)

func TestValidateLocalpart(t *testing.T) {
	tests := []struct {
		name      string
		localpart string
		wantErr   string // substring of error message, empty means no error expected
	}{
		// Valid localparts.
		{name: "simple", localpart: "alice", wantErr: ""},
		{name: "with_slash", localpart: "iree/amdgpu/pm", wantErr: ""},
		{name: "two_segments", localpart: "machine/workstation", wantErr: ""},
		{name: "deep_hierarchy", localpart: "service/stt/whisper/v2", wantErr: ""},
		{name: "with_dots", localpart: "iree/amdgpu.v3/pm", wantErr: ""},
		{name: "with_underscores", localpart: "my_agent/sub_task", wantErr: ""},
		{name: "with_hyphens", localpart: "my-agent/sub-task", wantErr: ""},
		{name: "with_equals", localpart: "key=value", wantErr: ""},
		{name: "numeric", localpart: "agent42/task0", wantErr: ""},
		{name: "single_char", localpart: "a", wantErr: ""},
		{name: "max_length", localpart: strings.Repeat("a", MaxLocalpartLength), wantErr: ""},
		{name: "all_allowed_chars", localpart: "az09._=-/test", wantErr: ""},

		// Empty localpart.
		{name: "empty", localpart: "", wantErr: "localpart is empty"},

		// Too long.
		{name: "one_over_max", localpart: strings.Repeat("a", MaxLocalpartLength+1), wantErr: "maximum is 84"},

		// Invalid characters.
		{name: "uppercase", localpart: "Alice", wantErr: "invalid character"},
		{name: "space", localpart: "alice bob", wantErr: "invalid character"},
		{name: "at_sign", localpart: "@alice", wantErr: "invalid character"},
		{name: "colon", localpart: "alice:bob", wantErr: "invalid character"},
		{name: "hash", localpart: "#room", wantErr: "invalid character"},
		{name: "exclamation", localpart: "room!id", wantErr: "invalid character"},
		{name: "backslash", localpart: "path\\to", wantErr: "invalid character"},
		{name: "tab", localpart: "alice\tbob", wantErr: "invalid character"},
		{name: "tilde", localpart: "~alice", wantErr: "invalid character"},
		{name: "star", localpart: "iree/*", wantErr: "invalid character"},

		// Structural: leading/trailing slash.
		{name: "leading_slash", localpart: "/alice", wantErr: "must not start with /"},
		{name: "trailing_slash", localpart: "alice/", wantErr: "must not end with /"},
		{name: "only_slash", localpart: "/", wantErr: "must not start with /"},

		// Structural: empty segments (double slash).
		{name: "double_slash", localpart: "alice//bob", wantErr: "empty segment"},
		{name: "triple_slash", localpart: "a///b", wantErr: "empty segment"},

		// Path traversal.
		{name: "dotdot_segment", localpart: "alice/../bob", wantErr: "path traversal"},
		{name: "dotdot_only", localpart: "..", wantErr: "path traversal"},
		{name: "dotdot_start", localpart: "../alice", wantErr: "path traversal"},
		{name: "dotdot_end", localpart: "alice/..", wantErr: "path traversal"},

		// Hidden files (segments starting with dot).
		{name: "hidden_first_segment", localpart: ".hidden", wantErr: "starts with '.'"},
		{name: "hidden_later_segment", localpart: "alice/.hidden/bob", wantErr: "starts with '.'"},
		{name: "dot_only_segment", localpart: "alice/./bob", wantErr: "starts with '.'"},
		{name: "dotfile_segment", localpart: "alice/.config", wantErr: "starts with '.'"},

		// Dots allowed when not leading a segment.
		{name: "dot_in_middle", localpart: "amd.gpu", wantErr: ""},
		{name: "dot_at_end", localpart: "version1.0", wantErr: ""},
		{name: "multiple_dots_middle", localpart: "a.b.c", wantErr: ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateLocalpart(test.localpart)
			if test.wantErr == "" {
				if err != nil {
					t.Errorf("ValidateLocalpart(%q) = %v, want nil", test.localpart, err)
				}
			} else {
				if err == nil {
					t.Errorf("ValidateLocalpart(%q) = nil, want error containing %q", test.localpart, test.wantErr)
				} else if !strings.Contains(err.Error(), test.wantErr) {
					t.Errorf("ValidateLocalpart(%q) = %v, want error containing %q", test.localpart, err, test.wantErr)
				}
			}
		})
	}
}

func TestValidateRelativePath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		label   string
		wantErr string
	}{
		// Valid paths.
		{name: "simple", path: "feature/amdgpu", label: "worktree path"},
		{name: "single_segment", path: "main", label: "workspace name"},
		{name: "deep", path: "a/b/c/d", label: "worktree path"},
		{name: "with_dots_dashes", path: "v1.0/feature-branch", label: "worktree path"},

		// Empty.
		{name: "empty", path: "", label: "worktree path", wantErr: "worktree path is empty"},

		// Invalid characters — label appears in error.
		{name: "uppercase", path: "Feature", label: "workspace name", wantErr: "workspace name: invalid character"},
		{name: "shell_metachar", path: "foo;bar", label: "worktree path", wantErr: "worktree path: invalid character"},

		// Structural.
		{name: "leading_slash", path: "/foo", label: "worktree path", wantErr: "must not start with /"},
		{name: "trailing_slash", path: "foo/", label: "worktree path", wantErr: "must not end with /"},
		{name: "double_slash", path: "foo//bar", label: "worktree path", wantErr: "empty segment"},
		{name: "path_traversal", path: "foo/../bar", label: "worktree path", wantErr: "path traversal"},
		{name: "hidden_segment", path: "foo/.hidden", label: "worktree path", wantErr: "starts with '.'"},

		// No length restriction (unlike ValidateLocalpart).
		{name: "very_long", path: strings.Repeat("a/", 100) + "end", label: "worktree path"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateRelativePath(test.path, test.label)
			if test.wantErr == "" {
				if err != nil {
					t.Errorf("ValidateRelativePath(%q, %q) = %v, want nil", test.path, test.label, err)
				}
			} else {
				if err == nil {
					t.Errorf("ValidateRelativePath(%q, %q) = nil, want error containing %q", test.path, test.label, test.wantErr)
				} else if !strings.Contains(err.Error(), test.wantErr) {
					t.Errorf("ValidateRelativePath(%q, %q) = %v, want error containing %q", test.path, test.label, err, test.wantErr)
				}
			}
		})
	}
}

func TestLocalpartFromMatrixID(t *testing.T) {
	tests := []struct {
		name     string
		matrixID string
		want     string
		wantErr  string
	}{
		{
			name:     "simple",
			matrixID: "@alice:bureau.local",
			want:     "alice",
		},
		{
			name:     "hierarchical",
			matrixID: "@iree/amdgpu/pm:bureau.local",
			want:     "iree/amdgpu/pm",
		},
		{
			name:     "server_with_port",
			matrixID: "@alice:example.org:8448",
			want:     "alice",
			// First colon is the localpart/server boundary — localparts cannot
			// contain colons, but server names can include ports.
		},
		{
			name:     "missing_at",
			matrixID: "alice:bureau.local",
			wantErr:  "must start with @",
		},
		{
			name:     "empty",
			matrixID: "",
			wantErr:  "must start with @",
		},
		{
			name:     "at_only",
			matrixID: "@",
			wantErr:  "must start with @",
		},
		{
			name:     "no_colon",
			matrixID: "@alice",
			wantErr:  "missing :server",
		},
		{
			name:     "colon_at_start",
			matrixID: "@:bureau.local",
			wantErr:  "empty localpart",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := LocalpartFromMatrixID(test.matrixID)
			if test.wantErr == "" {
				if err != nil {
					t.Errorf("LocalpartFromMatrixID(%q) error = %v, want nil", test.matrixID, err)
				} else if got != test.want {
					t.Errorf("LocalpartFromMatrixID(%q) = %q, want %q", test.matrixID, got, test.want)
				}
			} else {
				if err == nil {
					t.Errorf("LocalpartFromMatrixID(%q) = %q, want error containing %q", test.matrixID, got, test.wantErr)
				} else if !strings.Contains(err.Error(), test.wantErr) {
					t.Errorf("LocalpartFromMatrixID(%q) error = %v, want error containing %q", test.matrixID, err, test.wantErr)
				}
			}
		})
	}
}

func TestRoomAliasLocalpart(t *testing.T) {
	tests := []struct {
		name      string
		fullAlias string
		want      string
	}{
		{
			name:      "standard alias",
			fullAlias: "#bureau/machine:bureau.local",
			want:      "bureau/machine",
		},
		{
			name:      "nested alias",
			fullAlias: "#bureau/fleet/prod/machine/workstation:bureau.local",
			want:      "bureau/fleet/prod/machine/workstation",
		},
		{
			name:      "different server",
			fullAlias: "#test:example.org",
			want:      "test",
		},
		{
			name:      "no hash prefix",
			fullAlias: "bureau/machine:bureau.local",
			want:      "bureau/machine",
		},
		{
			name:      "no server suffix",
			fullAlias: "#bureau/machine",
			want:      "bureau/machine",
		},
		{
			name:      "bare name no prefix no colon",
			fullAlias: "bureau/fleet/staging/service/stt",
			want:      "bureau/fleet/staging/service/stt",
		},
		{
			name:      "server with port",
			fullAlias: "#agents:example.org:8448",
			want:      "agents",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := RoomAliasLocalpart(test.fullAlias)
			if got != test.want {
				t.Errorf("RoomAliasLocalpart(%q) = %q, want %q", test.fullAlias, got, test.want)
			}
		})
	}
}

func TestAdminSocketPathLength(t *testing.T) {
	// Verify that the max localpart length keeps admin socket paths within
	// the 108-byte sun_path limit for unix domain sockets.
	maxPath := DefaultRunDir + "/" + strings.Repeat("a", MaxLocalpartLength) + AdminSocketSuffix
	if length := len(maxPath); length > 108 {
		t.Errorf("max admin socket path is %d bytes (%q), exceeds 108-byte sun_path limit", length, maxPath)
	}
}

func TestProxyServiceName(t *testing.T) {
	tests := []struct {
		name      string
		localpart string
		want      string
	}{
		{
			name:      "hierarchical service",
			localpart: "service/stt/whisper",
			want:      "service-stt-whisper",
		},
		{
			name:      "flat name unchanged",
			localpart: "stt",
			want:      "stt",
		},
		{
			name:      "two segments",
			localpart: "machine/workstation",
			want:      "machine-workstation",
		},
		{
			name:      "already has hyphens",
			localpart: "service/stt-v2/whisper",
			want:      "service-stt-v2-whisper",
		},
		{
			name:      "deep hierarchy",
			localpart: "a/b/c/d/e",
			want:      "a-b-c-d-e",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ProxyServiceName(test.localpart)
			if got != test.want {
				t.Errorf("ProxyServiceName(%q) = %q, want %q", test.localpart, got, test.want)
			}
		})
	}
}

func TestSpaceNotAllowed(t *testing.T) {
	// Regression test: space was accidentally allowed by a buggy init() loop.
	err := ValidateLocalpart("alice bob")
	if err == nil {
		t.Error("ValidateLocalpart(\"alice bob\") = nil, want error (space should not be allowed)")
	}
}

func TestDerivedSocketPaths(t *testing.T) {
	runDir := "/tmp/test"
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"launcher", LauncherSocketPath(runDir), "/tmp/test/launcher.sock"},
		{"tmux", TmuxSocketPath(runDir), "/tmp/test/tmux.sock"},
		{"relay", RelaySocketPath(runDir), "/tmp/test/relay.sock"},
		{"observe", ObserveSocketPath(runDir), "/tmp/test/observe.sock"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.got != test.want {
				t.Errorf("got %q, want %q", test.got, test.want)
			}
		})
	}
}

func TestValidateRunDir(t *testing.T) {
	tests := []struct {
		name    string
		runDir  string
		wantErr string
	}{
		{
			name:   "default is valid",
			runDir: DefaultRunDir,
		},
		{
			name:   "short temp path",
			runDir: "/tmp/test",
		},
		{
			name:    "too long for any localpart",
			runDir:  strings.Repeat("a", 100),
			wantErr: "too long for any localpart",
		},
		{
			name:   "long but still usable run dir",
			runDir: "/tmp/bureau-testing",
			// 19 bytes — overhead is 12, so available = 107-19-12 = 76.
			// Below MaxLocalpartLength (84) but still usable, so no error.
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateRunDir(test.runDir)
			if test.wantErr == "" {
				if err != nil {
					t.Errorf("ValidateRunDir(%q) = %v, want nil", test.runDir, err)
				}
			} else {
				if err == nil {
					t.Errorf("ValidateRunDir(%q) = nil, want error containing %q", test.runDir, test.wantErr)
				} else if !strings.Contains(err.Error(), test.wantErr) {
					t.Errorf("ValidateRunDir(%q) = %v, want error containing %q", test.runDir, err, test.wantErr)
				}
			}
		})
	}
}

func TestMaxLocalpartAvailable(t *testing.T) {
	tests := []struct {
		name   string
		runDir string
		want   int
	}{
		{
			name:   "default run dir",
			runDir: DefaultRunDir, // "/run/bureau" = 11 bytes
			want:   84,            // 107 - 11 - 12 = 84
		},
		{
			name:   "longer run dir reduces budget",
			runDir: "/tmp/bureau-testing", // 19 bytes
			want:   76,                    // 107 - 19 - 12 = 76
		},
		{
			name:   "impossibly long run dir returns zero",
			runDir: strings.Repeat("a", 100),
			want:   0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := MaxLocalpartAvailable(test.runDir)
			if got != test.want {
				t.Errorf("MaxLocalpartAvailable(%q) = %d, want %d", test.runDir, got, test.want)
			}
		})
	}
}
