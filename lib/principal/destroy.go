// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package principal

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// DestroyResult holds the result of removing a principal assignment.
type DestroyResult struct {
	// ConfigEventID is the event ID of the updated MachineConfig state
	// event after removing the principal.
	ConfigEventID string
}

// Destroy removes a principal's assignment from the MachineConfig.
// Reads the current config, removes the matching PrincipalAssignment,
// and publishes the updated config. Returns an error if the principal
// is not found in the config.
//
// The daemon detects the config change via /sync and tears down the
// principal's sandbox. The principal's Matrix account is preserved
// for audit trail purposes.
func Destroy(ctx context.Context, session messaging.Session, configRoomID string, machine ref.Machine, localpart string) (*DestroyResult, error) {
	configRaw, err := session.GetStateEvent(ctx, configRoomID, schema.EventTypeMachineConfig, machine.Localpart())
	if err != nil {
		return nil, fmt.Errorf("read machine config for %s: %w", machine.Localpart(), err)
	}

	var config schema.MachineConfig
	if err := json.Unmarshal(configRaw, &config); err != nil {
		return nil, fmt.Errorf("parse machine config for %s: %w", machine.Localpart(), err)
	}

	found := false
	filtered := make([]schema.PrincipalAssignment, 0, len(config.Principals))
	for _, assignment := range config.Principals {
		if assignment.Principal.Localpart() == localpart {
			found = true
			continue
		}
		filtered = append(filtered, assignment)
	}
	if !found {
		return nil, fmt.Errorf("principal %q not found in machine config for %s", localpart, machine.Localpart())
	}
	config.Principals = filtered

	eventID, err := session.SendStateEvent(ctx, configRoomID, schema.EventTypeMachineConfig, machine.Localpart(), config)
	if err != nil {
		return nil, fmt.Errorf("publish machine config for %s: %w", machine.Localpart(), err)
	}

	return &DestroyResult{ConfigEventID: eventID}, nil
}
