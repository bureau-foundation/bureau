// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package template

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
)

func TestValidateTemplateContent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		content        *schema.TemplateContent
		expectedIssues int
		wantSubstrings []string
	}{
		{
			name: "valid root template",
			content: &schema.TemplateContent{
				Description: "A valid base template",
				Command:     []string{"/bin/bash"},
			},
			expectedIssues: 0,
		},
		{
			name: "valid child template",
			content: &schema.TemplateContent{
				Description: "A valid child template",
				Inherits:    "bureau/template:base",
			},
			expectedIssues: 0,
		},
		{
			name: "empty description",
			content: &schema.TemplateContent{
				Command: []string{"/bin/bash"},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"description is empty"},
		},
		{
			name: "no command and no inherits",
			content: &schema.TemplateContent{
				Description: "A template with no command and no parent",
			},
			expectedIssues: 1,
			wantSubstrings: []string{"no command defined"},
		},
		{
			name: "invalid inherits reference",
			content: &schema.TemplateContent{
				Description: "Has bad inherits",
				Inherits:    "no-colon-here",
			},
			expectedIssues: 1,
			wantSubstrings: []string{"inherits reference is invalid"},
		},
		{
			name: "filesystem mount missing dest",
			content: &schema.TemplateContent{
				Description: "Has mount issues",
				Command:     []string{"/bin/bash"},
				Filesystem: []schema.TemplateMount{
					{Source: "/usr"},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"dest is required"},
		},
		{
			name: "filesystem unknown type",
			content: &schema.TemplateContent{
				Description: "Has unknown mount type",
				Command:     []string{"/bin/bash"},
				Filesystem: []schema.TemplateMount{
					{Source: "/dev/sda", Dest: "/mnt", Type: "ext4"},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"unknown type"},
		},
		{
			name: "filesystem unknown mode",
			content: &schema.TemplateContent{
				Description: "Has unknown mount mode",
				Command:     []string{"/bin/bash"},
				Filesystem: []schema.TemplateMount{
					{Source: "/usr", Dest: "/usr", Mode: "wx"},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"unknown mode"},
		},
		{
			name: "tmpfs with source path",
			content: &schema.TemplateContent{
				Description: "tmpfs should not have source",
				Command:     []string{"/bin/bash"},
				Filesystem: []schema.TemplateMount{
					{Source: "/something", Dest: "/tmp", Type: "tmpfs"},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"tmpfs mounts should not have a source path"},
		},
		{
			name: "bind mount missing source",
			content: &schema.TemplateContent{
				Description: "bind mount needs source",
				Command:     []string{"/bin/bash"},
				Filesystem: []schema.TemplateMount{
					{Dest: "/usr"},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"bind mounts require a source path"},
		},
		{
			name: "negative resource limits",
			content: &schema.TemplateContent{
				Description: "Has negative resources",
				Command:     []string{"/bin/bash"},
				Resources: &schema.TemplateResources{
					CPUShares:     -1,
					MemoryLimitMB: -512,
					PidsLimit:     -10,
				},
			},
			expectedIssues: 3,
			wantSubstrings: []string{"cpu_shares", "memory_limit_mb", "pids_limit"},
		},
		{
			name: "empty role command",
			content: &schema.TemplateContent{
				Description: "Has empty role",
				Command:     []string{"/bin/bash"},
				Roles: map[string][]string{
					"agent": {},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{`roles["agent"]`},
		},
		{
			name: "multiple issues",
			content: &schema.TemplateContent{
				Filesystem: []schema.TemplateMount{
					{Dest: "/usr"},
					{Source: "/bad", Dest: "/bad", Type: "zfs"},
				},
				Roles: map[string][]string{
					"empty": {},
				},
			},
			expectedIssues: 5, // description, no command, bind without source, unknown type, empty role
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			issues := validateTemplateContent(testCase.content)
			if len(issues) != testCase.expectedIssues {
				t.Fatalf("got %d issues, want %d:\n%s", len(issues), testCase.expectedIssues, strings.Join(issues, "\n"))
			}

			for _, substring := range testCase.wantSubstrings {
				found := false
				for _, issue := range issues {
					if strings.Contains(issue, substring) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected issue containing %q, got:\n%s", substring, strings.Join(issues, "\n"))
				}
			}
		})
	}
}

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
  "inherits": "bureau/template:llm-agent",
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
		if content.Inherits != "bureau/template:llm-agent" {
			t.Errorf("Inherits = %q, want %q", content.Inherits, "bureau/template:llm-agent")
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
  "inherits": "bureau/template:base",
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

		if content.Inherits != "bureau/template:base" {
			t.Errorf("Inherits = %q, want %q", content.Inherits, "bureau/template:base")
		}
		if content.EnvironmentVariables["EXTRA"] != "value" {
			t.Errorf("EnvironmentVariables[EXTRA] = %q, want %q", content.EnvironmentVariables["EXTRA"], "value")
		}
	})
}
