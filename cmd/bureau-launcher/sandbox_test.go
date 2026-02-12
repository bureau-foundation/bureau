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
	exitCodePath := filepath.Join(directory, "exit-code")

	path, err := writeSandboxScript(directory, "/usr/bin/bwrap", []string{
		"--unshare-pid",
		"--bind", "/workspace", "/workspace",
		"--", "/bin/bash",
	}, exitCodePath)
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

	// Should contain the bwrap command (without exec — the shell must
	// survive to capture the exit code).
	if !strings.Contains(content, "/usr/bin/bwrap") {
		t.Errorf("expected '/usr/bin/bwrap' in script, got:\n%s", content)
	}

	// Should contain the arguments.
	if !strings.Contains(content, "--unshare-pid") {
		t.Error("missing --unshare-pid in script")
	}
	if !strings.Contains(content, "--bind /workspace /workspace") {
		t.Error("missing bind arguments in script")
	}

	// Should capture exit code.
	if !strings.Contains(content, "_exit_code=$?") {
		t.Error("missing exit code capture in script")
	}
	if !strings.Contains(content, exitCodePath) {
		t.Errorf("exit code path %q not found in script", exitCodePath)
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

func TestWorkspaceContext(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		localpart        string
		wantProject      string
		wantWorktreePath string
	}{
		{
			name:             "three-segment principal",
			localpart:        "iree/amdgpu/pm",
			wantProject:      "iree",
			wantWorktreePath: "amdgpu/pm",
		},
		{
			name:             "two-segment principal",
			localpart:        "iree/main",
			wantProject:      "iree",
			wantWorktreePath: "main",
		},
		{
			name:             "deep principal path",
			localpart:        "iree/amdgpu/inference/coordinator",
			wantProject:      "iree",
			wantWorktreePath: "amdgpu/inference/coordinator",
		},
		{
			name:             "single-segment principal",
			localpart:        "sysadmin",
			wantProject:      "",
			wantWorktreePath: "",
		},
		{
			name:             "service principal",
			localpart:        "service/stt/whisper",
			wantProject:      "service",
			wantWorktreePath: "stt/whisper",
		},
		{
			name:             "machine principal",
			localpart:        "machine/workstation",
			wantProject:      "machine",
			wantWorktreePath: "workstation",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			project, worktreePath := workspaceContext(tt.localpart)
			if project != tt.wantProject {
				t.Errorf("project = %q, want %q", project, tt.wantProject)
			}
			if worktreePath != tt.wantWorktreePath {
				t.Errorf("worktreePath = %q, want %q", worktreePath, tt.wantWorktreePath)
			}
		})
	}
}

// TestSpecToProfile_WorkspaceVariableExpansion verifies that the full pipeline
// of specToProfile + variable expansion resolves workspace template variables
// in filesystem mounts, environment values, and create_dirs.
func TestSpecToProfile_WorkspaceVariableExpansion(t *testing.T) {
	t.Parallel()

	spec := &schema.SandboxSpec{
		Command: []string{"/bin/bash"},
		Filesystem: []schema.TemplateMount{
			{
				Source: "/var/bureau/workspace/${PROJECT}/${WORKTREE_PATH}",
				Dest:   "/workspace",
				Mode:   "rw",
			},
			{
				Source: "/var/bureau/workspace/${PROJECT}/.bare",
				Dest:   "/workspace/.bare",
				Mode:   "rw",
			},
			{
				Source: "${WORKSPACE_ROOT}/${PROJECT}/.shared",
				Dest:   "/workspace/.shared",
				Mode:   "rw",
			},
			{
				Source: "${WORKSPACE_ROOT}/.cache",
				Dest:   "/workspace/.cache",
				Mode:   "rw",
			},
		},
		EnvironmentVariables: map[string]string{
			"HOME":           "/workspace",
			"PROJECT":        "${PROJECT}",
			"WORKSPACE_ROOT": "${WORKSPACE_ROOT}",
		},
		CreateDirs: []string{
			"${WORKSPACE_ROOT}/${PROJECT}/${WORKTREE_PATH}/.local",
		},
	}

	// Step 1: Convert to profile (no expansion yet).
	profile := specToProfile(spec, "/run/bureau/principal/iree/amdgpu/pm.sock")

	// Step 2: Build variables and expand (this mirrors buildSandboxCommand).
	vars := sandbox.Variables{
		"WORKSPACE_ROOT": "/var/bureau/workspace",
		"CACHE_ROOT":     "/var/bureau/cache",
		"PROXY_SOCKET":   "/run/bureau/principal/iree/amdgpu/pm.sock",
		"TERM":           "xterm-256color",
	}
	project, worktreePath := workspaceContext("iree/amdgpu/pm")
	if project != "" {
		vars["PROJECT"] = project
		vars["WORKTREE_PATH"] = worktreePath
	}
	expanded := vars.ExpandProfile(profile)

	// Verify filesystem mounts expanded correctly.
	// First mount: worktree.
	if expanded.Filesystem[0].Source != "/var/bureau/workspace/iree/amdgpu/pm" {
		t.Errorf("worktree mount source = %q, want /var/bureau/workspace/iree/amdgpu/pm",
			expanded.Filesystem[0].Source)
	}
	// Second mount: bare repo.
	if expanded.Filesystem[1].Source != "/var/bureau/workspace/iree/.bare" {
		t.Errorf("bare mount source = %q, want /var/bureau/workspace/iree/.bare",
			expanded.Filesystem[1].Source)
	}
	// Third mount: shared state (using WORKSPACE_ROOT variable).
	if expanded.Filesystem[2].Source != "/var/bureau/workspace/iree/.shared" {
		t.Errorf("shared mount source = %q, want /var/bureau/workspace/iree/.shared",
			expanded.Filesystem[2].Source)
	}
	// Fourth mount: cross-project cache.
	if expanded.Filesystem[3].Source != "/var/bureau/workspace/.cache" {
		t.Errorf("cache mount source = %q, want /var/bureau/workspace/.cache",
			expanded.Filesystem[3].Source)
	}

	// Verify environment variables expanded.
	if expanded.Environment["PROJECT"] != "iree" {
		t.Errorf("PROJECT env = %q, want iree", expanded.Environment["PROJECT"])
	}
	if expanded.Environment["WORKSPACE_ROOT"] != "/var/bureau/workspace" {
		t.Errorf("WORKSPACE_ROOT env = %q, want /var/bureau/workspace",
			expanded.Environment["WORKSPACE_ROOT"])
	}
	// HOME should be unchanged (no variable reference).
	if expanded.Environment["HOME"] != "/workspace" {
		t.Errorf("HOME env = %q, want /workspace", expanded.Environment["HOME"])
	}

	// Verify create_dirs expanded.
	if len(expanded.CreateDirs) != 1 {
		t.Fatalf("expected 1 CreateDir, got %d", len(expanded.CreateDirs))
	}
	if expanded.CreateDirs[0] != "/var/bureau/workspace/iree/amdgpu/pm/.local" {
		t.Errorf("CreateDirs[0] = %q, want /var/bureau/workspace/iree/amdgpu/pm/.local",
			expanded.CreateDirs[0])
	}
}

// TestSpecToProfile_CacheRootVariableExpansion verifies that CACHE_ROOT is
// available for template mount expansion. The machine-level cache root
// (/var/bureau/cache) is separate from the workspace cache
// (/var/bureau/workspace/.cache): workspace cache is project-adjacent and
// backed up with workspace data; cache root is machine infrastructure
// managed by the sysadmin principal with tiered access (rw for sysadmin,
// ro for agents).
func TestSpecToProfile_CacheRootVariableExpansion(t *testing.T) {
	t.Parallel()

	spec := &schema.SandboxSpec{
		Command: []string{"/bin/bash"},
		Filesystem: []schema.TemplateMount{
			// Sysadmin-style: full cache rw.
			{
				Source: "${CACHE_ROOT}",
				Dest:   "/cache",
				Mode:   "rw",
			},
			// Agent-style: specific subdirectories ro.
			{
				Source:   "${CACHE_ROOT}/bin",
				Dest:     "/usr/local/bin",
				Mode:     "ro",
				Optional: true,
			},
			{
				Source:   "${CACHE_ROOT}/hf",
				Dest:     "/cache/huggingface",
				Mode:     "ro",
				Optional: true,
			},
		},
		EnvironmentVariables: map[string]string{
			"HF_HOME": "/cache/huggingface",
		},
	}

	profile := specToProfile(spec, "/run/bureau/principal/bureau/sysadmin.sock")

	vars := sandbox.Variables{
		"WORKSPACE_ROOT": "/var/bureau/workspace",
		"CACHE_ROOT":     "/var/bureau/cache",
		"PROXY_SOCKET":   "/run/bureau/proxy.sock",
		"TERM":           "xterm-256color",
	}
	project, worktreePath := workspaceContext("bureau/sysadmin")
	if project != "" {
		vars["PROJECT"] = project
		vars["WORKTREE_PATH"] = worktreePath
	}
	expanded := vars.ExpandProfile(profile)

	// Full cache mount (sysadmin tier).
	if expanded.Filesystem[0].Source != "/var/bureau/cache" {
		t.Errorf("cache mount source = %q, want /var/bureau/cache",
			expanded.Filesystem[0].Source)
	}
	if expanded.Filesystem[0].Mode != "rw" {
		t.Errorf("cache mount mode = %q, want rw", expanded.Filesystem[0].Mode)
	}

	// Read-only bin subdirectory (agent tier).
	if expanded.Filesystem[1].Source != "/var/bureau/cache/bin" {
		t.Errorf("bin mount source = %q, want /var/bureau/cache/bin",
			expanded.Filesystem[1].Source)
	}
	if expanded.Filesystem[1].Mode != "ro" {
		t.Errorf("bin mount mode = %q, want ro", expanded.Filesystem[1].Mode)
	}
	if !expanded.Filesystem[1].Optional {
		t.Error("bin mount should be optional (subdirectory may not exist)")
	}

	// Read-only HuggingFace cache (agent tier).
	if expanded.Filesystem[2].Source != "/var/bureau/cache/hf" {
		t.Errorf("hf mount source = %q, want /var/bureau/cache/hf",
			expanded.Filesystem[2].Source)
	}
}

// TestSpecToProfile_SingleSegmentPrincipalNoWorkspaceVars verifies that
// single-segment principals (no slash) leave workspace variable references
// unexpanded. Templates that incorrectly reference ${PROJECT} for such
// principals will produce literal ${PROJECT} in paths, causing mount
// failures — which is the correct behavior (fail loud, not silent).
func TestSpecToProfile_SingleSegmentPrincipalNoWorkspaceVars(t *testing.T) {
	t.Parallel()

	spec := &schema.SandboxSpec{
		Command: []string{"/bin/bash"},
		Filesystem: []schema.TemplateMount{
			{
				Source: "/var/bureau/workspace/${PROJECT}/${WORKTREE_PATH}",
				Dest:   "/workspace",
				Mode:   "rw",
			},
		},
	}

	profile := specToProfile(spec, "/run/bureau/principal/sysadmin.sock")

	// Build variables for a single-segment principal.
	vars := sandbox.Variables{
		"WORKSPACE_ROOT": "/var/bureau/workspace",
		"CACHE_ROOT":     "/var/bureau/cache",
		"PROXY_SOCKET":   "/run/bureau/principal/sysadmin.sock",
	}
	project, worktreePath := workspaceContext("sysadmin")
	if project != "" {
		vars["PROJECT"] = project
		vars["WORKTREE_PATH"] = worktreePath
	}
	expanded := vars.ExpandProfile(profile)

	// ${PROJECT} and ${WORKTREE_PATH} should remain unexpanded.
	if expanded.Filesystem[0].Source != "/var/bureau/workspace/${PROJECT}/${WORKTREE_PATH}" {
		t.Errorf("mount source = %q, want literal ${PROJECT}/${WORKTREE_PATH} preserved",
			expanded.Filesystem[0].Source)
	}
}
