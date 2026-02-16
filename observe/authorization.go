// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import (
	"encoding/json"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/schema"
)

// AuthorizationRequest is sent to the daemon's observe socket to evaluate
// an authorization check and return the full evaluation trace. The daemon
// checks whether actor can perform auth_action on target using the live
// authorization index, and returns the decision along with the matched
// rules and complete policy context for both sides.
//
// Authentication is mandatory: both Observer and Token must be present.
// The daemon verifies the token against the homeserver before processing.
type AuthorizationRequest struct {
	// Action must be "query_authorization".
	Action string `json:"action"`

	// Actor is the localpart of the principal performing the action
	// (e.g., "iree/amdgpu/pm").
	Actor string `json:"actor"`

	// AuthAction is the action to check (e.g., "observe",
	// "observe/read-write", "ticket/create", "command/pipeline/list").
	// Named "auth_action" in JSON to avoid collision with the dispatch
	// Action field.
	AuthAction string `json:"auth_action"`

	// Target is the localpart of the target principal for cross-principal
	// actions. Empty for self-service actions (e.g., matrix/join,
	// command/pipeline/list).
	Target string `json:"target,omitempty"`

	// Observer is the Matrix user ID of the entity making this request.
	Observer string `json:"observer"`

	// Token is a Matrix access token proving the Observer's identity.
	Token string `json:"token"`
}

// AuthorizationResponse is the daemon's response to an AuthorizationRequest.
// It contains the evaluation decision, the specific rules that matched
// during evaluation, and the complete policy context for both actor and
// target.
type AuthorizationResponse struct {
	// OK is true if the query was processed successfully. A "deny"
	// decision is still OK=true — OK indicates the query itself
	// succeeded, not the authorization outcome.
	OK bool `json:"ok"`

	// Error describes why the query failed. Only set when OK is false.
	Error string `json:"error,omitempty"`

	// Decision is "allow" or "deny".
	Decision string `json:"decision"`

	// Reason is a human-readable explanation for a deny decision.
	// Empty when Decision is "allow".
	Reason string `json:"reason,omitempty"`

	// MatchedGrant is the grant that matched during evaluation, if any.
	// Nil when denied at the grant stage (no matching grant found).
	MatchedGrant *schema.Grant `json:"matched_grant,omitempty"`

	// MatchedDenial is the denial that fired, if any. Only set when
	// the decision was deny due to an explicit denial.
	MatchedDenial *schema.Denial `json:"matched_denial,omitempty"`

	// MatchedAllowance is the allowance that matched on the target,
	// if any. Nil for self-service actions or when denied at the
	// allowance stage.
	MatchedAllowance *schema.Allowance `json:"matched_allowance,omitempty"`

	// MatchedAllowanceDenial is the allowance denial that fired on
	// the target, if any. Only set when the decision was deny due
	// to an explicit allowance denial.
	MatchedAllowanceDenial *schema.AllowanceDenial `json:"matched_allowance_denial,omitempty"`

	// ActorGrants is the complete list of resolved grants for the actor.
	ActorGrants []schema.Grant `json:"actor_grants"`

	// ActorDenials is the complete list of resolved denials for the actor.
	ActorDenials []schema.Denial `json:"actor_denials"`

	// TargetAllowances is the complete list of resolved allowances for
	// the target. Empty for self-service actions.
	TargetAllowances []schema.Allowance `json:"target_allowances,omitempty"`

	// TargetAllowanceDenials is the complete list of resolved allowance
	// denials for the target. Empty for self-service actions.
	TargetAllowanceDenials []schema.AllowanceDenial `json:"target_allowance_denials,omitempty"`
}

// QueryAuthorization connects to the daemon's observe socket and evaluates
// an authorization check. Returns the full evaluation trace including the
// decision, matched rules, and complete policy context.
//
// daemonSocket is the path to the daemon's observation unix socket
// (typically DefaultDaemonSocket). The caller must set Actor, AuthAction,
// and optionally Target on the request; the Action field is set
// automatically. Observer and Token are set by the caller (operator CLI)
// or injected by the proxy (sandboxed agents).
func QueryAuthorization(daemonSocket string, request AuthorizationRequest) (*AuthorizationResponse, error) {
	request.Action = "query_authorization"
	data, err := queryDaemonRaw(daemonSocket, request)
	if err != nil {
		return nil, err
	}

	var response AuthorizationResponse
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, fmt.Errorf("unmarshal authorization response: %w", err)
	}

	if !response.OK {
		return nil, fmt.Errorf("query authorization: %s", response.Error)
	}

	return &response, nil
}
