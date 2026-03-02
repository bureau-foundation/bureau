// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/forge"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

// autoSubscribeConfigRequest is the decoded request body for the
// "github/auto-subscribe-config" action. When Write is true, the
// provided Rules are published as an m.bureau.forge_auto_subscribe
// state event. When Write is false (read mode), the current rules
// for the requesting agent are returned.
type autoSubscribeConfigRequest struct {
	Write bool                          `cbor:"write,omitempty"`
	Rules forge.ForgeAutoSubscribeRules `cbor:"rules,omitempty"`

	// Room specifies the room where the auto-subscribe state event
	// lives. Required for write operations (the service publishes
	// the state event to this room).
	Room string `cbor:"room,omitempty"`
}

// autoSubscribeConfigResponse is the response body.
type autoSubscribeConfigResponse struct {
	Rules forge.ForgeAutoSubscribeRules `cbor:"rules"`
}

// handleAutoSubscribeConfig lets agents read or write their
// auto-subscribe rules. Read returns the current in-memory rules
// (from /sync ingestion). Write publishes an
// m.bureau.forge_auto_subscribe state event via the Matrix session.
func (gs *GitHubService) handleAutoSubscribeConfig(
	ctx context.Context,
	token *servicetoken.Token,
	raw []byte,
) (any, error) {
	action := forge.ProviderAction(forge.ProviderGitHub, forge.ActionAutoSubscribeConfig)
	if !servicetoken.GrantsAllow(token.Grants, action, "") {
		return nil, fmt.Errorf("access denied: missing grant for %s", action)
	}

	var request autoSubscribeConfigRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	agentLocalpart := token.Subject.Localpart()

	if !request.Write {
		// Read mode: return current rules from the Manager's index.
		// If no explicit rules exist, return defaults.
		rules := gs.manager.AutoSubscribeRules(agentLocalpart)
		return autoSubscribeConfigResponse{Rules: rules}, nil
	}

	// Write mode: validate and publish the state event.
	if request.Room == "" {
		return nil, fmt.Errorf("room is required for write operations")
	}
	if err := request.Rules.Validate(); err != nil {
		return nil, fmt.Errorf("invalid rules: %w", err)
	}

	roomID, err := ref.ParseRoomID(request.Room)
	if err != nil {
		return nil, fmt.Errorf("invalid room ID: %w", err)
	}

	stateKey := agentLocalpart
	_, err = gs.session.SendStateEvent(
		ctx, roomID, forge.EventTypeForgeAutoSubscribe, stateKey, request.Rules)
	if err != nil {
		return nil, fmt.Errorf("publish auto-subscribe rules: %w", err)
	}

	// Update the in-memory index immediately (the /sync will also
	// deliver this event, but updating now avoids a delay).
	gs.manager.UpdateAutoSubscribeRules(agentLocalpart, request.Rules)

	return autoSubscribeConfigResponse{Rules: request.Rules}, nil
}
