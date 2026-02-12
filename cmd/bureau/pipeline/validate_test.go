// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	libpipeline "github.com/bureau-foundation/bureau/lib/pipeline"
	"github.com/bureau-foundation/bureau/lib/schema"
)

func TestValidateValidPipeline(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, "pipeline.jsonc")
	err := os.WriteFile(path, []byte(`{
  "description": "Test pipeline",
  "steps": [
    {"name": "build", "run": "make build"}
  ]
}`), 0o644)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cmd := validateCommand()
	if err := cmd.Run([]string{path}); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestValidateJSONCWithComments(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, "pipeline.jsonc")
	err := os.WriteFile(path, []byte(`{
  // Workspace initialization pipeline.
  "description": "Init workspace",

  /* Variables for customization */
  "variables": {
    "PROJECT": {"description": "project name", "required": true},
  },

  "steps": [
    {"name": "clone", "run": "git clone ${PROJECT}"},
    {"name": "setup", "run": "make setup", "timeout": "5m"},
  ]
}`), 0o644)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cmd := validateCommand()
	if err := cmd.Run([]string{path}); err != nil {
		t.Fatalf("expected no error for JSONC with comments, got: %v", err)
	}
}

func TestValidateNoArgs(t *testing.T) {
	t.Parallel()

	cmd := validateCommand()
	err := cmd.Run([]string{})
	if err == nil {
		t.Fatal("expected error for no args")
	}
	if !strings.Contains(err.Error(), "usage") {
		t.Errorf("error %q should contain usage hint", err.Error())
	}
}

func TestValidateNonexistentFile(t *testing.T) {
	t.Parallel()

	cmd := validateCommand()
	err := cmd.Run([]string{"/nonexistent/pipeline.json"})
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestValidateInvalidJSON(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, "bad.json")
	if err := os.WriteFile(path, []byte("{not json at all"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cmd := validateCommand()
	err := cmd.Run([]string{path})
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestValidateWithIssues(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, "bad-pipeline.json")
	// No steps — validation must catch this.
	if err := os.WriteFile(path, []byte(`{"description": "empty"}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cmd := validateCommand()
	err := cmd.Run([]string{path})
	if err == nil {
		t.Fatal("expected error for pipeline with no steps")
	}
	if !strings.Contains(err.Error(), "validation issue") {
		t.Errorf("error %q should mention validation issues", err.Error())
	}
}

// TestValidatePipelineContent exercises the validation rules via
// lib/pipeline.Validate directly. This covers the structural checks
// that the CLI validate command delegates to.
func TestValidatePipelineContent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		content        *schema.PipelineContent
		expectedIssues int
		wantSubstrings []string
	}{
		{
			name: "valid run step",
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{Name: "build", Run: "make build"},
				},
			},
			expectedIssues: 0,
		},
		{
			name: "valid publish step",
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{
						Name: "publish-config",
						Publish: &schema.PipelinePublish{
							EventType: "m.bureau.workspace",
							Room:      "#iree/config:bureau.local",
							Content:   map[string]any{"status": "ready"},
						},
					},
				},
			},
			expectedIssues: 0,
		},
		{
			name: "no steps",
			content: &schema.PipelineContent{
				Description: "empty pipeline",
			},
			expectedIssues: 1,
			wantSubstrings: []string{"no steps"},
		},
		{
			name: "step missing name",
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{Run: "echo hello"},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"name is required"},
		},
		{
			name: "step with both run and publish",
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{
						Name: "conflict",
						Run:  "echo hello",
						Publish: &schema.PipelinePublish{
							EventType: "m.test",
							Room:      "#test:local",
							Content:   map[string]any{},
						},
					},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"mutually exclusive"},
		},
		{
			name: "step with neither run nor publish nor assert_state",
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{Name: "nothing"},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"must set exactly one of run, publish, or assert_state"},
		},
		{
			name: "check on publish step",
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{
						Name:  "bad-check",
						Check: "curl http://localhost",
						Publish: &schema.PipelinePublish{
							EventType: "m.test",
							Room:      "#test:local",
							Content:   map[string]any{},
						},
					},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"check is only valid on run steps"},
		},
		{
			name: "interactive on publish step",
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{
						Name:        "bad-interactive",
						Interactive: true,
						Publish: &schema.PipelinePublish{
							EventType: "m.test",
							Room:      "#test:local",
							Content:   map[string]any{},
						},
					},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"interactive is only valid on run steps"},
		},
		{
			name: "grace_period on publish step",
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{
						Name:        "bad-grace",
						GracePeriod: "10s",
						Publish: &schema.PipelinePublish{
							EventType: "m.test",
							Room:      "#test:local",
							Content:   map[string]any{},
						},
					},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"grace_period is only valid on run steps"},
		},
		{
			name: "publish missing event_type",
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{
						Name: "bad-publish",
						Publish: &schema.PipelinePublish{
							Room:    "#test:local",
							Content: map[string]any{},
						},
					},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"publish.event_type is required"},
		},
		{
			name: "publish missing room",
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{
						Name: "bad-publish",
						Publish: &schema.PipelinePublish{
							EventType: "m.test",
							Content:   map[string]any{},
						},
					},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"publish.room is required"},
		},
		{
			name: "publish missing content",
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{
						Name: "bad-publish",
						Publish: &schema.PipelinePublish{
							EventType: "m.test",
							Room:      "#test:local",
						},
					},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"publish.content is required"},
		},
		{
			name: "invalid timeout",
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{Name: "slow", Run: "sleep 100", Timeout: "not-a-duration"},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"invalid timeout"},
		},
		{
			name: "invalid grace_period",
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{Name: "slow", Run: "sleep 100", GracePeriod: "xyz"},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"invalid grace_period"},
		},
		{
			name: "log without room",
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{Name: "build", Run: "make"},
				},
				Log: &schema.PipelineLog{},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"log.room is required"},
		},
		{
			name: "valid with log room",
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{Name: "build", Run: "make"},
				},
				Log: &schema.PipelineLog{Room: "#iree/amdgpu/general:bureau.local"},
			},
			expectedIssues: 0,
		},
		{
			name: "valid with timeout and grace_period",
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{Name: "build", Run: "make", Timeout: "30m", GracePeriod: "10s"},
				},
			},
			expectedIssues: 0,
		},
		{
			name: "valid with when guard",
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{Name: "optional", Run: "echo done", When: "${DEPLOY}"},
				},
			},
			expectedIssues: 0,
		},
		{
			name: "multiple issues",
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{Run: "echo no-name"}, // missing name
					{Name: "both", Run: "echo", Publish: &schema.PipelinePublish{EventType: "m.test", Room: "#r:l", Content: map[string]any{}}}, // both set
					{Name: "neither"}, // neither set
				},
				Log: &schema.PipelineLog{},
			},
			expectedIssues: 4, // missing name + both set + neither set + log.room
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			issues := libpipeline.Validate(testCase.content)
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

// TestReadPipelineFile verifies JSONC file reading through the pipeline
// library. This covers the file I/O path that the validate, push, and
// other file-based commands share.
func TestReadPipelineFile(t *testing.T) {
	t.Parallel()

	t.Run("valid JSON", func(t *testing.T) {
		t.Parallel()

		directory := t.TempDir()
		path := filepath.Join(directory, "pipeline.json")
		err := os.WriteFile(path, []byte(`{
  "description": "Deploy pipeline",
  "variables": {
    "TARGET": {"description": "deploy target", "required": true}
  },
  "steps": [
    {"name": "build", "run": "make build"},
    {"name": "deploy", "run": "make deploy TARGET=${TARGET}", "timeout": "10m"}
  ]
}`), 0o644)
		if err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		content, err := libpipeline.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}

		if content.Description != "Deploy pipeline" {
			t.Errorf("Description = %q, want %q", content.Description, "Deploy pipeline")
		}
		if len(content.Steps) != 2 {
			t.Fatalf("Steps count = %d, want 2", len(content.Steps))
		}
		if content.Steps[0].Name != "build" {
			t.Errorf("Steps[0].Name = %q, want %q", content.Steps[0].Name, "build")
		}
		if content.Steps[1].Timeout != "10m" {
			t.Errorf("Steps[1].Timeout = %q, want %q", content.Steps[1].Timeout, "10m")
		}
		if content.Variables["TARGET"].Required != true {
			t.Errorf("Variables[TARGET].Required = false, want true")
		}
	})

	t.Run("JSONC with comments", func(t *testing.T) {
		t.Parallel()

		directory := t.TempDir()
		path := filepath.Join(directory, "pipeline.jsonc")
		err := os.WriteFile(path, []byte(`{
  // GPU build pipeline
  "description": "AMDGPU build",
  "steps": [
    /* Compile the project */
    {"name": "compile", "run": "cmake --build build/"},
    {"name": "test", "run": "ctest --test-dir build/", "optional": true},
  ]
}`), 0o644)
		if err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		content, err := libpipeline.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}

		if content.Description != "AMDGPU build" {
			t.Errorf("Description = %q, want %q", content.Description, "AMDGPU build")
		}
		if len(content.Steps) != 2 {
			t.Fatalf("Steps count = %d, want 2", len(content.Steps))
		}
		if !content.Steps[1].Optional {
			t.Error("Steps[1].Optional = false, want true")
		}
	})

	t.Run("with publish step", func(t *testing.T) {
		t.Parallel()

		directory := t.TempDir()
		path := filepath.Join(directory, "publish-pipeline.json")
		err := os.WriteFile(path, []byte(`{
  "description": "Workspace setup",
  "steps": [
    {"name": "clone", "run": "git clone ${REPO}"},
    {
      "name": "announce",
      "publish": {
        "event_type": "m.bureau.workspace",
        "room": "#iree/config:bureau.local",
        "state_key": "iree/workspace",
        "content": {"status": "ready", "path": "/var/bureau/workspace/iree"}
      }
    }
  ]
}`), 0o644)
		if err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		content, err := libpipeline.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}

		if len(content.Steps) != 2 {
			t.Fatalf("Steps count = %d, want 2", len(content.Steps))
		}
		publishStep := content.Steps[1]
		if publishStep.Publish == nil {
			t.Fatal("Steps[1].Publish is nil")
		}
		if publishStep.Publish.EventType != "m.bureau.workspace" {
			t.Errorf("Publish.EventType = %q, want %q", publishStep.Publish.EventType, "m.bureau.workspace")
		}
		if publishStep.Publish.StateKey != "iree/workspace" {
			t.Errorf("Publish.StateKey = %q, want %q", publishStep.Publish.StateKey, "iree/workspace")
		}
	})

	t.Run("nonexistent file", func(t *testing.T) {
		t.Parallel()

		_, err := libpipeline.ReadFile("/nonexistent/pipeline.json")
		if err == nil {
			t.Fatal("expected error for nonexistent file")
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		t.Parallel()

		directory := t.TempDir()
		path := filepath.Join(directory, "bad.json")
		if err := os.WriteFile(path, []byte("{not json at all"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		_, err := libpipeline.ReadFile(path)
		if err == nil {
			t.Fatal("expected error for invalid JSON")
		}
	})
}
