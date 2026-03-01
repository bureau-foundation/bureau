// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/bureau-foundation/bureau/lib/agentdriver"
	"github.com/bureau-foundation/bureau/lib/service"
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
	ToolResponse  json.RawMessage `json:"tool_response,omitempty"`
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

// handlePostToolUse logs tool execution for audit purposes and
// performs plan archival when ExitPlanMode is detected. Audit logging
// is intentionally minimal — the stream-json parser already captures
// full tool call/result data. Plan archival is best-effort: failures
// are logged but do not block Claude Code from continuing.
func handlePostToolUse(event *hookEvent) error {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	logger.Info("tool executed",
		"hook", "post-tool-use",
		"session_id", event.SessionID,
		"tool", event.ToolName,
		"cwd", event.CWD,
	)

	if event.ToolName == "ExitPlanMode" {
		archivePlan(logger, event)
	}

	return nil
}

// exitPlanModeResponse holds the fields from ExitPlanMode's
// tool_response that are relevant for plan archival. Claude Code
// provides the full plan content and file path in the PostToolUse
// hook event, so no filesystem scanning is needed.
type exitPlanModeResponse struct {
	Plan     string `json:"plan"`
	FilePath string `json:"filePath"`
}

// extractPlanResponse extracts plan content and file path from an
// ExitPlanMode tool_response. Returns empty strings if the response
// is missing or doesn't contain plan content.
func extractPlanResponse(toolResponse json.RawMessage) (planContent string, filePath string) {
	if len(toolResponse) == 0 {
		return "", ""
	}
	var response exitPlanModeResponse
	if json.Unmarshal(toolResponse, &response) != nil {
		return "", ""
	}
	return response.Plan, response.FilePath
}

// planLabel derives an artifact label from a plan file path. The
// label follows the pattern "plan/<name>" where <name> is the file's
// base name without extension (e.g., "/scratch/plans/auth-redesign.md"
// becomes "plan/auth-redesign").
func planLabel(filePath string) string {
	if filePath == "" {
		return "plan/unnamed"
	}
	base := filepath.Base(filePath)
	name := strings.TrimSuffix(base, filepath.Ext(base))
	if name == "" {
		return "plan/unnamed"
	}
	return "plan/" + name
}

// archivePlan extracts plan content from an ExitPlanMode tool response
// and archives it as a Bureau artifact via the agent service. If ticket
// context is available (BUREAU_TICKET_ID, BUREAU_TICKET_ROOM, and the
// ticket service socket), the artifact is also attached to the ticket.
//
// This is best-effort: errors are logged but never returned. The plan
// file remains on disk at /scratch/plans/ regardless of archival
// outcome. Plan archival should never block Claude Code.
func archivePlan(logger *slog.Logger, event *hookEvent) {
	planContent, planFilePath := extractPlanResponse(event.ToolResponse)
	if planContent == "" {
		logger.Warn("ExitPlanMode tool response has no plan content")
		return
	}

	label := planLabel(planFilePath)

	// Check if the agent service socket exists. Outside a Bureau
	// sandbox (e.g., local development), the socket won't be present
	// and plan archival is silently skipped.
	if _, err := os.Stat(agentdriver.DefaultAgentServiceSocketPath); err != nil {
		logger.Info("agent service socket not available, skipping plan archival",
			"path", agentdriver.DefaultAgentServiceSocketPath)
		return
	}

	agentClient, err := agentdriver.NewAgentServiceClient(
		agentdriver.DefaultAgentServiceSocketPath,
		agentdriver.DefaultAgentServiceTokenPath,
	)
	if err != nil {
		logger.Warn("connecting to agent service for plan archival", "error", err)
		return
	}

	response, err := agentClient.ArchiveArtifact(context.Background(), agentdriver.ArchiveArtifactRequest{
		Data:        []byte(planContent),
		ContentType: "text/markdown",
		Label:       label,
	})
	if err != nil {
		logger.Warn("archiving plan artifact", "error", err)
		return
	}

	logger.Info("plan archived",
		"ref", response.Ref,
		"size", response.Size,
		"label", label,
	)

	attachPlanToTicket(logger, response.Ref, label)
}

// Ticket service socket paths inside a Bureau sandbox. The ticket
// service is available when the agent template declares
// RequiredServices: ["ticket"].
const (
	ticketServiceSocketPath = "/run/bureau/service/ticket.sock"
	ticketServiceTokenPath  = "/run/bureau/service/token/ticket.token"
)

// attachPlanToTicket attaches a plan artifact to the ticket identified
// by BUREAU_TICKET_ID and BUREAU_TICKET_ROOM environment variables.
// These are set by the daemon for ticket-triggered sandboxes. When
// either is missing or the ticket service socket is unavailable,
// attachment is silently skipped — many agent sandboxes don't have
// ticket context, and that's normal.
func attachPlanToTicket(logger *slog.Logger, artifactRef, label string) {
	ticketID := os.Getenv("BUREAU_TICKET_ID")
	ticketRoom := os.Getenv("BUREAU_TICKET_ROOM")
	if ticketID == "" || ticketRoom == "" {
		return
	}

	if _, err := os.Stat(ticketServiceSocketPath); err != nil {
		logger.Info("ticket service socket not available, skipping attachment",
			"path", ticketServiceSocketPath)
		return
	}

	ticketClient, err := service.NewServiceClient(ticketServiceSocketPath, ticketServiceTokenPath)
	if err != nil {
		logger.Warn("connecting to ticket service for plan attachment", "error", err)
		return
	}

	err = ticketClient.Call(context.Background(), "add-attachment", map[string]any{
		"ticket":       ticketID,
		"room":         ticketRoom,
		"ref":          artifactRef,
		"label":        label,
		"content_type": "text/markdown",
	}, nil)
	if err != nil {
		logger.Warn("attaching plan to ticket", "error", err,
			"ticket", ticketID,
			"ref", artifactRef)
		return
	}

	logger.Info("plan attached to ticket",
		"ticket", ticketID,
		"ref", artifactRef,
		"label", label,
	)
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

// claudeCodeSettings generates the Claude Code settings.local.json
// content for sandboxed agent operation. This configures hooks, sandbox
// behavior, plan storage, and permission defaults. The binary path
// parameter controls hook commands that point back to this binary.
func claudeCodeSettings(binaryPath string) map[string]any {
	return map[string]any{
		// Autonomous operation: Bureau's bwrap namespace provides
		// filesystem isolation, PreToolUse hooks enforce write-path
		// authorization, and the proxy provides credential
		// authorization. Claude Code's permission system is redundant
		// inside the sandbox. This setting is belt-and-suspenders
		// with the --dangerously-skip-permissions CLI flag (the CLI
		// flag has higher precedence and handles cases where a
		// project's committed settings.json overrides this value).
		"permissions": map[string]any{
			"defaultMode": "bypassPermissions",
		},

		// Disable Claude Code's own bubblewrap sandbox. Bureau's
		// bwrap namespace already provides isolation — nested
		// bubblewrap would either fail (namespace nesting) or
		// produce confusing double-sandboxing behavior.
		"sandbox": map[string]any{
			"enabled": false,
		},

		// Direct plan files to /scratch/plans/ for durable storage.
		// /scratch/ is a bind-mounted durable directory that persists
		// across agent sessions. The PostToolUse hook archives plan
		// content from the ExitPlanMode tool_response (which includes
		// the full plan text), so this directory serves as a fallback
		// — plans survive even if archival fails.
		"plansDirectory": "/scratch/plans",

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

// writeClaudeCodeSettings writes the Claude Code configuration to
// .claude/settings.local.json in the given working directory. This
// includes hooks, permissions, sandbox behavior, and plan storage
// configuration for sandboxed agent operation.
func writeClaudeCodeSettings(workingDirectory, binaryPath string) error {
	settingsDirectory := filepath.Join(workingDirectory, ".claude")
	if err := os.MkdirAll(settingsDirectory, 0755); err != nil {
		return fmt.Errorf("creating .claude directory: %w", err)
	}

	settings := claudeCodeSettings(binaryPath)
	data, err := json.MarshalIndent(settings, "", "    ")
	if err != nil {
		return fmt.Errorf("marshaling Claude Code settings: %w", err)
	}

	settingsPath := filepath.Join(settingsDirectory, "settings.local.json")
	if err := os.WriteFile(settingsPath, data, 0644); err != nil {
		return fmt.Errorf("writing %s: %w", settingsPath, err)
	}

	return nil
}
