// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
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

	t.Run("ExitPlanMode triggers plan archival attempt", func(t *testing.T) {
		t.Parallel()

		// Outside a Bureau sandbox, archivePlan will log that the
		// agent service socket is unavailable and return. This test
		// confirms the dispatch path works without erroring.
		event := &hookEvent{
			SessionID: "sess-plan-1",
			CWD:       "/workspace/project",
			ToolName:  "ExitPlanMode",
			ToolResponse: mustMarshal(t, map[string]string{
				"plan":     "# My Plan\n\nDo the thing.",
				"filePath": "/scratch/plans/my-plan.md",
			}),
		}
		err := handlePostToolUse(event)
		if err != nil {
			t.Fatalf("handlePostToolUse(ExitPlanMode): %v", err)
		}
	})
}

// --- Plan response extraction tests ---

func TestExtractPlanResponse(t *testing.T) {
	t.Parallel()

	t.Run("extracts plan content and file path", func(t *testing.T) {
		t.Parallel()

		toolResponse := mustMarshal(t, map[string]string{
			"plan":     "# Auth Redesign\n\nSwitch to JWT tokens.",
			"filePath": "/scratch/plans/auth-redesign.md",
		})
		planContent, filePath := extractPlanResponse(toolResponse)
		if planContent != "# Auth Redesign\n\nSwitch to JWT tokens." {
			t.Errorf("planContent = %q, want auth redesign content", planContent)
		}
		if filePath != "/scratch/plans/auth-redesign.md" {
			t.Errorf("filePath = %q, want /scratch/plans/auth-redesign.md", filePath)
		}
	})

	t.Run("returns empty for nil response", func(t *testing.T) {
		t.Parallel()

		planContent, filePath := extractPlanResponse(nil)
		if planContent != "" {
			t.Errorf("planContent = %q, want empty", planContent)
		}
		if filePath != "" {
			t.Errorf("filePath = %q, want empty", filePath)
		}
	})

	t.Run("returns empty for empty response", func(t *testing.T) {
		t.Parallel()

		planContent, filePath := extractPlanResponse(json.RawMessage{})
		if planContent != "" {
			t.Errorf("planContent = %q, want empty", planContent)
		}
		if filePath != "" {
			t.Errorf("filePath = %q, want empty", filePath)
		}
	})

	t.Run("returns empty for malformed JSON", func(t *testing.T) {
		t.Parallel()

		planContent, filePath := extractPlanResponse(json.RawMessage(`not json`))
		if planContent != "" {
			t.Errorf("planContent = %q, want empty", planContent)
		}
		if filePath != "" {
			t.Errorf("filePath = %q, want empty", filePath)
		}
	})

	t.Run("returns empty plan when field missing", func(t *testing.T) {
		t.Parallel()

		// Claude Code sends additional fields (isAgent, hasTaskTool)
		// but the plan and filePath fields are the ones we care about.
		// If plan is missing, content should be empty.
		toolResponse := mustMarshal(t, map[string]bool{
			"isAgent":     false,
			"hasTaskTool": true,
		})
		planContent, filePath := extractPlanResponse(toolResponse)
		if planContent != "" {
			t.Errorf("planContent = %q, want empty", planContent)
		}
		if filePath != "" {
			t.Errorf("filePath = %q, want empty", filePath)
		}
	})

	t.Run("handles response with extra fields", func(t *testing.T) {
		t.Parallel()

		// Claude Code's actual ExitPlanMode response includes extra
		// fields beyond plan and filePath. Verify we extract correctly
		// and ignore the rest.
		toolResponse := mustMarshal(t, map[string]any{
			"plan":        "# Migration Plan\n\nStep 1: backup.",
			"filePath":    "/scratch/plans/migration.md",
			"isAgent":     false,
			"hasTaskTool": true,
		})
		planContent, filePath := extractPlanResponse(toolResponse)
		if planContent != "# Migration Plan\n\nStep 1: backup." {
			t.Errorf("planContent = %q, want migration plan content", planContent)
		}
		if filePath != "/scratch/plans/migration.md" {
			t.Errorf("filePath = %q, want /scratch/plans/migration.md", filePath)
		}
	})
}

// --- Plan label derivation tests ---

func TestPlanLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		filePath string
		expected string
	}{
		{
			name:     "standard plan path",
			filePath: "/scratch/plans/auth-redesign.md",
			expected: "plan/auth-redesign",
		},
		{
			name:     "nested path",
			filePath: "/scratch/plans/sprint-3/database-migration.md",
			expected: "plan/database-migration",
		},
		{
			name:     "no extension",
			filePath: "/scratch/plans/quick-fix",
			expected: "plan/quick-fix",
		},
		{
			name:     "double extension uses outer",
			filePath: "/scratch/plans/backup.tar.gz",
			expected: "plan/backup.tar",
		},
		{
			name:     "empty path",
			filePath: "",
			expected: "plan/unnamed",
		},
		{
			name:     "claude code style name",
			filePath: "/scratch/plans/transient-launching-hopper.md",
			expected: "plan/transient-launching-hopper",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			result := planLabel(testCase.filePath)
			if result != testCase.expected {
				t.Errorf("planLabel(%q) = %q, want %q",
					testCase.filePath, result, testCase.expected)
			}
		})
	}
}

// --- archivePlan early-exit tests ---
//
// archivePlan connects to real services (agent service socket, ticket
// service socket). These tests verify the early-exit paths that run
// outside a Bureau sandbox — no service sockets available.

func TestArchivePlanNoServiceSocket(t *testing.T) {
	t.Parallel()

	t.Run("skips archival when agent service socket missing", func(t *testing.T) {
		t.Parallel()

		// Outside a Bureau sandbox, the agent service socket at
		// /run/bureau/service/agent.sock does not exist. archivePlan
		// should log this and return without error.
		event := &hookEvent{
			SessionID: "sess-archive-1",
			CWD:       "/workspace/project",
			ToolName:  "ExitPlanMode",
			ToolResponse: mustMarshal(t, map[string]any{
				"plan":     "# Test Plan\n\nThis is a test.",
				"filePath": "/scratch/plans/test-plan.md",
			}),
		}

		// archivePlan logs to a logger but never returns an error.
		// We're verifying it doesn't panic or hang.
		logger := discardLogger()
		archivePlan(logger, event)
	})

	t.Run("skips archival when plan content empty", func(t *testing.T) {
		t.Parallel()

		event := &hookEvent{
			SessionID:    "sess-archive-2",
			CWD:          "/workspace/project",
			ToolName:     "ExitPlanMode",
			ToolResponse: mustMarshal(t, map[string]any{}),
		}

		logger := discardLogger()
		archivePlan(logger, event)
	})

	t.Run("skips archival when tool response nil", func(t *testing.T) {
		t.Parallel()

		event := &hookEvent{
			SessionID:    "sess-archive-3",
			CWD:          "/workspace/project",
			ToolName:     "ExitPlanMode",
			ToolResponse: nil,
		}

		logger := discardLogger()
		archivePlan(logger, event)
	})
}

// --- attachPlanToTicket early-exit tests ---

// TestAttachPlanToTicketNoEnvVars tests the early-exit paths of
// attachPlanToTicket. These subtests use t.Setenv to control the
// BUREAU_TICKET_ID and BUREAU_TICKET_ROOM environment variables,
// which precludes t.Parallel on subtests.
func TestAttachPlanToTicketNoEnvVars(t *testing.T) {
	t.Run("skips when BUREAU_TICKET_ID missing", func(t *testing.T) {
		t.Setenv("BUREAU_TICKET_ID", "")
		t.Setenv("BUREAU_TICKET_ROOM", "!room:bureau.local")

		logger := discardLogger()
		attachPlanToTicket(logger, "ref-abc123", "plan/test")
	})

	t.Run("skips when BUREAU_TICKET_ROOM missing", func(t *testing.T) {
		t.Setenv("BUREAU_TICKET_ID", "tkt-test-1")
		t.Setenv("BUREAU_TICKET_ROOM", "")

		logger := discardLogger()
		attachPlanToTicket(logger, "ref-abc123", "plan/test")
	})

	t.Run("skips when both env vars missing", func(t *testing.T) {
		t.Setenv("BUREAU_TICKET_ID", "")
		t.Setenv("BUREAU_TICKET_ROOM", "")

		logger := discardLogger()
		attachPlanToTicket(logger, "ref-abc123", "plan/test")
	})

	t.Run("skips when ticket service socket missing", func(t *testing.T) {
		t.Setenv("BUREAU_TICKET_ID", "tkt-test-2")
		t.Setenv("BUREAU_TICKET_ROOM", "!room:bureau.local")

		// The ticket service socket at /run/bureau/service/ticket.sock
		// won't exist outside a sandbox. attachPlanToTicket should
		// log this and return.
		logger := discardLogger()
		attachPlanToTicket(logger, "ref-abc123", "plan/test")
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

// --- Claude Code settings generation tests ---

func TestClaudeCodeSettings(t *testing.T) {
	t.Parallel()

	t.Run("generates valid settings JSON", func(t *testing.T) {
		t.Parallel()

		settings := claudeCodeSettings("/usr/bin/bureau-agent-claude", "bureau")
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

		settings := claudeCodeSettings("/nix/store/abc123/bureau-agent-claude", "bureau")
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

		settings := claudeCodeSettings("/bin/test", "bureau")
		data, err := json.Marshal(settings)
		if err != nil {
			t.Fatalf("marshal settings: %v", err)
		}

		settingsJSON := string(data)
		if !strings.Contains(settingsJSON, "Edit|Write|NotebookEdit") {
			t.Errorf("PreToolUse matcher should cover write tools, got: %s", settingsJSON)
		}
	})

	t.Run("bypasses permissions for autonomous operation", func(t *testing.T) {
		t.Parallel()

		settings := claudeCodeSettings("/bin/test", "bureau")
		data, err := json.Marshal(settings)
		if err != nil {
			t.Fatalf("marshal settings: %v", err)
		}

		settingsJSON := string(data)
		if !strings.Contains(settingsJSON, "bypassPermissions") {
			t.Errorf("settings should set bypassPermissions, got: %s", settingsJSON)
		}
	})

	t.Run("disables Claude Code sandbox", func(t *testing.T) {
		t.Parallel()

		settings := claudeCodeSettings("/bin/test", "bureau")

		sandboxSection, ok := settings["sandbox"].(map[string]any)
		if !ok {
			t.Fatal("settings should have 'sandbox' section")
		}
		enabled, ok := sandboxSection["enabled"].(bool)
		if !ok {
			t.Fatal("sandbox.enabled should be a bool")
		}
		if enabled {
			t.Error("sandbox.enabled should be false (Bureau provides isolation)")
		}
	})

	t.Run("sets plans directory to scratch", func(t *testing.T) {
		t.Parallel()

		settings := claudeCodeSettings("/bin/test", "bureau")

		plansDirectory, ok := settings["plansDirectory"].(string)
		if !ok {
			t.Fatal("settings should have 'plansDirectory' string")
		}
		if plansDirectory != "/scratch/plans" {
			t.Errorf("plansDirectory = %q, want /scratch/plans", plansDirectory)
		}
	})

	t.Run("configures MCP server with bureau binary", func(t *testing.T) {
		t.Parallel()

		settings := claudeCodeSettings("/bin/test", "/nix/store/xyz/bureau")

		mcpServers, ok := settings["mcpServers"].(map[string]any)
		if !ok {
			t.Fatal("settings should have 'mcpServers' section")
		}

		bureau, ok := mcpServers["bureau"].(map[string]any)
		if !ok {
			t.Fatal("mcpServers should have 'bureau' entry")
		}

		command, ok := bureau["command"].(string)
		if !ok {
			t.Fatal("bureau MCP server should have 'command' string")
		}
		if command != "/nix/store/xyz/bureau" {
			t.Errorf("command = %q, want /nix/store/xyz/bureau", command)
		}

		args, ok := bureau["args"].([]string)
		if !ok {
			t.Fatal("bureau MCP server should have 'args' string slice")
		}
		expectedArgs := []string{"mcp", "serve", "--progressive"}
		if len(args) != len(expectedArgs) {
			t.Fatalf("args length = %d, want %d", len(args), len(expectedArgs))
		}
		for i, arg := range args {
			if arg != expectedArgs[i] {
				t.Errorf("args[%d] = %q, want %q", i, arg, expectedArgs[i])
			}
		}
	})
}

func TestWriteClaudeCodeSettings(t *testing.T) {
	t.Parallel()

	t.Run("creates settings file with all sections", func(t *testing.T) {
		t.Parallel()

		directory := t.TempDir()
		err := writeClaudeCodeSettings(directory, "/bin/test-agent", "bureau")
		if err != nil {
			t.Fatalf("writeClaudeCodeSettings: %v", err)
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

		// Verify permissions, sandbox, plans directory, and MCP servers.
		if _, ok := parsed["permissions"]; !ok {
			t.Error("should have 'permissions' section")
		}
		if _, ok := parsed["sandbox"]; !ok {
			t.Error("should have 'sandbox' section")
		}
		if _, ok := parsed["plansDirectory"]; !ok {
			t.Error("should have 'plansDirectory'")
		}
		if _, ok := parsed["mcpServers"]; !ok {
			t.Error("should have 'mcpServers' section")
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
		err := writeClaudeCodeSettings(directory, "/bin/test", "bureau")
		if err != nil {
			t.Fatalf("writeClaudeCodeSettings: %v", err)
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

		err := writeClaudeCodeSettings(directory, "/bin/test", "bureau")
		if err != nil {
			t.Fatalf("writeClaudeCodeSettings should be idempotent: %v", err)
		}
	})
}

// --- hookEvent JSON parsing tests ---

func TestHookEventToolResponse(t *testing.T) {
	t.Parallel()

	t.Run("parses tool_response field from PostToolUse", func(t *testing.T) {
		t.Parallel()

		// This is the actual JSON shape Claude Code sends for PostToolUse.
		// The field is "tool_response" (not "tool_result").
		input := `{
			"session_id": "sess-xyz",
			"cwd": "/workspace",
			"hook_event_name": "PostToolUse",
			"tool_name": "ExitPlanMode",
			"tool_input": {},
			"tool_response": {
				"plan": "# Test\n\nContent here.",
				"filePath": "/scratch/plans/test.md",
				"isAgent": false,
				"hasTaskTool": true
			}
		}`

		event, err := readHookEvent(strings.NewReader(input))
		if err != nil {
			t.Fatalf("readHookEvent: %v", err)
		}
		if event.ToolName != "ExitPlanMode" {
			t.Errorf("ToolName = %q, want ExitPlanMode", event.ToolName)
		}
		if len(event.ToolResponse) == 0 {
			t.Fatal("ToolResponse should not be empty for PostToolUse")
		}

		// Verify plan content can be extracted from the parsed event.
		planContent, filePath := extractPlanResponse(event.ToolResponse)
		if planContent != "# Test\n\nContent here." {
			t.Errorf("planContent = %q, want test content", planContent)
		}
		if filePath != "/scratch/plans/test.md" {
			t.Errorf("filePath = %q, want /scratch/plans/test.md", filePath)
		}
	})

	t.Run("tool_response absent for PreToolUse", func(t *testing.T) {
		t.Parallel()

		input := `{
			"session_id": "sess-pre",
			"cwd": "/workspace",
			"hook_event_name": "PreToolUse",
			"tool_name": "Edit",
			"tool_input": {"file_path": "/workspace/main.go"}
		}`

		event, err := readHookEvent(strings.NewReader(input))
		if err != nil {
			t.Fatalf("readHookEvent: %v", err)
		}
		if len(event.ToolResponse) != 0 {
			t.Errorf("ToolResponse should be empty for PreToolUse, got %s", string(event.ToolResponse))
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

// discardLogger returns a slog.Logger that writes to io.Discard.
// Useful for testing functions that take a logger but whose log output
// is not under test.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}
