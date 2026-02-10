// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/sandbox"
)

func TestSpecToProfile_Filesystem(t *testing.T) {
	t.Parallel()

	spec := &schema.SandboxSpec{
		Command: []string{"/bin/bash"},
		Filesystem: []schema.TemplateMount{
			{Source: "/workspace", Dest: "/workspace", Mode: "rw"},
			{Source: "/usr", Dest: "/usr", Mode: "ro"},
			{Dest: "/tmp", Type: "tmpfs"},
		},
	}

	profile := specToProfile(spec, "/run/bureau/principal/test.sock")

	// Should have the 3 spec mounts + 1 proxy socket mount.
	if len(profile.Filesystem) != 4 {
		t.Fatalf("expected 4 mounts, got %d", len(profile.Filesystem))
	}

	// Check the workspace mount.
	workspace := profile.Filesystem[0]
	if workspace.Source != "/workspace" || workspace.Dest != "/workspace" || workspace.Mode != "rw" {
		t.Errorf("workspace mount = %+v", workspace)
	}

	// Check the usr mount.
	usr := profile.Filesystem[1]
	if usr.Source != "/usr" || usr.Dest != "/usr" || usr.Mode != "ro" {
		t.Errorf("usr mount = %+v", usr)
	}

	// Check the tmpfs mount.
	tmpfs := profile.Filesystem[2]
	if tmpfs.Dest != "/tmp" || tmpfs.Type != "tmpfs" {
		t.Errorf("tmpfs mount = %+v", tmpfs)
	}

	// Check the proxy socket mount (always appended last).
	proxySock := profile.Filesystem[3]
	if proxySock.Source != "/run/bureau/principal/test.sock" {
		t.Errorf("proxy socket source = %q, want /run/bureau/principal/test.sock", proxySock.Source)
	}
	if proxySock.Dest != "/run/bureau/proxy.sock" {
		t.Errorf("proxy socket dest = %q, want /run/bureau/proxy.sock", proxySock.Dest)
	}
	if proxySock.Mode != sandbox.MountModeRW {
		t.Errorf("proxy socket mode = %q, want rw", proxySock.Mode)
	}
}

func TestSpecToProfile_Namespaces(t *testing.T) {
	t.Parallel()

	spec := &schema.SandboxSpec{
		Command: []string{"/bin/bash"},
		Namespaces: &schema.TemplateNamespaces{
			PID: true,
			Net: true,
			IPC: true,
			UTS: true,
		},
	}

	profile := specToProfile(spec, "/tmp/proxy.sock")

	if !profile.Namespaces.PID {
		t.Error("expected PID namespace")
	}
	if !profile.Namespaces.Net {
		t.Error("expected Net namespace")
	}
	if !profile.Namespaces.IPC {
		t.Error("expected IPC namespace")
	}
	if !profile.Namespaces.UTS {
		t.Error("expected UTS namespace")
	}
}

func TestSpecToProfile_NilNamespaces(t *testing.T) {
	t.Parallel()

	spec := &schema.SandboxSpec{
		Command: []string{"/bin/bash"},
	}

	profile := specToProfile(spec, "/tmp/proxy.sock")

	if profile.Namespaces.PID || profile.Namespaces.Net || profile.Namespaces.IPC || profile.Namespaces.UTS {
		t.Errorf("expected all namespaces false when spec.Namespaces is nil, got %+v", profile.Namespaces)
	}
}

func TestSpecToProfile_Resources(t *testing.T) {
	t.Parallel()

	spec := &schema.SandboxSpec{
		Command: []string{"/bin/bash"},
		Resources: &schema.TemplateResources{
			CPUShares:     1024,
			MemoryLimitMB: 2048,
			PidsLimit:     512,
		},
	}

	profile := specToProfile(spec, "/tmp/proxy.sock")

	if profile.Resources.MemoryMax != "2048M" {
		t.Errorf("MemoryMax = %q, want %q", profile.Resources.MemoryMax, "2048M")
	}
	if profile.Resources.TasksMax != 512 {
		t.Errorf("TasksMax = %d, want 512", profile.Resources.TasksMax)
	}
	// CPUShares 1024 → CPUWeight 100 (1024 * 100 / 1024 = 100).
	if profile.Resources.CPUWeight != 100 {
		t.Errorf("CPUWeight = %d, want 100 (from CPUShares 1024)", profile.Resources.CPUWeight)
	}
}

func TestSpecToProfile_CPUSharesScaling(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		cpuShares  int
		wantWeight int
	}{
		{"default shares", 1024, 100},
		{"double shares", 2048, 200},
		{"half shares", 512, 50},
		{"minimum clamp", 1, 1},
		{"zero means no limit", 0, 0},
		{"high shares", 102400, 10000},
		// Verify clamping at upper bound.
		{"extreme shares", 204800, 10000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			spec := &schema.SandboxSpec{
				Command:   []string{"/bin/bash"},
				Resources: &schema.TemplateResources{CPUShares: tt.cpuShares},
			}

			profile := specToProfile(spec, "/tmp/proxy.sock")

			if profile.Resources.CPUWeight != tt.wantWeight {
				t.Errorf("CPUWeight = %d, want %d (from CPUShares %d)",
					profile.Resources.CPUWeight, tt.wantWeight, tt.cpuShares)
			}
		})
	}
}

func TestSpecToProfile_Security(t *testing.T) {
	t.Parallel()

	spec := &schema.SandboxSpec{
		Command: []string{"/bin/bash"},
		Security: &schema.TemplateSecurity{
			NewSession:    true,
			DieWithParent: true,
			NoNewPrivs:    true,
		},
	}

	profile := specToProfile(spec, "/tmp/proxy.sock")

	if !profile.Security.NewSession {
		t.Error("expected NewSession")
	}
	if !profile.Security.DieWithParent {
		t.Error("expected DieWithParent")
	}
	if !profile.Security.NoNewPrivs {
		t.Error("expected NoNewPrivs")
	}
}

func TestSpecToProfile_EnvironmentVariables(t *testing.T) {
	t.Parallel()

	spec := &schema.SandboxSpec{
		Command: []string{"/bin/bash"},
		EnvironmentVariables: map[string]string{
			"HOME": "/workspace",
			"TERM": "xterm-256color",
		},
	}

	profile := specToProfile(spec, "/tmp/proxy.sock")

	if profile.Environment["HOME"] != "/workspace" {
		t.Errorf("HOME = %q, want /workspace", profile.Environment["HOME"])
	}
	if profile.Environment["TERM"] != "xterm-256color" {
		t.Errorf("TERM = %q, want xterm-256color", profile.Environment["TERM"])
	}
}

func TestSpecToProfile_EnvironmentPath(t *testing.T) {
	t.Parallel()

	spec := &schema.SandboxSpec{
		Command:         []string{"/bin/bash"},
		EnvironmentPath: "/nix/store/abc123-bureau-agent-env",
	}

	profile := specToProfile(spec, "/tmp/proxy.sock")

	// Should add /nix/store ro bind mount.
	hasNixStore := false
	for _, mount := range profile.Filesystem {
		if mount.Dest == "/nix/store" && mount.Source == "/nix/store" && mount.Mode == sandbox.MountModeRO {
			hasNixStore = true
			break
		}
	}
	if !hasNixStore {
		t.Error("expected /nix/store ro bind mount for EnvironmentPath")
	}

	// Should prepend environment bin to PATH.
	expectedPathPrefix := "/nix/store/abc123-bureau-agent-env/bin"
	path := profile.Environment["PATH"]
	if !strings.HasPrefix(path, expectedPathPrefix) {
		t.Errorf("PATH = %q, want prefix %q", path, expectedPathPrefix)
	}
}

func TestSpecToProfile_EnvironmentPathWithExistingPath(t *testing.T) {
	t.Parallel()

	spec := &schema.SandboxSpec{
		Command:         []string{"/bin/bash"},
		EnvironmentPath: "/nix/store/abc123-env",
		EnvironmentVariables: map[string]string{
			"PATH": "/usr/bin:/bin",
		},
	}

	profile := specToProfile(spec, "/tmp/proxy.sock")

	// Should prepend to existing PATH.
	expected := "/nix/store/abc123-env/bin:/usr/bin:/bin"
	if profile.Environment["PATH"] != expected {
		t.Errorf("PATH = %q, want %q", profile.Environment["PATH"], expected)
	}
}

func TestSpecToProfile_EnvironmentPathSkipsDuplicateNixMount(t *testing.T) {
	t.Parallel()

	spec := &schema.SandboxSpec{
		Command:         []string{"/bin/bash"},
		EnvironmentPath: "/nix/store/abc123-env",
		Filesystem: []schema.TemplateMount{
			{Source: "/nix/store", Dest: "/nix/store", Mode: "ro"},
		},
	}

	profile := specToProfile(spec, "/tmp/proxy.sock")

	// Count /nix/store mounts — should be exactly 1 (not duplicated).
	count := 0
	for _, mount := range profile.Filesystem {
		if mount.Dest == "/nix/store" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 /nix/store mount, got %d", count)
	}
}

func TestSpecToProfile_CreateDirs(t *testing.T) {
	t.Parallel()

	spec := &schema.SandboxSpec{
		Command:    []string{"/bin/bash"},
		CreateDirs: []string{"/workspace/.cache", "/workspace/.config"},
	}

	profile := specToProfile(spec, "/tmp/proxy.sock")

	if len(profile.CreateDirs) != 2 {
		t.Fatalf("expected 2 CreateDirs, got %d", len(profile.CreateDirs))
	}
	if profile.CreateDirs[0] != "/workspace/.cache" {
		t.Errorf("CreateDirs[0] = %q, want /workspace/.cache", profile.CreateDirs[0])
	}
	if profile.CreateDirs[1] != "/workspace/.config" {
		t.Errorf("CreateDirs[1] = %q, want /workspace/.config", profile.CreateDirs[1])
	}
}

func TestSpecToProfile_MountOptionalField(t *testing.T) {
	t.Parallel()

	spec := &schema.SandboxSpec{
		Command: []string{"/bin/bash"},
		Filesystem: []schema.TemplateMount{
			{Source: "/opt/cuda", Dest: "/opt/cuda", Mode: "ro", Optional: true},
		},
	}

	profile := specToProfile(spec, "/tmp/proxy.sock")

	// The optional flag should be preserved through conversion.
	// First mount is the optional one, second is the proxy socket.
	if len(profile.Filesystem) != 2 {
		t.Fatalf("expected 2 mounts, got %d", len(profile.Filesystem))
	}
	if !profile.Filesystem[0].Optional {
		t.Error("expected Optional flag to be preserved")
	}
}

func TestWritePayloadFile(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()

	payload := map[string]any{
		"model":      "llama-3.1",
		"batch_size": 32,
		"tags":       []string{"gpu", "inference"},
	}

	path, err := writePayloadFile(directory, payload)
	if err != nil {
		t.Fatalf("writePayloadFile: %v", err)
	}

	// Check the path.
	expectedPath := filepath.Join(directory, "payload.json")
	if path != expectedPath {
		t.Errorf("path = %q, want %q", path, expectedPath)
	}

	// Read and verify the content.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading payload file: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshaling payload: %v", err)
	}

	if decoded["model"] != "llama-3.1" {
		t.Errorf("model = %v, want llama-3.1", decoded["model"])
	}
	// JSON numbers unmarshal as float64.
	if decoded["batch_size"] != float64(32) {
		t.Errorf("batch_size = %v, want 32", decoded["batch_size"])
	}
}

func TestWriteSandboxScript(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()

	path, err := writeSandboxScript(directory, "/usr/bin/bwrap", []string{
		"--unshare-pid",
		"--bind", "/workspace", "/workspace",
		"--", "/bin/bash",
	})
	if err != nil {
		t.Fatalf("writeSandboxScript: %v", err)
	}

	// Check the path.
	expectedPath := filepath.Join(directory, "sandbox.sh")
	if path != expectedPath {
		t.Errorf("path = %q, want %q", path, expectedPath)
	}

	// Read and verify the content.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading script file: %v", err)
	}

	content := string(data)

	// Should start with shebang.
	if !strings.HasPrefix(content, "#!/bin/sh\n") {
		t.Errorf("expected shebang, got %q", content[:min(len(content), 20)])
	}

	// Should contain exec with the bwrap command.
	if !strings.Contains(content, "exec /usr/bin/bwrap") {
		t.Errorf("expected 'exec /usr/bin/bwrap' in script, got:\n%s", content)
	}

	// Should contain the arguments.
	if !strings.Contains(content, "--unshare-pid") {
		t.Error("missing --unshare-pid in script")
	}
	if !strings.Contains(content, "--bind /workspace /workspace") {
		t.Error("missing bind arguments in script")
	}

	// Should be executable.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode()&0111 == 0 {
		t.Errorf("script not executable: mode %v", info.Mode())
	}
}

func TestShellQuote(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input    string
		expected string
	}{
		// Safe strings pass through unchanged.
		{"/usr/bin/bwrap", "/usr/bin/bwrap"},
		{"--unshare-pid", "--unshare-pid"},
		{"/workspace", "/workspace"},
		{"HOME=/workspace", "HOME=/workspace"},

		// Strings with spaces get single-quoted.
		{"hello world", "'hello world'"},

		// Strings with special shell characters.
		{"$(evil)", "'$(evil)'"},
		{"foo;bar", "'foo;bar'"},
		{"a&b", "'a&b'"},
		{"x|y", "'x|y'"},

		// Single quotes get escaped.
		{"it's", "'it'\\''s'"},

		// Empty string gets quoted.
		{"", "''"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			result := shellQuote(tt.input)
			if result != tt.expected {
				t.Errorf("shellQuote(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestIsShellSafe(t *testing.T) {
	t.Parallel()

	safe := []rune{'a', 'z', 'A', 'Z', '0', '9', '-', '_', '.', '/', ':', '=', '+', ',', '@'}
	for _, char := range safe {
		if !isShellSafe(char) {
			t.Errorf("isShellSafe(%q) = false, want true", char)
		}
	}

	unsafe := []rune{' ', '\t', '$', '`', '"', '\'', '(', ')', '{', '}', '[', ']', ';', '&', '|', '<', '>', '\\', '!', '~', '#', '*', '?'}
	for _, char := range unsafe {
		if isShellSafe(char) {
			t.Errorf("isShellSafe(%q) = true, want false", char)
		}
	}
}
