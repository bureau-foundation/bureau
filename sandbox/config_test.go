// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"testing"
)

func TestMergeProfiles(t *testing.T) {
	t.Parallel()

	parent := &Profile{
		Name:        "parent",
		Description: "Parent profile",
		Filesystem: []Mount{
			{Source: "/usr", Dest: "/usr", Mode: "ro"},
			{Source: "/tmp", Dest: "/tmp", Mode: "rw"},
		},
		Namespaces: NamespaceConfig{
			PID: true,
			Net: true,
		},
		Environment: map[string]string{
			"PATH": "/usr/bin",
			"HOME": "/home",
		},
		Resources: ResourceConfig{
			TasksMax:  100,
			MemoryMax: "4G",
		},
		Security: SecurityConfig{
			NewSession: true,
		},
		CreateDirs: []string{"/tmp", "/var/tmp"},
	}

	child := &Profile{
		Name:        "child",
		Description: "Child profile",
		Filesystem: []Mount{
			{Source: "/workspace", Dest: "/tmp", Mode: "ro"},
		},
		Environment: map[string]string{
			"HOME":  "/workspace",
			"EXTRA": "value",
		},
		Resources: ResourceConfig{
			MemoryMax: "2G",
		},
	}

	merged := MergeProfiles(parent, child)

	// Name comes from child.
	if merged.Name != "child" {
		t.Errorf("expected name 'child', got %q", merged.Name)
	}

	// Inherit is cleared after merge.
	if merged.Inherit != "" {
		t.Errorf("expected empty inherit after merge, got %q", merged.Inherit)
	}

	// Description comes from child.
	if merged.Description != "Child profile" {
		t.Errorf("expected 'Child profile', got %q", merged.Description)
	}

	// Namespaces inherited from parent (child has zero-value NamespaceConfig).
	if !merged.Namespaces.PID || !merged.Namespaces.Net {
		t.Error("expected inherited namespaces")
	}

	// Filesystem: parent's /usr kept, child's /tmp overrides parent's /tmp.
	foundUsr := false
	foundTmp := false
	for _, m := range merged.Filesystem {
		if m.Dest == "/usr" && m.Source == "/usr" && m.Mode == "ro" {
			foundUsr = true
		}
		if m.Dest == "/tmp" && m.Source == "/workspace" && m.Mode == "ro" {
			foundTmp = true
		}
	}
	if !foundUsr {
		t.Error("expected inherited /usr mount")
	}
	if !foundTmp {
		t.Error("expected child's /tmp mount to override parent's")
	}

	// Environment: merged (parent PATH kept, child HOME overrides, child EXTRA added).
	if merged.Environment["PATH"] != "/usr/bin" {
		t.Errorf("expected inherited PATH, got %q", merged.Environment["PATH"])
	}
	if merged.Environment["HOME"] != "/workspace" {
		t.Errorf("expected overridden HOME=/workspace, got %q", merged.Environment["HOME"])
	}
	if merged.Environment["EXTRA"] != "value" {
		t.Errorf("expected EXTRA=value, got %q", merged.Environment["EXTRA"])
	}

	// Resources: non-zero child fields override, zero-value fields inherit.
	if merged.Resources.TasksMax != 100 {
		t.Errorf("expected inherited tasks_max=100, got %d", merged.Resources.TasksMax)
	}
	if merged.Resources.MemoryMax != "2G" {
		t.Errorf("expected overridden memory_max=2G, got %q", merged.Resources.MemoryMax)
	}

	// Security inherited from parent.
	if !merged.Security.NewSession {
		t.Error("expected inherited new_session")
	}

	// CreateDirs inherited from parent (child has none to add).
	if len(merged.CreateDirs) != 2 {
		t.Errorf("expected 2 create_dirs, got %d", len(merged.CreateDirs))
	}

	// Verify parent was not mutated.
	if parent.Name != "parent" {
		t.Error("parent was mutated by merge")
	}
	if parent.Environment["HOME"] != "/home" {
		t.Error("parent environment was mutated by merge")
	}
}

func TestMergeProfilesCreateDirsDeduplicate(t *testing.T) {
	t.Parallel()

	parent := &Profile{
		Name:       "parent",
		CreateDirs: []string{"/tmp", "/var/tmp"},
	}

	child := &Profile{
		Name:       "child",
		CreateDirs: []string{"/tmp", "/run/bureau"},
	}

	merged := MergeProfiles(parent, child)

	dirSet := make(map[string]bool)
	for _, d := range merged.CreateDirs {
		if dirSet[d] {
			t.Errorf("duplicate create_dir: %s", d)
		}
		dirSet[d] = true
	}

	if !dirSet["/tmp"] || !dirSet["/var/tmp"] || !dirSet["/run/bureau"] {
		t.Errorf("expected all three dirs, got %v", merged.CreateDirs)
	}
}

func TestVariableExpansion(t *testing.T) {
	t.Parallel()

	vars := Variables{
		"WORKTREE":     "/home/user/work",
		"PROXY_SOCKET": "/run/proxy.sock",
	}

	tests := []struct {
		input    string
		expected string
	}{
		{"${WORKTREE}", "/home/user/work"},
		{"${PROXY_SOCKET}", "/run/proxy.sock"},
		{"${WORKTREE}/bin", "/home/user/work/bin"},
		{"no vars here", "no vars here"},
		{"${UNKNOWN}", "${UNKNOWN}"},
		{"${WORKTREE}:${PROXY_SOCKET}", "/home/user/work:/run/proxy.sock"},
	}

	for _, tt := range tests {
		result := vars.Expand(tt.input)
		if result != tt.expected {
			t.Errorf("Expand(%q) = %q, expected %q", tt.input, result, tt.expected)
		}
	}
}

func TestExpandProfile(t *testing.T) {
	t.Parallel()

	vars := Variables{
		"WORKTREE":     "/home/user/work",
		"PROXY_SOCKET": "/run/proxy.sock",
		"TERM":         "xterm",
	}

	profile := &Profile{
		Name: "test",
		Filesystem: []Mount{
			{Source: "${WORKTREE}", Dest: "/workspace", Mode: "rw"},
			{Source: "${PROXY_SOCKET}", Dest: "/run/bureau/proxy.sock", Mode: "rw"},
		},
		Environment: map[string]string{
			"TERM":    "${TERM}",
			"WORKDIR": "${WORKTREE}",
		},
		CreateDirs: []string{"${WORKTREE}/.cache"},
	}

	expanded := vars.ExpandProfile(profile)

	// Check filesystem.
	if expanded.Filesystem[0].Source != "/home/user/work" {
		t.Errorf("expected expanded worktree, got %q", expanded.Filesystem[0].Source)
	}
	if expanded.Filesystem[1].Source != "/run/proxy.sock" {
		t.Errorf("expected expanded proxy socket, got %q", expanded.Filesystem[1].Source)
	}

	// Check environment.
	if expanded.Environment["TERM"] != "xterm" {
		t.Errorf("expected TERM=xterm, got %q", expanded.Environment["TERM"])
	}
	if expanded.Environment["WORKDIR"] != "/home/user/work" {
		t.Errorf("expected WORKDIR=/home/user/work, got %q", expanded.Environment["WORKDIR"])
	}

	// Check create_dirs.
	if expanded.CreateDirs[0] != "/home/user/work/.cache" {
		t.Errorf("expected expanded create_dirs, got %q", expanded.CreateDirs[0])
	}

	// Original profile should be unchanged.
	if profile.Filesystem[0].Source != "${WORKTREE}" {
		t.Error("original profile was modified")
	}
}

func TestProfileValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		profile   Profile
		expectErr bool
	}{
		{
			name: "valid profile",
			profile: Profile{
				Name: "test",
				Filesystem: []Mount{
					{Source: "/tmp", Dest: "/test", Mode: "ro"},
				},
			},
			expectErr: false,
		},
		{
			name: "missing dest",
			profile: Profile{
				Name: "test",
				Filesystem: []Mount{
					{Source: "/tmp", Mode: "ro"},
				},
			},
			expectErr: true,
		},
		{
			name: "missing source for bind",
			profile: Profile{
				Name: "test",
				Filesystem: []Mount{
					{Dest: "/test", Mode: "ro"},
				},
			},
			expectErr: true,
		},
		{
			name: "tmpfs without source is ok",
			profile: Profile{
				Name: "test",
				Filesystem: []Mount{
					{Dest: "/tmp", Type: "tmpfs"},
				},
			},
			expectErr: false,
		},
		{
			name: "invalid mode",
			profile: Profile{
				Name: "test",
				Filesystem: []Mount{
					{Source: "/tmp", Dest: "/test", Mode: "invalid"},
				},
			},
			expectErr: true,
		},
		{
			name: "negative tasks_max",
			profile: Profile{
				Name: "test",
				Resources: ResourceConfig{
					TasksMax: -1,
				},
			},
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.profile.Validate()
			if tt.expectErr && err == nil {
				t.Error("expected validation error, got nil")
			}
			if !tt.expectErr && err != nil {
				t.Errorf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestResourceConfigHasLimits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		config   ResourceConfig
		expected bool
	}{
		{
			name:     "no limits",
			config:   ResourceConfig{},
			expected: false,
		},
		{
			name:     "tasks_max only",
			config:   ResourceConfig{TasksMax: 100},
			expected: true,
		},
		{
			name:     "memory_max only",
			config:   ResourceConfig{MemoryMax: "4G"},
			expected: true,
		},
		{
			name:     "cpu_quota only",
			config:   ResourceConfig{CPUQuota: "200%"},
			expected: true,
		},
		{
			name: "all limits",
			config: ResourceConfig{
				TasksMax:  100,
				MemoryMax: "4G",
				CPUQuota:  "200%",
			},
			expected: true,
		},
		{
			name:     "tasks_max 0 means unlimited",
			config:   ResourceConfig{TasksMax: 0},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := tt.config.HasLimits()
			if result != tt.expected {
				t.Errorf("HasLimits() = %v, expected %v", result, tt.expected)
			}
		})
	}
}

func TestDefaultVariables(t *testing.T) {
	t.Parallel()

	vars := DefaultVariables()

	// BUREAU_ROOT should be set.
	if vars["BUREAU_ROOT"] == "" {
		t.Error("BUREAU_ROOT should be set")
	}

	// PROXY_SOCKET should default to /run/bureau/proxy.sock.
	if vars["PROXY_SOCKET"] != "/run/bureau/proxy.sock" {
		t.Errorf("expected PROXY_SOCKET=/run/bureau/proxy.sock, got %q", vars["PROXY_SOCKET"])
	}
}
