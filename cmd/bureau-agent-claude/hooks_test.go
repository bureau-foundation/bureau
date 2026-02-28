// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- PreToolUse tests ---

func TestHandlePreToolUse(t *testing.T) {
	t.Parallel()

	t.Run("allows write to workspace", func(t *testing.T) {
		t.Parallel()

		event := &hookEvent{
			ToolName: "Edit",
			CWD:      "/workspace/project",
			ToolInput: mustMarshal(t, map[string]string{
				"file_path":  "/workspace/project/main.go",
				"old_string": "foo",
				"new_string": "bar",
			}),
		}
		err := handlePreToolUse(event)
		if err != nil {
			t.Fatalf("handlePreToolUse: %v", err)
		}
	})

	t.Run("allows write to scratch", func(t *testing.T) {
		t.Parallel()

		event := &hookEvent{
			ToolName: "Write",
			CWD:      "/workspace/project",
			ToolInput: mustMarshal(t, map[string]string{
				"file_path": "/scratch/notes.md",
				"content":   "hello",
			}),
		}
		err := handlePreToolUse(event)
		if err != nil {
			t.Fatalf("handlePreToolUse: %v", err)
		}
	})

	t.Run("allows write to tmp", func(t *testing.T) {
		t.Parallel()

		event := &hookEvent{
			ToolName: "Edit",
			CWD:      "/workspace/project",
			ToolInput: mustMarshal(t, map[string]string{
				"file_path":  "/tmp/debug.log",
				"old_string": "a",
				"new_string": "b",
			}),
		}
		err := handlePreToolUse(event)
		if err != nil {
			t.Fatalf("handlePreToolUse: %v", err)
		}
	})

	t.Run("allows bash commands", func(t *testing.T) {
		t.Parallel()

		event := &hookEvent{
			ToolName: "Bash",
			CWD:      "/workspace/project",
			ToolInput: mustMarshal(t, map[string]string{
				"command": "ls -la /etc",
			}),
		}
		err := handlePreToolUse(event)
		if err != nil {
			t.Fatalf("handlePreToolUse should allow Bash: %v", err)
		}
	})

	t.Run("allows read operations", func(t *testing.T) {
		t.Parallel()

		readTools := []string{"Read", "Glob", "Grep"}
		for _, toolName := range readTools {
			event := &hookEvent{
				ToolName: toolName,
				CWD:      "/workspace/project",
				ToolInput: mustMarshal(t, map[string]string{
					"file_path": "/etc/passwd",
				}),
			}
			err := handlePreToolUse(event)
			if err != nil {
				t.Fatalf("handlePreToolUse(%s): %v", toolName, err)
			}
		}
	})

	t.Run("resolves relative paths against cwd", func(t *testing.T) {
		t.Parallel()

		event := &hookEvent{
			ToolName: "Edit",
			CWD:      "/workspace/project",
			ToolInput: mustMarshal(t, map[string]string{
				"file_path":  "src/main.go",
				"old_string": "a",
				"new_string": "b",
			}),
		}
		// src/main.go relative to /workspace/project → /workspace/project/src/main.go → allowed
		err := handlePreToolUse(event)
		if err != nil {
			t.Fatalf("handlePreToolUse with relative path: %v", err)
		}
	})

	t.Run("handles notebook path field", func(t *testing.T) {
		t.Parallel()

		event := &hookEvent{
			ToolName: "NotebookEdit",
			CWD:      "/workspace/project",
			ToolInput: mustMarshal(t, map[string]string{
				"notebook_path": "/workspace/project/analysis.ipynb",
				"new_source":    "print('hello')",
			}),
		}
		err := handlePreToolUse(event)
		if err != nil {
			t.Fatalf("handlePreToolUse(NotebookEdit): %v", err)
		}
	})

	t.Run("blocks write outside allowed dirs", func(t *testing.T) {
		t.Parallel()

		event := &hookEvent{
			ToolName: "Edit",
			CWD:      "/workspace/project",
			ToolInput: mustMarshal(t, map[string]string{
				"file_path":  "/etc/passwd",
				"old_string": "root",
				"new_string": "hacked",
			}),
		}
		err := handlePreToolUse(event)
		if err == nil {
			t.Fatal("expected error for write outside allowed dirs")
		}
		var denied *hookDeniedError
		if !errors.As(err, &denied) {
			t.Fatalf("expected hookDeniedError, got %T: %v", err, err)
		}
		if !strings.Contains(denied.Message, "/etc/passwd") {
			t.Errorf("denial message should mention the path, got: %s", denied.Message)
		}
		if !strings.Contains(denied.Message, "/workspace/") {
			t.Errorf("denial message should mention allowed dirs, got: %s", denied.Message)
		}
	})

	t.Run("blocks write to root", func(t *testing.T) {
		t.Parallel()

		event := &hookEvent{
			ToolName: "Write",
			CWD:      "/workspace/project",
			ToolInput: mustMarshal(t, map[string]string{
				"file_path": "/secret.key",
				"content":   "stolen",
			}),
		}
		err := handlePreToolUse(event)
		if err == nil {
			t.Fatal("expected error for write to root")
		}
		var denied *hookDeniedError
		if !errors.As(err, &denied) {
			t.Fatalf("expected hookDeniedError, got %T: %v", err, err)
		}
	})

	t.Run("blocks traversal attack", func(t *testing.T) {
		t.Parallel()

		event := &hookEvent{
			ToolName: "Edit",
			CWD:      "/workspace/project",
			ToolInput: mustMarshal(t, map[string]string{
				"file_path":  "/workspace/project/../../etc/passwd",
				"old_string": "root",
				"new_string": "hacked",
			}),
		}
		err := handlePreToolUse(event)
		if err == nil {
			t.Fatal("expected error for path traversal attack")
		}
		var denied *hookDeniedError
		if !errors.As(err, &denied) {
			t.Fatalf("expected hookDeniedError, got %T: %v", err, err)
		}
	})

	t.Run("allows when tool input has no path", func(t *testing.T) {
		t.Parallel()

		// A tool with unexpected input shape should be allowed
		// rather than blocked — the tool itself validates its args.
		event := &hookEvent{
			ToolName: "Edit",
			CWD:      "/workspace/project",
			ToolInput: mustMarshal(t, map[string]string{
				"something_else": "value",
			}),
		}
		err := handlePreToolUse(event)
		if err != nil {
			t.Fatalf("handlePreToolUse with no path: %v", err)
		}
	})

	t.Run("allows when tool input is empty", func(t *testing.T) {
		t.Parallel()

		event := &hookEvent{
			ToolName:  "Write",
			CWD:       "/workspace/project",
			ToolInput: nil,
		}
		err := handlePreToolUse(event)
		if err != nil {
			t.Fatalf("handlePreToolUse with nil input: %v", err)
		}
	})
}

// --- Path checking tests ---

func TestIsWritePathAllowed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		path    string
		allowed bool
	}{
		{"workspace root", "/workspace/project/main.go", true},
		{"workspace nested", "/workspace/project/src/pkg/file.go", true},
		{"scratch root", "/scratch/plan.md", true},
		{"scratch nested", "/scratch/plans/sprint-1/design.md", true},
		{"tmp root", "/tmp/debug.log", true},
		{"tmp nested", "/tmp/bureau/cache/data", true},
		{"etc passwd", "/etc/passwd", false},
		{"root file", "/secret.key", false},
		{"home directory", "/home/agent/.bashrc", false},
		{"var directory", "/var/log/syslog", false},
		{"usr directory", "/usr/local/bin/exploit", false},
		{"workspace without trailing content", "/workspace", false},
		{"scratch without trailing content", "/scratch", false},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			result := isWritePathAllowed(testCase.path)
			if result != testCase.allowed {
				t.Errorf("isWritePathAllowed(%q) = %v, want %v",
					testCase.path, result, testCase.allowed)
			}
		})
	}
}

func TestResolvePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		cwd      string
		path     string
		expected string
	}{
		{"absolute path unchanged", "/workspace", "/tmp/file.txt", "/tmp/file.txt"},
		{"relative path resolved", "/workspace/project", "src/main.go", "/workspace/project/src/main.go"},
		{"dot relative", "/workspace/project", "./main.go", "/workspace/project/main.go"},
		{"parent traversal cleaned", "/workspace/project/src", "../main.go", "/workspace/project/main.go"},
		{"absolute path cleaned", "/workspace", "/tmp/../etc/passwd", "/etc/passwd"},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			result := resolvePath(testCase.cwd, testCase.path)
			if result != testCase.expected {
				t.Errorf("resolvePath(%q, %q) = %q, want %q",
					testCase.cwd, testCase.path, result, testCase.expected)
			}
		})
	}
}

// --- extractTargetPath tests ---

func TestExtractTargetPath(t *testing.T) {
	t.Parallel()

	t.Run("Edit uses file_path", func(t *testing.T) {
		t.Parallel()
		input := mustMarshal(t, map[string]string{"file_path": "/workspace/main.go"})
		result := extractTargetPath("Edit", input)
		if result != "/workspace/main.go" {
			t.Errorf("got %q, want /workspace/main.go", result)
		}
	})

	t.Run("Write uses file_path", func(t *testing.T) {
		t.Parallel()
		input := mustMarshal(t, map[string]string{"file_path": "/tmp/out.txt"})
		result := extractTargetPath("Write", input)
		if result != "/tmp/out.txt" {
			t.Errorf("got %q, want /tmp/out.txt", result)
		}
	})

	t.Run("NotebookEdit uses notebook_path", func(t *testing.T) {
		t.Parallel()
		input := mustMarshal(t, map[string]string{"notebook_path": "/workspace/nb.ipynb"})
		result := extractTargetPath("NotebookEdit", input)
		if result != "/workspace/nb.ipynb" {
			t.Errorf("got %q, want /workspace/nb.ipynb", result)
		}
	})

	t.Run("Read uses file_path", func(t *testing.T) {
		t.Parallel()
		input := mustMarshal(t, map[string]string{"file_path": "/etc/hosts"})
		result := extractTargetPath("Read", input)
		if result != "/etc/hosts" {
			t.Errorf("got %q, want /etc/hosts", result)
		}
	})

	t.Run("Glob uses path", func(t *testing.T) {
		t.Parallel()
		input := mustMarshal(t, map[string]string{"path": "/workspace", "pattern": "*.go"})
		result := extractTargetPath("Glob", input)
		if result != "/workspace" {
			t.Errorf("got %q, want /workspace", result)
		}
	})

	t.Run("unknown tool returns empty", func(t *testing.T) {
		t.Parallel()
		input := mustMarshal(t, map[string]string{"anything": "value"})
		result := extractTargetPath("WebSearch", input)
		if result != "" {
			t.Errorf("got %q, want empty", result)
		}
	})

	t.Run("nil input returns empty", func(t *testing.T) {
		t.Parallel()
		result := extractTargetPath("Edit", nil)
		if result != "" {
			t.Errorf("got %q, want empty", result)
		}
	})

	t.Run("missing field returns empty", func(t *testing.T) {
		t.Parallel()
		input := mustMarshal(t, map[string]string{"other_field": "value"})
		result := extractTargetPath("Edit", input)
		if result != "" {
			t.Errorf("got %q, want empty", result)
		}
	})
}

// --- PostToolUse tests ---

func TestHandlePostToolUse(t *testing.T) {
	t.Parallel()

	t.Run("logs tool execution", func(t *testing.T) {
		t.Parallel()

		event := &hookEvent{
			SessionID:     "sess-123",
			CWD:           "/workspace/project",
			HookEventName: "PostToolUse",
			ToolName:      "Bash",
			ToolInput:     mustMarshal(t, map[string]string{"command": "go test ./..."}),
		}

		// handlePostToolUse logs to stderr; we can't easily capture
		// that in a unit test, but we verify it doesn't error.
		err := handlePostToolUse(event)
		if err != nil {
			t.Fatalf("handlePostToolUse: %v", err)
		}
	})

	t.Run("succeeds for any tool", func(t *testing.T) {
		t.Parallel()

		tools := []string{"Edit", "Write", "Read", "Bash", "Glob", "Grep", "WebFetch", "ExitPlanMode"}
		for _, toolName := range tools {
			event := &hookEvent{
				SessionID: "sess-456",
				CWD:       "/workspace/project",
				ToolName:  toolName,
			}
			err := handlePostToolUse(event)
			if err != nil {
				t.Fatalf("handlePostToolUse(%s): %v", toolName, err)
			}
		}
	})
}

// --- Hook event parsing tests ---

func TestReadHookEvent(t *testing.T) {
	t.Parallel()

	t.Run("parses valid event", func(t *testing.T) {
		t.Parallel()

		input := `{
			"session_id": "sess-abc",
			"cwd": "/workspace/project",
			"hook_event_name": "PreToolUse",
			"tool_name": "Edit",
			"tool_input": {"file_path": "/workspace/project/main.go"}
		}`

		event, err := readHookEvent(strings.NewReader(input))
		if err != nil {
			t.Fatalf("readHookEvent: %v", err)
		}
		if event.SessionID != "sess-abc" {
			t.Errorf("SessionID = %q, want sess-abc", event.SessionID)
		}
		if event.ToolName != "Edit" {
			t.Errorf("ToolName = %q, want Edit", event.ToolName)
		}
		if event.CWD != "/workspace/project" {
			t.Errorf("CWD = %q, want /workspace/project", event.CWD)
		}
	})

	t.Run("rejects malformed JSON", func(t *testing.T) {
		t.Parallel()

		_, err := readHookEvent(strings.NewReader("not json"))
		if err == nil {
			t.Fatal("expected error for malformed JSON")
		}
	})

	t.Run("handles empty input", func(t *testing.T) {
		t.Parallel()

		_, err := readHookEvent(strings.NewReader(""))
		if err == nil {
			t.Fatal("expected error for empty input")
		}
	})
}

// --- runHook dispatch tests ---

func TestRunHookDispatch(t *testing.T) {
	t.Parallel()

	t.Run("rejects no arguments", func(t *testing.T) {
		t.Parallel()

		err := runHook(nil)
		if err == nil {
			t.Fatal("expected error for no arguments")
		}
		if !strings.Contains(err.Error(), "usage") {
			t.Errorf("error should mention usage, got: %v", err)
		}
	})

	t.Run("rejects unknown event type", func(t *testing.T) {
		t.Parallel()

		// Redirect stdin for the handler.
		oldStdin := os.Stdin
		reader, writer, _ := os.Pipe()
		os.Stdin = reader

		validJSON := `{"session_id":"test","cwd":"/tmp","tool_name":"Edit"}`
		writer.WriteString(validJSON)
		writer.Close()

		err := runHook([]string{"unknown-event"})
		os.Stdin = oldStdin

		if err == nil {
			t.Fatal("expected error for unknown event type")
		}
		if !strings.Contains(err.Error(), "unknown hook event type") {
			t.Errorf("error should mention unknown event type, got: %v", err)
		}
	})
}

// --- Hook settings generation tests ---

func TestHookSettings(t *testing.T) {
	t.Parallel()

	t.Run("generates valid settings JSON", func(t *testing.T) {
		t.Parallel()

		settings := hookSettings("/usr/bin/bureau-agent-claude")
		data, err := json.Marshal(settings)
		if err != nil {
			t.Fatalf("marshal settings: %v", err)
		}

		// Verify it's valid JSON by round-tripping.
		var parsed map[string]any
		if err := json.Unmarshal(data, &parsed); err != nil {
			t.Fatalf("unmarshal settings: %v", err)
		}

		hooks, ok := parsed["hooks"].(map[string]any)
		if !ok {
			t.Fatal("settings should have 'hooks' key")
		}

		if _, ok := hooks["PreToolUse"]; !ok {
			t.Error("hooks should have 'PreToolUse' key")
		}
		if _, ok := hooks["PostToolUse"]; !ok {
			t.Error("hooks should have 'PostToolUse' key")
		}
	})

	t.Run("uses correct binary path", func(t *testing.T) {
		t.Parallel()

		settings := hookSettings("/nix/store/abc123/bureau-agent-claude")
		data, err := json.Marshal(settings)
		if err != nil {
			t.Fatalf("marshal settings: %v", err)
		}

		settingsJSON := string(data)
		if !strings.Contains(settingsJSON, "/nix/store/abc123/bureau-agent-claude hook pre-tool-use") {
			t.Errorf("settings should contain pre-tool-use command with binary path, got: %s", settingsJSON)
		}
		if !strings.Contains(settingsJSON, "/nix/store/abc123/bureau-agent-claude hook post-tool-use") {
			t.Errorf("settings should contain post-tool-use command with binary path, got: %s", settingsJSON)
		}
	})

	t.Run("PreToolUse matcher covers write tools", func(t *testing.T) {
		t.Parallel()

		settings := hookSettings("/bin/test")
		data, err := json.Marshal(settings)
		if err != nil {
			t.Fatalf("marshal settings: %v", err)
		}

		settingsJSON := string(data)
		if !strings.Contains(settingsJSON, "Edit|Write|NotebookEdit") {
			t.Errorf("PreToolUse matcher should cover write tools, got: %s", settingsJSON)
		}
	})
}

func TestWriteHookSettings(t *testing.T) {
	t.Parallel()

	t.Run("creates settings file", func(t *testing.T) {
		t.Parallel()

		directory := t.TempDir()
		err := writeHookSettings(directory, "/bin/test-agent")
		if err != nil {
			t.Fatalf("writeHookSettings: %v", err)
		}

		settingsPath := filepath.Join(directory, ".claude", "settings.local.json")
		data, err := os.ReadFile(settingsPath)
		if err != nil {
			t.Fatalf("reading settings file: %v", err)
		}

		// Verify the file is valid JSON.
		var parsed map[string]any
		if err := json.Unmarshal(data, &parsed); err != nil {
			t.Fatalf("settings file is not valid JSON: %v", err)
		}

		// Verify hooks are present.
		hooks, ok := parsed["hooks"]
		if !ok {
			t.Fatal("settings should have 'hooks' key")
		}
		hooksMap, ok := hooks.(map[string]any)
		if !ok {
			t.Fatal("hooks should be an object")
		}
		if _, ok := hooksMap["PreToolUse"]; !ok {
			t.Error("should have PreToolUse hook")
		}
		if _, ok := hooksMap["PostToolUse"]; !ok {
			t.Error("should have PostToolUse hook")
		}

		// Verify binary path is in the commands.
		content := string(data)
		if !strings.Contains(content, "/bin/test-agent hook pre-tool-use") {
			t.Error("settings should reference binary path for pre-tool-use")
		}
	})

	t.Run("creates .claude directory", func(t *testing.T) {
		t.Parallel()

		directory := t.TempDir()
		err := writeHookSettings(directory, "/bin/test")
		if err != nil {
			t.Fatalf("writeHookSettings: %v", err)
		}

		claudeDir := filepath.Join(directory, ".claude")
		info, err := os.Stat(claudeDir)
		if err != nil {
			t.Fatalf(".claude directory should exist: %v", err)
		}
		if !info.IsDir() {
			t.Error(".claude should be a directory")
		}
	})

	t.Run("idempotent when directory exists", func(t *testing.T) {
		t.Parallel()

		directory := t.TempDir()

		// Pre-create the .claude directory.
		if err := os.MkdirAll(filepath.Join(directory, ".claude"), 0755); err != nil {
			t.Fatalf("creating .claude: %v", err)
		}

		err := writeHookSettings(directory, "/bin/test")
		if err != nil {
			t.Fatalf("writeHookSettings should be idempotent: %v", err)
		}
	})
}

// --- Helpers ---

// mustMarshal marshals v to JSON, failing the test on error.
func mustMarshal(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshaling test data: %v", err)
	}
	return json.RawMessage(data)
}
