// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
)

// QueryLayoutRequest is sent to the daemon's observe socket to request
// a channel's expanded layout. The daemon resolves the channel alias,
// fetches the m.bureau.layout state event, fetches room membership,
// expands ObserveMembers panes into concrete Observe panes, and returns
// the result.
//
// The Action field distinguishes this from an ObserveRequest, which
// establishes a streaming observation session. Both share the same
// socket and initial JSON line protocol.
//
// Authentication is mandatory: both Observer and Token must be present.
type QueryLayoutRequest struct {
	// Action must be "query_layout".
	Action string `json:"action"`

	// Channel is the Matrix room alias for the channel to query
	// (e.g., "#iree/amdgpu/general:bureau.local"). The daemon
	// resolves this to a room ID internally.
	Channel string `json:"channel"`

	// Observer is the Matrix user ID of the entity requesting the layout.
	Observer string `json:"observer"`

	// Token is a Matrix access token that authenticates the Observer.
	Token string `json:"token"`
}

// QueryLayoutResponse is the daemon's response to a QueryLayoutRequest
// or MachineLayoutRequest. On success, Layout contains the fully expanded
// layout (ObserveMembers panes replaced with concrete Observe panes). On
// failure, OK is false and Error describes the problem.
//
// After sending this response, the daemon closes the connection. There
// is no streaming phase — this is pure request/response.
type QueryLayoutResponse struct {
	// OK is true if the layout was successfully fetched and expanded.
	OK bool `json:"ok"`

	// Layout is the expanded layout ready for Dashboard(). Only set
	// when OK is true.
	Layout *Layout `json:"layout,omitempty"`

	// Machine is the localpart of the machine that generated this layout
	// (e.g., "machine/workstation"). Set by query_machine_layout responses
	// for session naming; empty for query_layout (channel) responses.
	Machine string `json:"machine,omitempty"`

	// Error describes why the request failed. Only set when OK is false.
	Error string `json:"error,omitempty"`
}

// queryDaemonRaw connects to the daemon's observe socket, sends the
// JSON-encoded request, and returns the raw response line. This is the
// shared transport layer for all observe query functions — each caller
// sets the action on its typed request struct before passing it here,
// then unmarshals the returned bytes into its own response type.
func queryDaemonRaw(daemonSocket string, request any) ([]byte, error) {
	connection, err := net.Dial("unix", daemonSocket)
	if err != nil {
		return nil, fmt.Errorf("dial daemon socket %s: %w", daemonSocket, err)
	}
	defer connection.Close()

	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return nil, fmt.Errorf("send observe request: %w", err)
	}

	reader := bufio.NewReader(connection)
	responseLine, err := reader.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read observe response: %w", err)
	}

	return responseLine, nil
}

// QueryLayout connects to the daemon's observe socket and requests the
// expanded layout for a channel. The daemon resolves the channel alias
// to a room ID, fetches the m.bureau.layout state event, fetches room
// membership, and returns the layout with ObserveMembers panes expanded
// into concrete Observe panes.
//
// daemonSocket is the path to the daemon's observation unix socket
// (typically DefaultDaemonSocket). The caller must set Channel, Observer,
// and Token on the request; the Action field is set automatically.
func QueryLayout(daemonSocket string, request QueryLayoutRequest) (*Layout, error) {
	request.Action = "query_layout"
	data, err := queryDaemonRaw(daemonSocket, request)
	if err != nil {
		return nil, err
	}

	var response QueryLayoutResponse
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, fmt.Errorf("unmarshal query_layout response: %w", err)
	}

	if !response.OK {
		return nil, fmt.Errorf("query layout for %s: %s", request.Channel, response.Error)
	}

	if response.Layout == nil {
		return nil, fmt.Errorf("daemon returned empty layout for %s", request.Channel)
	}

	return response.Layout, nil
}
