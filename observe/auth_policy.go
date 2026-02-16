// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

// Principal policy query types: wire types for querying one side of the
// authorization model for a principal. GrantsRequest/Response queries the
// subject side (what the principal can do), AllowancesRequest/Response
// queries the target side (who can act on the principal).

import (
	"encoding/json"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/schema"
)

// GrantsRequest is sent to the daemon's observe socket to retrieve the
// complete resolved grant and denial policy for a principal. This shows
// what the principal is authorized to do (grants) and what it is
// explicitly blocked from doing (denials).
//
// Authentication is mandatory: both Observer and Token must be present.
type GrantsRequest struct {
	// Action must be "query_grants".
	Action string `json:"action"`

	// Principal is the localpart to query grants for
	// (e.g., "iree/amdgpu/pm").
	Principal string `json:"principal"`

	// Observer is the Matrix user ID of the entity making this request.
	Observer string `json:"observer"`

	// Token is a Matrix access token proving the Observer's identity.
	Token string `json:"token"`
}

// GrantsResponse is the daemon's response to a GrantsRequest.
type GrantsResponse struct {
	// OK is true if the query was processed successfully.
	OK bool `json:"ok"`

	// Error describes why the query failed. Only set when OK is false.
	Error string `json:"error,omitempty"`

	// Principal is the localpart that was queried.
	Principal string `json:"principal"`

	// Grants is the complete list of resolved grants for the principal.
	Grants []schema.Grant `json:"grants"`

	// Denials is the complete list of resolved denials for the principal.
	Denials []schema.Denial `json:"denials"`
}

// QueryGrants connects to the daemon's observe socket and retrieves the
// complete resolved grant and denial policy for a principal.
//
// daemonSocket is the path to the daemon's observation unix socket
// (typically DefaultDaemonSocket). The caller must set Principal on the
// request; the Action field is set automatically.
func QueryGrants(daemonSocket string, request GrantsRequest) (*GrantsResponse, error) {
	request.Action = "query_grants"
	data, err := queryDaemonRaw(daemonSocket, request)
	if err != nil {
		return nil, err
	}

	var response GrantsResponse
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, fmt.Errorf("unmarshal grants response: %w", err)
	}

	if !response.OK {
		return nil, fmt.Errorf("query grants: %s", response.Error)
	}

	return &response, nil
}

// AllowancesRequest is sent to the daemon's observe socket to retrieve
// the complete resolved allowance and allowance denial policy for a
// principal. This shows who is permitted to act on the principal
// (allowances) and who is explicitly blocked (allowance denials).
//
// Authentication is mandatory: both Observer and Token must be present.
type AllowancesRequest struct {
	// Action must be "query_allowances".
	Action string `json:"action"`

	// Principal is the localpart to query allowances for
	// (e.g., "iree/amdgpu/pm").
	Principal string `json:"principal"`

	// Observer is the Matrix user ID of the entity making this request.
	Observer string `json:"observer"`

	// Token is a Matrix access token proving the Observer's identity.
	Token string `json:"token"`
}

// AllowancesResponse is the daemon's response to an AllowancesRequest.
type AllowancesResponse struct {
	// OK is true if the query was processed successfully.
	OK bool `json:"ok"`

	// Error describes why the query failed. Only set when OK is false.
	Error string `json:"error,omitempty"`

	// Principal is the localpart that was queried.
	Principal string `json:"principal"`

	// Allowances is the complete list of resolved allowances for the
	// principal, showing who can act on it.
	Allowances []schema.Allowance `json:"allowances"`

	// AllowanceDenials is the complete list of resolved allowance
	// denials for the principal, showing who is explicitly blocked.
	AllowanceDenials []schema.AllowanceDenial `json:"allowance_denials"`
}

// QueryAllowances connects to the daemon's observe socket and retrieves
// the complete resolved allowance and allowance denial policy for a
// principal.
//
// daemonSocket is the path to the daemon's observation unix socket
// (typically DefaultDaemonSocket). The caller must set Principal on the
// request; the Action field is set automatically.
func QueryAllowances(daemonSocket string, request AllowancesRequest) (*AllowancesResponse, error) {
	request.Action = "query_allowances"
	data, err := queryDaemonRaw(daemonSocket, request)
	if err != nil {
		return nil, err
	}

	var response AllowancesResponse
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, fmt.Errorf("unmarshal allowances response: %w", err)
	}

	if !response.OK {
		return nil, fmt.Errorf("query allowances: %s", response.Error)
	}

	return &response, nil
}
