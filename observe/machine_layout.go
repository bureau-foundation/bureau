// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"sort"

	"github.com/bureau-foundation/bureau/lib/schema/observation"
)

// MachineLayoutRequest is sent to the daemon's observe socket to request
// a dynamically generated layout showing all observable principals running
// on the local machine. Unlike QueryLayoutRequest, no channel or room is
// involved — the daemon generates the layout from its live state.
//
// Authentication is mandatory: both Observer and Token must be present.
// The returned layout is filtered to only principals the observer is
// authorized to observe.
type MachineLayoutRequest struct {
	// Action must be "query_machine_layout".
	Action string `json:"action"`

	// Observer is the Matrix user ID of the entity requesting the layout.
	Observer string `json:"observer"`

	// Token is a Matrix access token that authenticates the Observer.
	Token string `json:"token"`
}

// QueryMachineLayout connects to the daemon's observe socket and requests
// the machine's auto-generated layout. The daemon collects all running
// principals, filters by the observer's authorization, and returns a
// layout with one observe pane per authorized principal.
//
// daemonSocket is the path to the daemon's observation unix socket
// (typically DefaultDaemonSocket). The caller must set Observer and Token
// on the request; the Action field is set automatically.
//
// The response includes a Machine field with the daemon's machine name,
// used by the CLI for session naming.
func QueryMachineLayout(daemonSocket string, request MachineLayoutRequest) (*QueryLayoutResponse, error) {
	connection, err := net.Dial("unix", daemonSocket)
	if err != nil {
		return nil, fmt.Errorf("dial daemon socket %s: %w", daemonSocket, err)
	}
	defer connection.Close()

	request.Action = "query_machine_layout"
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return nil, fmt.Errorf("send query_machine_layout request: %w", err)
	}

	reader := bufio.NewReader(connection)
	responseLine, err := reader.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read query_machine_layout response: %w", err)
	}

	var response QueryLayoutResponse
	if err := json.Unmarshal(responseLine, &response); err != nil {
		return nil, fmt.Errorf("unmarshal query_machine_layout response: %w", err)
	}

	if !response.OK {
		return nil, fmt.Errorf("query machine layout: %s", response.Error)
	}

	if response.Layout == nil {
		return nil, fmt.Errorf("daemon returned empty machine layout")
	}

	return &response, nil
}

// GenerateMachineLayout builds a default observation layout for a machine.
// Creates a single window named after the machine with one observe pane
// per principal, sorted alphabetically for deterministic output, stacked
// vertically.
//
// Returns nil if principals is empty — callers should check for this and
// return an appropriate error (the daemon returns "no observable principals
// running on this machine").
func GenerateMachineLayout(machineName string, principals []string) *Layout {
	if len(principals) == 0 {
		return nil
	}

	sorted := make([]string, len(principals))
	copy(sorted, principals)
	sort.Strings(sorted)

	panes := make([]Pane, len(sorted))
	for index, principal := range sorted {
		pane := Pane{
			Observe: principal,
		}
		// The first pane is the initial tmux pane (no split needed).
		// Subsequent panes split vertically to stack below.
		if index > 0 {
			pane.Split = observation.SplitVertical
		}
		panes[index] = pane
	}

	return &Layout{
		Windows: []Window{
			{
				Name:  machineName,
				Panes: panes,
			},
		},
	}
}
