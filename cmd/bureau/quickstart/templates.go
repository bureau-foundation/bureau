// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package quickstart

import (
	"github.com/bureau-foundation/bureau/lib/schema"
)

// agentTemplates returns the built-in quickstart agent templates. These
// templates inherit from the base templates published by "bureau matrix setup"
// and add agent-specific configuration.
//
// Each template uses ${PROXY_SOCKET}, ${MACHINE_NAME}, and ${SERVER_NAME}
// variable expansion — these are resolved by the launcher at sandbox creation
// time. The agent reads these to discover its config room alias.
func agentTemplates() map[string]schema.TemplateContent {
	return map[string]schema.TemplateContent{
		"sysadmin-test": {
			Description: "Test agent for quickstart validation — verifies proxy identity, Matrix auth, and bidirectional messaging",
			Inherits:    []string{"bureau/template:base"},
			Command:     []string{"bureau-test-agent"},
			EnvironmentVariables: map[string]string{
				"BUREAU_PROXY_SOCKET": "${PROXY_SOCKET}",
				"BUREAU_MACHINE_NAME": "${MACHINE_NAME}",
				"BUREAU_SERVER_NAME":  "${SERVER_NAME}",
			},
		},
		"sysadmin-claude": {
			Description: "Claude Code agent — interactive coding assistant in a sandboxed terminal",
			Inherits:    []string{"bureau/template:base-networked"},
			Command:     []string{"claude", "--dangerously-skip-permissions"},
			EnvironmentVariables: map[string]string{
				"BUREAU_PROXY_SOCKET": "${PROXY_SOCKET}",
				"BUREAU_MACHINE_NAME": "${MACHINE_NAME}",
				"BUREAU_SERVER_NAME":  "${SERVER_NAME}",
			},
		},
	}
}

// templateForAgent returns the template name to use for a given agent type.
// Returns the template name and whether the agent type is recognized.
func templateForAgent(agent string) (string, bool) {
	switch agent {
	case "test":
		return "sysadmin-test", true
	case "claude":
		return "sysadmin-claude", true
	default:
		return "", false
	}
}

// agentBinary returns the binary name that must be available in PATH inside
// the sandbox for a given agent type.
func agentBinary(agent string) string {
	switch agent {
	case "test":
		return "bureau-test-agent"
	case "claude":
		return "claude"
	default:
		return ""
	}
}
