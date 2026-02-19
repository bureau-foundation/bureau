// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/bureau-foundation/bureau/lib/proxyclient"
	"github.com/bureau-foundation/bureau/lib/schema"
)

const payloadPath = "/run/bureau/payload.json"

// AgentContext holds the Bureau environment information assembled from the
// proxy and local filesystem. It is used to build the system prompt and
// extract the initial task prompt.
type AgentContext struct {
	// Identity is the agent's proxy identity (user ID, server name).
	Identity *proxyclient.IdentityResponse

	// Grants are the pre-resolved authorization grants for this agent.
	Grants []schema.Grant

	// Services is the service directory visible to this agent.
	Services []proxyclient.ServiceEntry

	// Payload is the parsed contents of /run/bureau/payload.json, or nil
	// if no payload was configured for this principal.
	Payload map[string]any

	// ConfigRoomAlias is the Matrix room alias for this agent's config room.
	ConfigRoomAlias string

	// ConfigRoomID is the resolved Matrix room ID for the config room.
	ConfigRoomID string
}

// BuildContext assembles the agent context from the proxy and filesystem.
// It calls the proxy to get identity, grants, and services, reads the
// payload file, and resolves the config room.
func BuildContext(ctx context.Context, proxy *proxyclient.Client, machineName string) (*AgentContext, error) {
	identity, err := proxy.Identity(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting identity from proxy: %w", err)
	}

	grants, err := proxy.Grants(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting grants from proxy: %w", err)
	}

	services, err := proxy.Services(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting services from proxy: %w", err)
	}

	payload := readPayload()

	configRoomAlias := schema.FullRoomAlias(schema.EntityConfigRoomAlias(machineName), proxy.ServerName())
	configRoomID, err := proxy.ResolveAlias(ctx, configRoomAlias)
	if err != nil {
		return nil, fmt.Errorf("resolving config room %q: %w", configRoomAlias, err)
	}

	return &AgentContext{
		Identity:        identity,
		Grants:          grants,
		Services:        services,
		Payload:         payload,
		ConfigRoomAlias: configRoomAlias,
		ConfigRoomID:    configRoomID,
	}, nil
}

// readPayload reads and parses the payload JSON file. Returns nil if the
// file does not exist (no payload was configured for this principal).
func readPayload() map[string]any {
	data, err := os.ReadFile(payloadPath)
	if err != nil {
		return nil
	}
	var payload map[string]any
	if json.Unmarshal(data, &payload) != nil {
		return nil
	}
	return payload
}

// SystemPrompt generates the Bureau system prompt from the agent context.
// This is appended to the agent's own system prompt via a flag like
// --append-system-prompt-file.
func (agentContext *AgentContext) SystemPrompt() string {
	var builder strings.Builder

	builder.WriteString("# Runtime Context\n\n")

	// Identity section.
	builder.WriteString("## Identity\n\n")
	builder.WriteString(fmt.Sprintf("- User ID: %s\n", agentContext.Identity.UserID))
	builder.WriteString(fmt.Sprintf("- Server: %s\n", agentContext.Identity.ServerName))
	builder.WriteString(fmt.Sprintf("- Config room: %s\n", agentContext.ConfigRoomAlias))

	// Grants section.
	if len(agentContext.Grants) > 0 {
		builder.WriteString("\n## Authorization Grants\n\n")
		for _, grant := range agentContext.Grants {
			builder.WriteString(fmt.Sprintf("- Actions: %s", strings.Join(grant.Actions, ", ")))
			if len(grant.Targets) > 0 {
				builder.WriteString(fmt.Sprintf(" → Targets: %s", strings.Join(grant.Targets, ", ")))
			}
			builder.WriteString("\n")
		}
	}

	// Services section.
	if len(agentContext.Services) > 0 {
		builder.WriteString("\n## Available Services\n\n")
		for _, service := range agentContext.Services {
			builder.WriteString(fmt.Sprintf("- %s (%s)", service.Localpart, service.Protocol))
			if service.Description != "" {
				builder.WriteString(fmt.Sprintf(": %s", service.Description))
			}
			builder.WriteString("\n")
		}
	}

	// Payload section.
	if agentContext.Payload != nil {
		builder.WriteString("\n## Payload\n\n")
		payloadJSON, err := json.MarshalIndent(agentContext.Payload, "", "  ")
		if err == nil {
			builder.WriteString("```json\n")
			builder.Write(payloadJSON)
			builder.WriteString("\n```\n")
		}
	}

	return builder.String()
}

// WriteSystemPromptFile writes the system prompt to a temporary file and
// returns the path. The caller is responsible for removing the file after
// the agent process exits.
func (agentContext *AgentContext) WriteSystemPromptFile() (string, error) {
	file, err := os.CreateTemp("", "bureau-agent-system-prompt-*.md")
	if err != nil {
		return "", fmt.Errorf("creating system prompt temp file: %w", err)
	}
	if _, err := file.WriteString(agentContext.SystemPrompt()); err != nil {
		file.Close()
		os.Remove(file.Name())
		return "", fmt.Errorf("writing system prompt: %w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(file.Name())
		return "", fmt.Errorf("closing system prompt file: %w", err)
	}
	return file.Name(), nil
}

// TaskPrompt extracts the initial task prompt from the payload. Returns
// an empty string if no task prompt is configured.
func (agentContext *AgentContext) TaskPrompt() string {
	if agentContext.Payload == nil {
		return ""
	}
	prompt, _ := agentContext.Payload["prompt"].(string)
	return prompt
}
