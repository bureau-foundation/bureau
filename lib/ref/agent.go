// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

//nolint:dupl // Machine, Service, and Agent are structurally identical by design — distinct types for compile-time safety.
package ref

import "fmt"

// Agent identifies an agent entity within a fleet. Agents are sandboxed
// processes that perform work on behalf of users or automation
// (e.g., "code-reviewer", "sysadmin").
//
// Agent embeds an unexported entity type, which provides all
// accessor methods. See Machine for the full method list.
type Agent struct{ entity }

// NewAgent creates a validated Agent reference.
func NewAgent(fleet Fleet, name string) (Agent, error) {
	ent, err := newEntity(fleet, entityTypeAgent, name)
	if err != nil {
		return Agent{}, err
	}
	return Agent{entity: ent}, nil
}

// ParseAgentUserID parses a full Matrix user ID into an Agent.
func ParseAgentUserID(userID string) (Agent, error) {
	ent, err := parseEntityUserID(userID, entityTypeAgent)
	if err != nil {
		return Agent{}, err
	}
	return Agent{entity: ent}, nil
}

// ParseAgent parses a fleet-scoped localpart and server into an Agent.
func ParseAgent(localpart string, server ServerName) (Agent, error) {
	ent, err := parseEntityLocalpart(localpart, server, entityTypeAgent)
	if err != nil {
		return Agent{}, err
	}
	return Agent{entity: ent}, nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (a *Agent) UnmarshalText(data []byte) error {
	parsed, err := ParseAgentUserID(string(data))
	if err != nil {
		return fmt.Errorf("unmarshal Agent: %w", err)
	}
	*a = parsed
	return nil
}
