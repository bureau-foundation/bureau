// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"testing"
)

func TestParseProfilesConfig(t *testing.T) {
	yaml := `
profiles:
  test:
    description: "Test profile"
    filesystem:
      - source: /tmp
        dest: /test
        mode: ro
    namespaces:
      pid: true
      net: true
    environment:
      FOO: bar
    resources:
      tasks_max: 100
      memory_max: "4G"
    security:
      new_session: true
`
	config, err := ParseProfilesConfig([]byte(yaml))
	if err != nil {
		t.Fatalf("ParseProfilesConfig failed: %v", err)
	}

	profile, ok := config.Profiles["test"]
	if !ok {
		t.Fatal("profile 'test' not found")
	}

	if profile.Name != "test" {
		t.Errorf("expected name 'test', got %q", profile.Name)
	}

	if profile.Description != "Test profile" {
		t.Errorf("expected description 'Test profile', got %q", profile.Description)
	}

	if len(profile.Filesystem) != 1 {
		t.Errorf("expected 1 mount, got %d", len(profile.Filesystem))
	} else {
		m := profile.Filesystem[0]
		if m.Source != "/tmp" || m.Dest != "/test" || m.Mode != "ro" {
			t.Errorf("unexpected mount: %+v", m)
		}
	}

	if !profile.Namespaces.PID {
		t.Error("expected PID namespace")
	}
	if !profile.Namespaces.Net {
		t.Error("expected Net namespace")
	}

	if profile.Environment["FOO"] != "bar" {
		t.Errorf("expected FOO=bar, got %q", profile.Environment["FOO"])
	}

	if profile.Resources.TasksMax != 100 {
		t.Errorf("expected tasks_max=100, got %d", profile.Resources.TasksMax)
	}

	if profile.Resources.MemoryMax != "4G" {
		t.Errorf("expected memory_max=4G, got %q", profile.Resources.MemoryMax)
	}

	if !profile.Security.NewSession {
		t.Error("expected new_session=true")
	}
}

func TestProfileInheritance(t *testing.T) {
	yaml := `
profiles:
  parent:
    description: "Parent profile"
    filesystem:
      - source: /usr
        dest: /usr
        mode: ro
      - source: /tmp
        dest: /tmp
        mode: rw
    namespaces:
      pid: true
      net: true
    environment:
      PATH: /usr/bin
      HOME: /home
    resources:
      tasks_max: 100
      memory_max: "4G"
    security:
      new_session: true

  child:
    description: "Child profile"
    inherit: parent
    filesystem:
      - source: /workspace
        dest: /tmp
        mode: ro
    environment:
      HOME: /workspace
      EXTRA: value
    resources:
      memory_max: "2G"
`
	config, err := ParseProfilesConfig([]byte(yaml))
	if err != nil {
		t.Fatalf("ParseProfilesConfig failed: %v", err)
	}

	child, err := config.ResolveProfile("child")
	if err != nil {
		t.Fatalf("ResolveProfile failed: %v", err)
	}

	// Child should have its own description.
	if child.Description != "Child profile" {
		t.Errorf("expected 'Child profile', got %q", child.Description)
	}

	// Namespaces should be inherited.
	if !child.Namespaces.PID || !child.Namespaces.Net {
		t.Error("expected inherited namespaces")
	}

	// Filesystem should have parent's /usr and child's override of /tmp.
	foundUsr := false
	foundTmp := false
	for _, m := range child.Filesystem {
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
		t.Error("expected overridden /tmp mount")
	}

	// Environment should be merged.
	if child.Environment["PATH"] != "/usr/bin" {
		t.Errorf("expected inherited PATH, got %q", child.Environment["PATH"])
	}
	if child.Environment["HOME"] != "/workspace" {
		t.Errorf("expected overridden HOME=/workspace, got %q", child.Environment["HOME"])
	}
	if child.Environment["EXTRA"] != "value" {
		t.Errorf("expected EXTRA=value, got %q", child.Environment["EXTRA"])
	}

	// Resources should be overridden where specified.
	if child.Resources.TasksMax != 100 {
		t.Errorf("expected inherited tasks_max=100, got %d", child.Resources.TasksMax)
	}
	if child.Resources.MemoryMax != "2G" {
		t.Errorf("expected overridden memory_max=2G, got %q", child.Resources.MemoryMax)
	}

	// Security should be inherited.
	if !child.Security.NewSession {
		t.Error("expected inherited new_session")
	}
}

func TestVariableExpansion(t *testing.T) {
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
			result := tt.config.HasLimits()
			if result != tt.expected {
				t.Errorf("HasLimits() = %v, expected %v", result, tt.expected)
			}
		})
	}
}
