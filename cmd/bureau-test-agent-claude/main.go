// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// bureau-test-agent-claude is a mock Claude Code binary for integration
// testing. It accepts the same CLI flags that bureau-agent-claude's
// driver passes to real Claude Code, emits a fixed sequence of
// stream-json events on stdout, and exits cleanly.
//
// This binary does NOT implement agentdriver.Driver. It IS the
// subprocess that bureau-agent-claude's claudeDriver spawns via the
// CLAUDE_BINARY environment variable. From Bureau's perspective, the
// real bureau-agent-claude driver wraps this mock exactly as it would
// wrap real Claude Code: writes .claude/settings.local.json, starts
// the process with stream-json flags, parses stdout, and feeds events
// into agentdriver.Run.
//
// The mock validates that required flags are present (--output-format
// stream-json) and emits events that exercise the full Bureau agent
// pipeline: system init, assistant text, tool use/result, and final
// metrics with token counts.
//
// Debug diagnostics are written to /run/bureau/debug/claude-mock.log
// when the directory is bind-mounted (standard pattern for integration
// test debug output). This includes flag values, settings.local.json
// verification, and event emission confirmation.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

func main() {
	os.Exit(run())
}

func run() int {
	// Accept Claude Code CLI flags. The driver passes these;
	// the mock validates --output-format and records the rest
	// in debug output.
	outputFormat := flag.String("output-format", "", "Output format")
	flag.Bool("print", false, "Print mode")
	flag.Bool("verbose", false, "Verbose output")
	flag.Bool("dangerously-skip-permissions", false, "Skip permissions")
	systemPromptFile := flag.String("append-system-prompt-file", "", "System prompt file")
	flag.Parse()

	// Validate required flags.
	if *outputFormat != "stream-json" {
		fmt.Fprintf(os.Stderr, "expected --output-format stream-json, got %q\n", *outputFormat) //nolint:rawoutput
		return 1
	}

	// Open debug log if the directory exists (bind-mounted by
	// the integration test for post-test inspection).
	var debugLog io.Writer = io.Discard
	const debugLogPath = "/run/bureau/debug/claude-mock.log"
	if debugFile, err := os.OpenFile(debugLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		defer debugFile.Close()
		debugLog = debugFile
	}

	prompt := ""
	if flag.NArg() > 0 {
		prompt = flag.Arg(0)
	}

	fmt.Fprintf(debugLog, "mock claude started: output-format=%s prompt=%q system-prompt-file=%q\n",
		*outputFormat, prompt, *systemPromptFile)

	// Verify that the driver wrote .claude/settings.local.json
	// before starting this process. The CWD is the sandbox's
	// working directory where the driver creates the settings.
	verifySettingsFile(debugLog)

	// Emit stream-json events that exercise the Bureau agent
	// pipeline: system init, text responses, tool call/result
	// round-trip, and final metrics. These map to the event types
	// that claudeDriver.ParseOutput extracts.
	events := []map[string]any{
		{
			"type":       "system",
			"subtype":    "init",
			"session_id": "mock-claude-session-001",
			"message":    "Claude Code mock starting",
			"tools":      []string{"Read", "Write", "Bash", "Edit", "Glob", "Grep"},
		},
		{
			"type":    "assistant",
			"subtype": "text",
			"text":    "I received the prompt and will work on it. Prompt: " + prompt,
		},
		{
			"type":        "assistant",
			"subtype":     "tool_use",
			"tool_use_id": "tu-mock-cc-1",
			"name":        "Read",
			"input":       map[string]string{"file_path": "/workspace/README.md"},
		},
		{
			"type":        "tool",
			"subtype":     "result",
			"tool_use_id": "tu-mock-cc-1",
			"content":     "# Mock workspace\nThis file exists in the test workspace.",
			"is_error":    false,
		},
		{
			"type":    "assistant",
			"subtype": "text",
			"text":    "Task complete. The workspace content has been read.",
		},
		{
			"type":          "result",
			"subtype":       "success",
			"cost_usd":      0.015,
			"input_tokens":  750,
			"output_tokens": 300,
			"num_turns":     2,
			"duration_ms":   1500,
		},
	}

	for _, event := range events {
		data, err := json.Marshal(event)
		if err != nil {
			fmt.Fprintf(os.Stderr, "marshal error: %v\n", err) //nolint:rawoutput
			return 1
		}
		fmt.Fprintf(os.Stdout, "%s\n", data)
	}

	fmt.Fprintf(debugLog, "mock claude finished: emitted %d events\n", len(events))
	return 0
}

// verifySettingsFile checks that .claude/settings.local.json exists in
// the current working directory and contains valid JSON with the
// expected top-level keys. Results are written to the debug log for
// post-test inspection.
func verifySettingsFile(debugLog io.Writer) {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(debugLog, "SETTINGS_ERROR: cannot get cwd: %v\n", err)
		return
	}

	settingsPath := filepath.Join(cwd, ".claude", "settings.local.json")
	settingsData, err := os.ReadFile(settingsPath)
	if err != nil {
		fmt.Fprintf(debugLog, "SETTINGS_MISSING: %v\n", err)
		return
	}

	fmt.Fprintf(debugLog, "SETTINGS_FOUND: %d bytes at %s\n", len(settingsData), settingsPath)

	var settings map[string]any
	if err := json.Unmarshal(settingsData, &settings); err != nil {
		fmt.Fprintf(debugLog, "SETTINGS_INVALID_JSON: %v\n", err)
		return
	}

	keys := make([]string, 0, len(settings))
	for key := range settings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	fmt.Fprintf(debugLog, "SETTINGS_VALID: keys=%v\n", keys)
}
