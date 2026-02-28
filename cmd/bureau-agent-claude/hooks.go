// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// hookEvent is the JSON envelope Claude Code sends to hook handlers on
// stdin. All hook types share this structure — unused fields are zero
// for event types that don't populate them.
type hookEvent struct {
	SessionID     string          `json:"session_id"`
	CWD           string          `json:"cwd"`
	HookEventName string          `json:"hook_event_name"`
	ToolName      string          `json:"tool_name"`
	ToolInput     json.RawMessage `json:"tool_input"`
	ToolResult    json.RawMessage `json:"tool_result,omitempty"`
}

// allowedWritePrefixes are the directories where write operations are
// permitted inside a Bureau sandbox. These match the standard RW bind
// mounts: /workspace (git worktree), /scratch (durable working state),
// and /tmp (ephemeral). The sandbox's bwrap namespace provides the
// primary isolation; this check is defense-in-depth.
var allowedWritePrefixes = []string{
	"/workspace/",
	"/scratch/",
	"/tmp/",
}

// writeTools are the Claude Code tool names that perform filesystem
// writes. PreToolUse checks these against allowedWritePrefixes.
var writeTools = map[string]bool{
	"Edit":         true,
	"Write":        true,
	"NotebookEdit": true,
}

// exitCodeDenied is the exit code Claude Code uses for blocked
// PreToolUse hooks. When a hook exits with code 2, Claude Code treats
// the tool call as denied and surfaces the stderr content as feedback
// to the model.
const exitCodeDenied = 2

// hookDeniedError is returned by hook handlers to indicate that the
// tool call should be blocked. The Message is written to stderr so
// Claude Code can surface it as feedback to the model. runHook
// translates this to exit code 2.
type hookDeniedError struct {
	Message string
}

func (e *hookDeniedError) Error() string {
	return e.Message
}

// runHook is the entry point for hook handler mode. It reads a hook
// event from stdin, dispatches to the appropriate handler, and exits.
// Handlers return nil for allow, hookDeniedError for deny (exit 2),
// or a regular error for failures (exit 1).
func runHook(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: bureau-agent-claude hook <event-type>")
	}

	eventType := args[0]

	event, err := readHookEvent(os.Stdin)
	if err != nil {
		return fmt.Errorf("reading hook event: %w", err)
	}

	var handlerError error
	switch eventType {
	case "pre-tool-use":
		handlerError = handlePreToolUse(event)
	case "post-tool-use":
		handlerError = handlePostToolUse(event)
	case "stop":
		return nil
	default:
		return fmt.Errorf("unknown hook event type: %q", eventType)
	}

	// Translate hookDeniedError into exit code 2 with the denial
	// message on stderr. Claude Code reads stderr as feedback.
	var denied *hookDeniedError
	if errors.As(handlerError, &denied) {
		fmt.Fprint(os.Stderr, denied.Message) //nolint:rawoutput — Claude Code hook protocol: stderr content is surfaced to the model as denial feedback when exit code is 2
		os.Exit(exitCodeDenied)
	}

	return handlerError
}

// readHookEvent reads and parses a hook event from the given reader.
func readHookEvent(reader io.Reader) (*hookEvent, error) {
	var event hookEvent
	if err := json.NewDecoder(reader).Decode(&event); err != nil {
		return nil, fmt.Errorf("parsing hook event JSON: %w", err)
	}
	return &event, nil
}

// handlePreToolUse checks whether a tool call should be allowed. For
// write tools (Edit, Write, NotebookEdit), it verifies the target
// path is within the allowed sandbox directories. All other tools
// are allowed unconditionally — read operations are low-risk inside
// the sandbox, and Bash commands are audited but not blocked.
func handlePreToolUse(event *hookEvent) error {
	if !writeTools[event.ToolName] {
		return nil
	}

	targetPath := extractTargetPath(event.ToolName, event.ToolInput)
	if targetPath == "" {
		// No path found in tool input — allow the call rather
		// than failing on an unexpected input shape. The tool
		// itself will validate its arguments.
		return nil
	}

	resolvedPath := resolvePath(event.CWD, targetPath)

	if !isWritePathAllowed(resolvedPath) {
		return &hookDeniedError{
			Message: fmt.Sprintf("Write to %q is not permitted. Allowed directories: %s\n",
				resolvedPath, strings.Join(allowedWritePrefixes, ", ")),
		}
	}

	return nil
}

// handlePostToolUse logs tool execution for audit purposes. This is
// intentionally minimal — the stream-json parser already captures
// full tool call/result data. The audit hook adds a Bureau-side
// record that confirms the tool actually executed.
func handlePostToolUse(event *hookEvent) error {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	logger.Info("tool executed",
		"hook", "post-tool-use",
		"session_id", event.SessionID,
		"tool", event.ToolName,
		"cwd", event.CWD,
	)
	return nil
}

// extractTargetPath extracts the filesystem path from a tool's input
// JSON. Different tools use different field names for their target
// path.
func extractTargetPath(toolName string, input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}

	var fields map[string]json.RawMessage
	if json.Unmarshal(input, &fields) != nil {
		return ""
	}

	var fieldName string
	switch toolName {
	case "Edit", "Write", "Read":
		fieldName = "file_path"
	case "NotebookEdit":
		fieldName = "notebook_path"
	case "Glob", "Grep":
		fieldName = "path"
	default:
		return ""
	}

	raw, ok := fields[fieldName]
	if !ok {
		return ""
	}

	var path string
	if json.Unmarshal(raw, &path) != nil {
		return ""
	}
	return path
}

// resolvePath resolves a potentially relative path against the given
// working directory. Absolute paths are cleaned but not modified.
func resolvePath(cwd, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(cwd, path))
}

// isWritePathAllowed checks whether a resolved absolute path falls
// within one of the allowed write directories.
func isWritePathAllowed(path string) bool {
	for _, prefix := range allowedWritePrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// hookSettings generates the Claude Code settings.local.json content
// with hooks pointing to the given binary path.
func hookSettings(binaryPath string) map[string]any {
	return map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []map[string]any{
				{
					"matcher": "Edit|Write|NotebookEdit",
					"hooks": []map[string]any{
						{
							"type":    "command",
							"command": binaryPath + " hook pre-tool-use",
						},
					},
				},
			},
			"PostToolUse": []map[string]any{
				{
					"hooks": []map[string]any{
						{
							"type":    "command",
							"command": binaryPath + " hook post-tool-use",
							"timeout": 10,
						},
					},
				},
			},
		},
	}
}

// writeHookSettings writes the Claude Code hook configuration to
// .claude/settings.local.json in the given working directory. The
// hooks point to the given binary path for handling hook events.
func writeHookSettings(workingDirectory, binaryPath string) error {
	settingsDirectory := filepath.Join(workingDirectory, ".claude")
	if err := os.MkdirAll(settingsDirectory, 0755); err != nil {
		return fmt.Errorf("creating .claude directory: %w", err)
	}

	settings := hookSettings(binaryPath)
	data, err := json.MarshalIndent(settings, "", "    ")
	if err != nil {
		return fmt.Errorf("marshaling hook settings: %w", err)
	}

	settingsPath := filepath.Join(settingsDirectory, "settings.local.json")
	if err := os.WriteFile(settingsPath, data, 0644); err != nil {
		return fmt.Errorf("writing %s: %w", settingsPath, err)
	}

	return nil
}
