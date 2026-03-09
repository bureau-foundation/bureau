// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package template

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadTemplateFile(t *testing.T) {
	t.Parallel()

	t.Run("valid JSON", func(t *testing.T) {
		t.Parallel()

		directory := t.TempDir()
		path := filepath.Join(directory, "template.json")
		err := os.WriteFile(path, []byte(`{
  "description": "Test template",
  "command": ["/bin/bash"],
  "environment_variables": {
    "PATH": "/usr/bin:/bin"
  },
  "filesystem": [
    {"source": "/usr", "dest": "/usr", "mode": "ro"}
  ]
}`), 0o644)
		if err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		content, err := readTemplateFile(path)
		if err != nil {
			t.Fatalf("readTemplateFile: %v", err)
		}

		if content.Description != "Test template" {
			t.Errorf("Description = %q, want %q", content.Description, "Test template")
		}
		if len(content.Command) != 1 || content.Command[0] != "/bin/bash" {
			t.Errorf("Command = %v, want [/bin/bash]", content.Command)
		}
		if content.EnvironmentVariables["PATH"] != "/usr/bin:/bin" {
			t.Errorf("PATH = %q, want /usr/bin:/bin", content.EnvironmentVariables["PATH"])
		}
		if len(content.Filesystem) != 1 {
			t.Fatalf("Filesystem count = %d, want 1", len(content.Filesystem))
		}
		if content.Filesystem[0].Mode != "ro" {
			t.Errorf("Filesystem[0].Mode = %q, want ro", content.Filesystem[0].Mode)
		}
	})

	t.Run("JSONC with comments", func(t *testing.T) {
		t.Parallel()

		directory := t.TempDir()
		path := filepath.Join(directory, "template.jsonc")
		err := os.WriteFile(path, []byte(`{
  // GPU agent with IREE runtime support
  "description": "AMDGPU developer agent",
  "inherits": ["bureau/template:llm-agent"],
  "command": ["/usr/local/bin/agent", "--gpu"],

  /* Device access for ROCm/HIP */
  "filesystem": [
    {"source": "/dev/kfd", "dest": "/dev/kfd", "mode": "rw"}
  ],

  "environment_variables": {
    "HSA_OVERRIDE_GFX_VERSION": "11.0.0", // trailing comma OK in JSONC
  }
}`), 0o644)
		if err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		content, err := readTemplateFile(path)
		if err != nil {
			t.Fatalf("readTemplateFile: %v", err)
		}

		if content.Description != "AMDGPU developer agent" {
			t.Errorf("Description = %q, want %q", content.Description, "AMDGPU developer agent")
		}
		if len(content.Inherits) != 1 || content.Inherits[0] != "bureau/template:llm-agent" {
			t.Errorf("Inherits = %v, want [bureau/template:llm-agent]", content.Inherits)
		}
		if len(content.Command) != 2 {
			t.Fatalf("Command length = %d, want 2", len(content.Command))
		}
		if content.EnvironmentVariables["HSA_OVERRIDE_GFX_VERSION"] != "11.0.0" {
			t.Errorf("HSA_OVERRIDE_GFX_VERSION = %q, want 11.0.0", content.EnvironmentVariables["HSA_OVERRIDE_GFX_VERSION"])
		}
		if len(content.Filesystem) != 1 || content.Filesystem[0].Dest != "/dev/kfd" {
			t.Errorf("Filesystem = %v, want one /dev/kfd mount", content.Filesystem)
		}
	})

	t.Run("nonexistent file", func(t *testing.T) {
		t.Parallel()

		_, err := readTemplateFile("/nonexistent/template.json")
		if err == nil {
			t.Fatal("expected error for nonexistent file")
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		t.Parallel()

		directory := t.TempDir()
		path := filepath.Join(directory, "bad.json")
		err := os.WriteFile(path, []byte("{not json at all"), 0o644)
		if err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		_, err = readTemplateFile(path)
		if err == nil {
			t.Fatal("expected error for invalid JSON")
		}
	})

	t.Run("with inheritance", func(t *testing.T) {
		t.Parallel()

		directory := t.TempDir()
		path := filepath.Join(directory, "child.json")
		err := os.WriteFile(path, []byte(`{
  "description": "Child template",
  "inherits": ["bureau/template:base"],
  "environment_variables": {
    "EXTRA": "value"
  }
}`), 0o644)
		if err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		content, err := readTemplateFile(path)
		if err != nil {
			t.Fatalf("readTemplateFile: %v", err)
		}

		if len(content.Inherits) != 1 || content.Inherits[0] != "bureau/template:base" {
			t.Errorf("Inherits = %v, want [bureau/template:base]", content.Inherits)
		}
		if content.EnvironmentVariables["EXTRA"] != "value" {
			t.Errorf("EnvironmentVariables[EXTRA] = %q, want %q", content.EnvironmentVariables["EXTRA"], "value")
		}
	})
}
