// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParse(t *testing.T) {
	t.Parallel()

	t.Run("minimal pipeline", func(t *testing.T) {
		t.Parallel()

		content, err := Parse([]byte(`{
  "steps": [
    {"name": "hello", "run": "echo hello"}
  ]
}`))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if len(content.Steps) != 1 {
			t.Fatalf("Steps count = %d, want 1", len(content.Steps))
		}
		if content.Steps[0].Name != "hello" {
			t.Errorf("Steps[0].Name = %q, want %q", content.Steps[0].Name, "hello")
		}
		if content.Steps[0].Run != "echo hello" {
			t.Errorf("Steps[0].Run = %q, want %q", content.Steps[0].Run, "echo hello")
		}
	})

	t.Run("full pipeline", func(t *testing.T) {
		t.Parallel()

		content, err := Parse([]byte(`{
  "description": "Set up development workspace",
  "variables": {
    "REPOSITORY": {
      "description": "Git clone URL",
      "required": true
    },
    "BRANCH": {
      "description": "Branch to checkout",
      "default": "main"
    }
  },
  "steps": [
    {
      "name": "clone",
      "run": "git clone ${REPOSITORY}",
      "when": "test -n '${REPOSITORY}'",
      "timeout": "5m"
    },
    {
      "name": "publish-ready",
      "publish": {
        "event_type": "m.bureau.workspace.ready",
        "room": "${WORKSPACE_ROOM_ID}",
        "state_key": "",
        "content": {"status": "ready"}
      }
    }
  ],
  "log": {
    "room": "${WORKSPACE_ROOM_ID}"
  }
}`))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if content.Description != "Set up development workspace" {
			t.Errorf("Description = %q", content.Description)
		}
		if len(content.Variables) != 2 {
			t.Fatalf("Variables count = %d, want 2", len(content.Variables))
		}
		if !content.Variables["REPOSITORY"].Required {
			t.Error("REPOSITORY should be required")
		}
		if content.Variables["BRANCH"].Default != "main" {
			t.Errorf("BRANCH default = %q, want %q", content.Variables["BRANCH"].Default, "main")
		}
		if len(content.Steps) != 2 {
			t.Fatalf("Steps count = %d, want 2", len(content.Steps))
		}
		if content.Steps[0].Timeout != "5m" {
			t.Errorf("Steps[0].Timeout = %q, want %q", content.Steps[0].Timeout, "5m")
		}
		if content.Steps[1].Publish == nil {
			t.Fatal("Steps[1].Publish is nil")
		}
		if content.Steps[1].Publish.EventType != "m.bureau.workspace.ready" {
			t.Errorf("Steps[1].Publish.EventType = %q", content.Steps[1].Publish.EventType)
		}
		if content.Log == nil {
			t.Fatal("Log is nil")
		}
		if content.Log.Room != "${WORKSPACE_ROOM_ID}" {
			t.Errorf("Log.Room = %q", content.Log.Room)
		}
	})

	t.Run("JSONC with comments", func(t *testing.T) {
		t.Parallel()

		content, err := Parse([]byte(`{
  // Clone and prepare workspace
  "description": "Dev workspace setup",
  "steps": [
    {
      "name": "clone",
      "run": "git clone ${REPO}",
      /* This step is optional because the repo
         might already be cloned */
      "optional": true,
    }
  ]
}`))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if content.Description != "Dev workspace setup" {
			t.Errorf("Description = %q", content.Description)
		}
		if !content.Steps[0].Optional {
			t.Error("Steps[0].Optional should be true")
		}
	})

	t.Run("step with env", func(t *testing.T) {
		t.Parallel()

		content, err := Parse([]byte(`{
  "steps": [
    {
      "name": "build",
      "run": "make build",
      "env": {"CC": "gcc", "CFLAGS": "-O2"}
    }
  ]
}`))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if content.Steps[0].Env["CC"] != "gcc" {
			t.Errorf("Env[CC] = %q, want %q", content.Steps[0].Env["CC"], "gcc")
		}
		if content.Steps[0].Env["CFLAGS"] != "-O2" {
			t.Errorf("Env[CFLAGS] = %q, want %q", content.Steps[0].Env["CFLAGS"], "-O2")
		}
	})

	t.Run("interactive step", func(t *testing.T) {
		t.Parallel()

		content, err := Parse([]byte(`{
  "steps": [
    {"name": "setup", "run": "bash", "interactive": true}
  ]
}`))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if !content.Steps[0].Interactive {
			t.Error("Steps[0].Interactive should be true")
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		t.Parallel()

		_, err := Parse([]byte("{not json"))
		if err == nil {
			t.Fatal("expected error for invalid JSON")
		}
	})

	t.Run("empty object", func(t *testing.T) {
		t.Parallel()

		content, err := Parse([]byte("{}"))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if len(content.Steps) != 0 {
			t.Errorf("Steps count = %d, want 0", len(content.Steps))
		}
	})
}

func TestReadFile(t *testing.T) {
	t.Parallel()

	t.Run("valid JSONC file", func(t *testing.T) {
		t.Parallel()

		directory := t.TempDir()
		path := filepath.Join(directory, "test-pipeline.jsonc")
		err := os.WriteFile(path, []byte(`{
  // A test pipeline
  "description": "Test pipeline",
  "steps": [
    {"name": "test", "run": "go test ./...", "timeout": "10m"},
  ]
}`), 0o644)
		if err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		content, err := ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if content.Description != "Test pipeline" {
			t.Errorf("Description = %q", content.Description)
		}
		if content.Steps[0].Timeout != "10m" {
			t.Errorf("Timeout = %q, want %q", content.Steps[0].Timeout, "10m")
		}
	})

	t.Run("nonexistent file", func(t *testing.T) {
		t.Parallel()

		_, err := ReadFile("/nonexistent/pipeline.json")
		if err == nil {
			t.Fatal("expected error for nonexistent file")
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		t.Parallel()

		directory := t.TempDir()
		path := filepath.Join(directory, "bad.json")
		if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		_, err := ReadFile(path)
		if err == nil {
			t.Fatal("expected error for malformed JSON")
		}
	})
}

func TestNameFromPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path string
		want string
	}{
		{"deploy/content/pipeline/dev-workspace-init.jsonc", "dev-workspace-init"},
		{"dev-workspace-init.json", "dev-workspace-init"},
		{"/absolute/path/to/service-upgrade.jsonc", "service-upgrade"},
		{"no-extension", "no-extension"},
		{"multiple.dots.in.name.jsonc", "multiple.dots.in.name"},
	}

	for _, testCase := range tests {
		t.Run(testCase.path, func(t *testing.T) {
			t.Parallel()

			got := NameFromPath(testCase.path)
			if got != testCase.want {
				t.Errorf("NameFromPath(%q) = %q, want %q", testCase.path, got, testCase.want)
			}
		})
	}
}
