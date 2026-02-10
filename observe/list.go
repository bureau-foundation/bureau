// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
)

// ListRequest is sent to the daemon's observe socket to request a list
// of observable targets. The daemon returns all known principals and
// machines, optionally filtered to only those currently observable.
//
// The Action field distinguishes this from an ObserveRequest or
// QueryLayoutRequest on the same socket.
//
// Authentication is mandatory: both Observer and Token must be present.
// The daemon verifies the token and filters the returned list to only
// principals the observer is authorized to see.
type ListRequest struct {
	// Action must be "list".
	Action string `json:"action"`

	// Observable, when true, filters the response to only principals
	// that can currently be observed (running locally, or running on
	// a remote machine reachable via transport).
	Observable bool `json:"observable,omitempty"`

	// Observer is the Matrix user ID of the entity requesting the list.
	Observer string `json:"observer"`

	// Token is a Matrix access token that authenticates the Observer.
	Token string `json:"token"`
}

// ListResponse is the daemon's response to a ListRequest.
type ListResponse struct {
	// OK is true if the list was successfully generated.
	OK bool `json:"ok"`

	// Principals lists known principals. Each has enough metadata to
	// determine observability and location.
	Principals []ListPrincipal `json:"principals,omitempty"`

	// Machines lists known machines in the fleet.
	Machines []ListMachine `json:"machines,omitempty"`

	// Error describes why the request failed. Only set when OK is false.
	Error string `json:"error,omitempty"`
}

// ListPrincipal describes a principal known to the daemon.
type ListPrincipal struct {
	// Localpart is the principal's hierarchical name
	// (e.g., "iree/amdgpu/pm", "service/stt/whisper").
	Localpart string `json:"localpart"`

	// Machine is the machine localpart hosting this principal
	// (e.g., "machine/workstation").
	Machine string `json:"machine"`

	// Observable is true when the principal can be observed right now:
	// running locally, or on a remote machine with a reachable transport.
	Observable bool `json:"observable"`

	// Local is true when the principal is running on this machine.
	Local bool `json:"local"`
}

// ListMachine describes a machine known to the daemon.
type ListMachine struct {
	// Name is the machine localpart (e.g., "machine/workstation").
	Name string `json:"name"`

	// UserID is the full Matrix user ID
	// (e.g., "@machine/workstation:bureau.local").
	UserID string `json:"user_id"`

	// Self is true when this is the local machine.
	Self bool `json:"self"`

	// Reachable is true when the machine has a known transport address.
	// Always true for Self.
	Reachable bool `json:"reachable"`
}

// ListTargets connects to the daemon's observe socket and requests a
// list of known principals and machines. When observable is true, the
// daemon filters to only currently-observable targets.
//
// daemonSocket is the path to the daemon's observation unix socket
// (typically DefaultDaemonSocket).
func ListTargets(daemonSocket string, observable bool) (*ListResponse, error) {
	connection, err := net.Dial("unix", daemonSocket)
	if err != nil {
		return nil, fmt.Errorf("dial daemon socket %s: %w", daemonSocket, err)
	}
	defer connection.Close()

	if err := json.NewEncoder(connection).Encode(ListRequest{
		Action:     "list",
		Observable: observable,
	}); err != nil {
		return nil, fmt.Errorf("send list request: %w", err)
	}

	reader := bufio.NewReader(connection)
	responseLine, err := reader.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read list response: %w", err)
	}

	var response ListResponse
	if err := json.Unmarshal(responseLine, &response); err != nil {
		return nil, fmt.Errorf("unmarshal list response: %w", err)
	}

	if !response.OK {
		return nil, fmt.Errorf("list targets: %s", response.Error)
	}

	return &response, nil
}
