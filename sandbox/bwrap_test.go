// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"strings"
	"testing"
)

func TestBwrapBuilder(t *testing.T) {
	profile := &Profile{
		Name: "test",
		Filesystem: []Mount{
			{Source: "/workspace", Dest: "/workspace", Mode: "rw"},
			{Source: "/usr", Dest: "/usr", Mode: "ro"},
			{Dest: "/tmp", Type: "tmpfs"},
		},
		Namespaces: NamespaceConfig{
			PID: true,
			Net: true,
			IPC: true,
		},
		Security: SecurityConfig{
			NewSession:    true,
			DieWithParent: true,
		},
		Environment: map[string]string{
			"PATH": "/usr/bin",
			"HOME": "/workspace",
		},
	}

	builder := NewBwrapBuilder()
	args, err := builder.Build(&BwrapOptions{
		Profile:  profile,
		Command:  []string{"/bin/bash"},
		ClearEnv: true,
	})
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	argStr := strings.Join(args, " ")

	// Check namespaces.
	if !strings.Contains(argStr, "--unshare-pid") {
		t.Error("missing --unshare-pid")
	}
	if !strings.Contains(argStr, "--unshare-net") {
		t.Error("missing --unshare-net")
	}
	if !strings.Contains(argStr, "--unshare-ipc") {
		t.Error("missing --unshare-ipc")
	}

	// Check security.
	if !strings.Contains(argStr, "--new-session") {
		t.Error("missing --new-session")
	}
	if !strings.Contains(argStr, "--die-with-parent") {
		t.Error("missing --die-with-parent")
	}

	// Check filesystem.
	if !strings.Contains(argStr, "--bind /workspace /workspace") {
		t.Error("missing workspace bind")
	}
	if !strings.Contains(argStr, "--ro-bind /usr /usr") {
		t.Error("missing /usr ro-bind")
	}
	if !strings.Contains(argStr, "--tmpfs /tmp") {
		t.Error("missing tmpfs /tmp")
	}

	// Check environment.
	if !strings.Contains(argStr, "--clearenv") {
		t.Error("missing --clearenv")
	}
	if !strings.Contains(argStr, "--setenv HOME /workspace") {
		t.Error("missing HOME env")
	}
	if !strings.Contains(argStr, "--setenv PATH /usr/bin") {
		t.Error("missing PATH env")
	}

	// Check command separator and command.
	if !strings.Contains(argStr, "-- /bin/bash") {
		t.Error("missing command")
	}
}

func TestBwrapBuilderExtraBinds(t *testing.T) {
	profile := &Profile{
		Name: "test",
		Namespaces: NamespaceConfig{
			PID: true,
		},
	}

	builder := NewBwrapBuilder()
	args, err := builder.Build(&BwrapOptions{
		Profile: profile,
		ExtraBinds: []string{
			"/src/feature:/workspace/feature:ro",
			"/src/libs:/libs:rw",
		},
		Command: []string{"/bin/bash"},
	})
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	argStr := strings.Join(args, " ")

	if !strings.Contains(argStr, "--ro-bind /src/feature /workspace/feature") {
		t.Error("missing feature bind")
	}
	if !strings.Contains(argStr, "--bind /src/libs /libs") {
		t.Error("missing libs bind")
	}
}

func TestBwrapBuilderBazelCache(t *testing.T) {
	profile := &Profile{
		Name: "test",
		Namespaces: NamespaceConfig{
			PID: true,
		},
	}

	builder := NewBwrapBuilder()
	args, err := builder.Build(&BwrapOptions{
		Profile:    profile,
		BazelCache: "/var/cache/bazel",
		Command:    []string{"/bin/bash"},
	})
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	argStr := strings.Join(args, " ")

	if !strings.Contains(argStr, "--bind /var/cache/bazel /var/cache/bazel") {
		t.Error("missing bazel cache bind")
	}
	if !strings.Contains(argStr, "--setenv BAZEL_DISK_CACHE /var/cache/bazel") {
		t.Error("missing BAZEL_DISK_CACHE env")
	}
}

func TestBwrapBuilderValidation(t *testing.T) {
	builder := NewBwrapBuilder()

	// Missing profile.
	_, err := builder.Build(&BwrapOptions{
		Command: []string{"/bin/bash"},
	})
	if err == nil {
		t.Error("expected error for missing profile")
	}

	// Missing command.
	_, err = builder.Build(&BwrapOptions{
		Profile: &Profile{Name: "test"},
	})
	if err == nil {
		t.Error("expected error for missing command")
	}
}

func TestParseBindSpec(t *testing.T) {
	tests := []struct {
		spec       string
		wantSource string
		wantDest   string
		wantMode   string
		wantErr    bool
	}{
		{"/src:/dest", "/src", "/dest", "rw", false},
		{"/src:/dest:ro", "/src", "/dest", "ro", false},
		{"/src:/dest:rw", "/src", "/dest", "rw", false},
		{"invalid", "", "", "", true},
		{"/src:/dest:invalid", "", "", "", true},
		// Note: paths with colons are not supported in the simplified parser.
	}

	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			source, dest, mode, err := parseBindSpec(tt.spec)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if source != tt.wantSource {
				t.Errorf("source = %q, want %q", source, tt.wantSource)
			}
			if dest != tt.wantDest {
				t.Errorf("dest = %q, want %q", dest, tt.wantDest)
			}
			if mode != tt.wantMode {
				t.Errorf("mode = %q, want %q", mode, tt.wantMode)
			}
		})
	}
}

func TestSplitBindSpec(t *testing.T) {
	tests := []struct {
		input    string
		expected []string
	}{
		{"/src:/dest", []string{"/src", "/dest"}},
		{"/src:/dest:ro", []string{"/src", "/dest", "ro"}},
		{"/src:/dest:rw", []string{"/src", "/dest", "rw"}},
		// Note: paths with colons are not supported in the simplified parser.
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := splitBindSpec(tt.input)
			if len(result) != len(tt.expected) {
				t.Errorf("len = %d, want %d; got %v", len(result), len(tt.expected), result)
				return
			}
			for i, v := range result {
				if v != tt.expected[i] {
					t.Errorf("part[%d] = %q, want %q", i, v, tt.expected[i])
				}
			}
		})
	}
}

func TestPathHierarchy(t *testing.T) {
	tests := []struct {
		input    string
		expected []string
	}{
		{"/home/ben/.cache/pre-commit", []string{"/home", "/home/ben", "/home/ben/.cache", "/home/ben/.cache/pre-commit"}},
		{"/usr", []string{"/usr"}},
		{"/", nil},
		{".", nil},
		{"/a/b", []string{"/a", "/a/b"}},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := pathHierarchy(tt.input)
			if len(result) != len(tt.expected) {
				t.Errorf("pathHierarchy(%q) = %v (len %d), want %v (len %d)",
					tt.input, result, len(result), tt.expected, len(tt.expected))
				return
			}
			for i, v := range result {
				if v != tt.expected[i] {
					t.Errorf("pathHierarchy(%q)[%d] = %q, want %q", tt.input, i, v, tt.expected[i])
				}
			}
		})
	}
}
