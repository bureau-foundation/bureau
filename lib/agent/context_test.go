// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/proxyclient"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
)

func mustRoomID(raw string) ref.RoomID {
	roomID, err := ref.ParseRoomID(raw)
	if err != nil {
		panic(fmt.Sprintf("invalid test room ID %q: %v", raw, err))
	}
	return roomID
}

func TestSystemPrompt(t *testing.T) {
	t.Parallel()

	agentContext := &AgentContext{
		Identity: &proxyclient.IdentityResponse{
			UserID:     "@agent/test:bureau.local",
			ServerName: "bureau.local",
		},
		Grants: []schema.Grant{
			{Actions: []string{"matrix/send"}, Targets: []string{"bureau/fleet/*/machine/*"}},
			{Actions: []string{"service/discover"}},
		},
		Services: []proxyclient.ServiceEntry{
			{
				Localpart:   "service/stt/whisper",
				Protocol:    "http",
				Description: "Speech to text transcription",
			},
		},
		Payload: map[string]any{
			"prompt":  "Fix the failing test",
			"project": "bureau",
		},
		ConfigRoomAlias: "#bureau/fleet/prod/machine/ws:bureau.local",
		ConfigRoomID:    mustRoomID("!config:bureau.local"),
	}

	prompt := agentContext.SystemPrompt()

	// The system prompt provides neutral runtime context — it must not
	// prescribe the agent's identity or role. Agents are arbitrary
	// programs (assistants, NPCs, chatbots, etc.); only the operator's
	// configuration (template system prompt, payload) defines what the
	// agent IS.
	if strings.Contains(prompt, "You are running as") || strings.Contains(prompt, "Bureau agent") {
		t.Error("system prompt should not prescribe agent identity or role")
	}
	if !strings.Contains(prompt, "# Runtime Context") {
		t.Error("system prompt should start with neutral Runtime Context header")
	}

	// Verify identity section.
	if !strings.Contains(prompt, "@agent/test:bureau.local") {
		t.Error("system prompt should contain user ID")
	}
	if !strings.Contains(prompt, "bureau.local") {
		t.Error("system prompt should contain server name")
	}

	// Verify grants section.
	if !strings.Contains(prompt, "matrix/send") {
		t.Error("system prompt should contain grant actions")
	}
	if !strings.Contains(prompt, "bureau/fleet/*/machine/*") {
		t.Error("system prompt should contain grant targets")
	}

	// Verify services section.
	if !strings.Contains(prompt, "service/stt/whisper") {
		t.Error("system prompt should contain service localpart")
	}
	if !strings.Contains(prompt, "Speech to text") {
		t.Error("system prompt should contain service description")
	}

	// Verify payload section.
	if !strings.Contains(prompt, "Fix the failing test") {
		t.Error("system prompt should contain payload prompt")
	}
	if !strings.Contains(prompt, "bureau") {
		t.Error("system prompt should contain payload project")
	}
}

func TestSystemPromptMinimal(t *testing.T) {
	t.Parallel()

	agentContext := &AgentContext{
		Identity: &proxyclient.IdentityResponse{
			UserID:     "@agent/test:test.local",
			ServerName: "test.local",
		},
		ConfigRoomAlias: "#bureau/fleet/prod/machine/ws:test.local",
	}

	prompt := agentContext.SystemPrompt()

	// Should not contain sections for empty fields.
	if strings.Contains(prompt, "Authorization Grants") {
		t.Error("system prompt should not contain grants section when empty")
	}
	if strings.Contains(prompt, "Available Services") {
		t.Error("system prompt should not contain services section when empty")
	}
	if strings.Contains(prompt, "Payload") {
		t.Error("system prompt should not contain payload section when empty")
	}
}

func TestWriteSystemPromptFile(t *testing.T) {
	t.Parallel()

	agentContext := &AgentContext{
		Identity: &proxyclient.IdentityResponse{
			UserID:     "@agent/test:test.local",
			ServerName: "test.local",
		},
		ConfigRoomAlias: "#bureau/fleet/prod/machine/ws:test.local",
	}

	path, err := agentContext.WriteSystemPromptFile()
	if err != nil {
		t.Fatalf("WriteSystemPromptFile: %v", err)
	}
	defer os.Remove(path)

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading system prompt file: %v", err)
	}

	if !strings.Contains(string(content), "@agent/test:test.local") {
		t.Error("system prompt file should contain user ID")
	}
}

func TestTaskPrompt(t *testing.T) {
	t.Parallel()

	t.Run("with prompt", func(t *testing.T) {
		t.Parallel()
		agentContext := &AgentContext{
			Payload: map[string]any{"prompt": "Build the feature"},
		}
		if agentContext.TaskPrompt() != "Build the feature" {
			t.Errorf("TaskPrompt = %q, want 'Build the feature'", agentContext.TaskPrompt())
		}
	})

	t.Run("no prompt key", func(t *testing.T) {
		t.Parallel()
		agentContext := &AgentContext{
			Payload: map[string]any{"project": "bureau"},
		}
		if agentContext.TaskPrompt() != "" {
			t.Errorf("TaskPrompt = %q, want empty", agentContext.TaskPrompt())
		}
	})

	t.Run("nil payload", func(t *testing.T) {
		t.Parallel()
		agentContext := &AgentContext{}
		if agentContext.TaskPrompt() != "" {
			t.Errorf("TaskPrompt = %q, want empty", agentContext.TaskPrompt())
		}
	})
}
