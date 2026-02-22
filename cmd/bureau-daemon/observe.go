// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Observation routing: the daemon routes observation requests from clients
// (bureau observe CLI) to relay processes that attach to tmux sessions. For
// local principals, the daemon forks a relay directly. For remote principals,
// it forwards the request through the transport to the remote daemon.
//
// The observation protocol has two phases:
//   - Handshake: client sends observeRequest JSON, daemon sends observeResponse JSON
//   - Streaming: after success, the socket carries the binary observation protocol
//     (framed messages: data, resize, history, metadata). The daemon bridges bytes
//     zero-copy between the client and the relay — no parsing of protocol messages.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bureau-foundation/bureau/lib/netutil"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/observe"
)

// observeRequest mirrors observe.ObserveRequest. Defined locally to parallel
// the existing pattern with launcherIPCRequest. The observation protocol uses
// JSON (user-facing CLI boundary), unlike the launcher IPC which uses CBOR.
type observeRequest struct {
	// Action selects the request type:
	//   - "" or "observe": streaming observation session
	//   - "query_layout": fetch and expand a channel's layout
	//   - "query_machine_layout": generate layout from running principals
	//   - "list": list known principals and machines
	Action string `json:"action,omitempty"`

	// Principal is the localpart of the target (e.g., "iree/amdgpu/pm").
	// Used when Action is "observe" (or empty).
	Principal string `json:"principal,omitempty"`

	// Mode is "readwrite" or "readonly".
	// Used when Action is "observe" (or empty).
	Mode string `json:"mode,omitempty"`

	// Channel is the Matrix room alias (e.g., "#iree/amdgpu/general:bureau.local").
	// Used when Action is "query_layout".
	Channel string `json:"channel,omitempty"`

	// Observable, when true with Action "list", filters the response
	// to only currently-observable targets.
	Observable bool `json:"observable,omitempty"`

	// Actor is the full Matrix user ID of the acting principal for
	// authorization queries. Used when Action is "query_authorization".
	Actor string `json:"actor,omitempty"`

	// AuthAction is the action to check for authorization queries
	// (e.g., "observe", "ticket/create"). Named "auth_action" in JSON
	// to avoid collision with the dispatch Action field.
	// Used when Action is "query_authorization".
	AuthAction string `json:"auth_action,omitempty"`

	// Target is the full Matrix user ID of the target principal for
	// authorization queries. Empty for self-service actions.
	// Used when Action is "query_authorization".
	Target string `json:"target,omitempty"`

	// Observer is the Matrix user ID of the entity making this request.
	// Required for all actions — the daemon verifies this via Token.
	Observer string `json:"observer,omitempty"`

	// Token is a Matrix access token proving the Observer's identity.
	// The daemon verifies this against the homeserver before processing
	// any request.
	Token string `json:"token,omitempty"`
}

// observeResponse mirrors observe.ObserveResponse. Defined locally to
// parallel the existing pattern with launcherIPCResponse.
type observeResponse struct {
	// OK is true if the observation session was established.
	OK bool `json:"ok"`

	// Session is the tmux session name (e.g., "bureau/iree/amdgpu/pm").
	Session string `json:"session,omitempty"`

	// Machine is the machine localpart hosting the principal.
	Machine string `json:"machine,omitempty"`

	// GrantedMode is the observation mode actually granted by the
	// daemon. May be "readonly" even if "readwrite" was requested,
	// when the target's allowances only permit readonly observation.
	GrantedMode string `json:"granted_mode,omitempty"`

	// Error describes why the request failed.
	Error string `json:"error,omitempty"`
}

// activeObserveSession tracks a single in-flight observation connection.
// The daemon maintains a registry of these so it can enforce allowance
// changes: when observation allowances tighten, sessions that no longer
// pass authorization are terminated by closing their client connection.
type activeObserveSession struct {
	principal   ref.UserID
	observer    ref.UserID
	grantedMode string
	clientConn  net.Conn
}

// registerObserveSession adds a session to the daemon's active session
// registry. Called after the handshake succeeds, before bridgeConnections.
func (d *Daemon) registerObserveSession(session *activeObserveSession) {
	d.observeSessionsMu.Lock()
	defer d.observeSessionsMu.Unlock()
	d.observeSessions = append(d.observeSessions, session)
}

// unregisterObserveSession removes a session from the registry. Called
// when bridgeConnections returns (one side disconnected or was closed).
func (d *Daemon) unregisterObserveSession(session *activeObserveSession) {
	d.observeSessionsMu.Lock()
	defer d.observeSessionsMu.Unlock()
	for i, candidate := range d.observeSessions {
		if candidate == session {
			d.observeSessions = append(d.observeSessions[:i], d.observeSessions[i+1:]...)
			return
		}
	}
}

// enforceObserveAllowanceChange re-evaluates all active observation sessions
// for a principal against the current allowances in the authorization
// index. Sessions that no longer pass authorization are terminated by
// closing the client connection, which unblocks bridgeConnections and
// triggers relay cleanup.
func (d *Daemon) enforceObserveAllowanceChange(principalUserID ref.UserID) {
	d.observeSessionsMu.Lock()
	defer d.observeSessionsMu.Unlock()

	for _, session := range d.observeSessions {
		if session.principal != principalUserID {
			continue
		}

		authz := d.authorizeObserve(session.observer, session.principal, session.grantedMode)
		if authz.Allowed && authz.GrantedMode == session.grantedMode {
			continue
		}

		// Policy no longer allows this session at its current mode.
		// Close the client connection to terminate the relay bridge.
		d.logger.Info("terminating observation session: policy changed",
			"observer", session.observer,
			"principal", session.principal,
			"mode", session.grantedMode,
			"still_allowed", authz.Allowed,
		)
		session.clientConn.Close()
	}
}

// startObserveListener creates the observation Unix socket and starts
// accepting client connections in a goroutine.
func (d *Daemon) startObserveListener(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(d.observeSocketPath), 0755); err != nil {
		return fmt.Errorf("creating observe socket directory: %w", err)
	}

	// Remove stale socket from a previous run.
	if err := os.Remove(d.observeSocketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing existing observe socket: %w", err)
	}

	listener, err := net.Listen("unix", d.observeSocketPath)
	if err != nil {
		return fmt.Errorf("creating observe socket at %s: %w", d.observeSocketPath, err)
	}
	d.observeListener = listener

	if err := os.Chmod(d.observeSocketPath, 0660); err != nil {
		listener.Close()
		return fmt.Errorf("setting observe socket permissions: %w", err)
	}

	d.logger.Info("observe listener started", "socket", d.observeSocketPath)

	go d.acceptObserveConnections(ctx)
	return nil
}

// stopObserveListener closes the observation socket and removes the file.
func (d *Daemon) stopObserveListener() {
	if d.observeListener != nil {
		d.observeListener.Close()
		os.Remove(d.observeSocketPath)
	}
}

// acceptObserveConnections runs the accept loop for the observation socket.
// Each connection is handled in its own goroutine.
func (d *Daemon) acceptObserveConnections(ctx context.Context) {
	for {
		connection, err := d.observeListener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				if !strings.Contains(err.Error(), "use of closed network connection") {
					d.logger.Error("accept observe connection", "error", err)
				}
				return
			}
		}
		go d.handleObserveClient(connection)
	}
}

// handleObserveClient processes a single request on the observe socket.
// It reads the initial JSON line, authenticates the observer, dispatches
// on the action field, and either establishes a streaming observation
// session or handles a request/response query.
//
// Every request must include a valid Matrix access token. The daemon verifies
// the token against the homeserver (via the cached tokenVerifier) before
// processing any action.
func (d *Daemon) handleObserveClient(clientConnection net.Conn) {
	defer clientConnection.Close()

	// Set a deadline for the JSON handshake. Cleared before entering
	// the streaming bridge (for observe) or left in place for queries.
	clientConnection.SetDeadline(time.Now().Add(10 * time.Second))

	var request observeRequest
	if err := json.NewDecoder(clientConnection).Decode(&request); err != nil {
		d.sendObserveError(clientConnection, fmt.Sprintf("invalid request: %v", err))
		return
	}

	// Authenticate the observer. Every action requires a valid token.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	observer, err := d.authenticateObserver(ctx, request.Token)
	if err != nil {
		d.logger.Warn("observe authentication failed",
			"asserted_observer", request.Observer,
			"action", request.Action,
			"remote_addr", clientConnection.RemoteAddr().String(),
		)
		d.sendObserveError(clientConnection, err.Error())
		return
	}

	// Replace the observer field with the verified identity. The client
	// may have sent a user ID, but the daemon trusts only the token
	// verification result.
	request.Observer = observer.String()

	d.logger.Debug("observer authenticated", "observer", observer)

	switch request.Action {
	case "", "observe":
		d.handleObserveSession(clientConnection, request)
	case "query_layout":
		d.handleQueryLayout(clientConnection, request)
	case "query_machine_layout":
		d.handleMachineLayout(clientConnection, request)
	case "list":
		d.handleList(clientConnection, request)
	case "query_authorization":
		d.handleQueryAuthorization(clientConnection, request)
	case "query_grants", "query_allowances":
		d.handleQueryPrincipalPolicy(clientConnection, request)
	default:
		d.sendObserveError(clientConnection,
			fmt.Sprintf("unknown action %q", request.Action))
	}
}

// handleObserveSession handles a streaming observation request. It validates
// the principal, determines whether it is local or remote, and either
// authorizes locally and forks a relay, or forwards through the transport
// to the hosting machine.
//
// Authorization model: only the hosting machine authorizes observation.
// For local principals, this daemon checks the authorization index. For
// remote principals, this daemon authenticates the observer (already done
// by handleObserveClient) and forwards — the provider daemon authorizes
// via handleTransportObserve. This avoids dual-authorization where the
// consumer would need to duplicate the provider's allowance policy.
//
// The observer identity in request.Observer is already verified by
// handleObserveClient before this method is called.
func (d *Daemon) handleObserveSession(clientConnection net.Conn, request observeRequest) {
	if err := principal.ValidateLocalpart(request.Principal); err != nil {
		d.sendObserveError(clientConnection, fmt.Sprintf("invalid principal: %v", err))
		return
	}

	if request.Mode != "readwrite" && request.Mode != "readonly" {
		d.sendObserveError(clientConnection,
			fmt.Sprintf("invalid mode %q: must be readwrite or readonly", request.Mode))
		return
	}

	// Construct a fleet-scoped Entity from the request's account
	// localpart to look up in the Entity-keyed d.running map.
	principalEntity, err := ref.NewEntityFromAccountLocalpart(d.fleet, request.Principal)
	if err != nil {
		d.sendObserveError(clientConnection,
			fmt.Sprintf("invalid principal %q: %v", request.Principal, err))
		return
	}

	// Check if the principal is running locally. Snapshot under RLock
	// to avoid racing with background goroutines (watchSandboxExit,
	// rollbackPrincipal) that modify d.running under Lock.
	d.reconcileMu.RLock()
	isLocallyRunning := d.running[principalEntity]
	d.reconcileMu.RUnlock()

	if isLocallyRunning {
		// Parse the verified observer identity for authorization checks.
		observer, parseErr := ref.ParseUserID(request.Observer)
		if parseErr != nil {
			d.sendObserveError(clientConnection,
				fmt.Sprintf("invalid observer identity %q: %v", request.Observer, parseErr))
			return
		}

		// Authorize the observer against the principal's allowances.
		// Only the hosting machine authorizes — this IS the host.
		requestedMode := request.Mode
		authz := d.authorizeObserve(observer, principalEntity.UserID(), requestedMode)
		if !authz.Allowed {
			d.logger.Warn("observation denied",
				"observer", request.Observer,
				"principal", request.Principal,
				"requested_mode", requestedMode,
			)
			d.sendObserveError(clientConnection,
				fmt.Sprintf("not authorized to observe %q — the principal's authorization policy has no observe allowance matching your identity", request.Principal))
			return
		}

		// Enforce the granted mode. The daemon decides the mode, not the client.
		request.Mode = authz.GrantedMode

		if requestedMode != authz.GrantedMode {
			d.logger.Info("observation mode downgraded",
				"observer", request.Observer,
				"principal", request.Principal,
				"requested_mode", requestedMode,
				"granted_mode", authz.GrantedMode,
			)
		}

		d.logger.Info("observation authorized",
			"observer", request.Observer,
			"principal", request.Principal,
			"requested_mode", requestedMode,
			"granted_mode", authz.GrantedMode,
		)

		d.handleLocalObserve(clientConnection, request, authz.GrantedMode)
		return
	}

	// Check if the principal is a known service on a remote machine
	// reachable via the transport. Authentication is already done;
	// the provider will authorize the forwarded request.
	if peerAddress, ok := d.findPrincipalPeer(request.Principal); ok {
		d.handleRemoteObserve(clientConnection, request, peerAddress)
		return
	}

	d.sendObserveError(clientConnection,
		fmt.Sprintf("principal %q not found on this machine or any reachable peer", request.Principal))
}

// handleQueryLayout handles a query_layout request: resolves a channel alias
// to a room ID, fetches the m.bureau.layout state event and room membership,
// expands ObserveMembers panes, and returns the concrete layout as JSON.
//
// Authentication is handled by handleObserveClient before dispatch. The
// layout is returned as-is — individual pane authorization happens when the
// client opens observation sessions for each pane's principal.
//
// This is pure request/response — the connection is closed after the response.
func (d *Daemon) handleQueryLayout(clientConnection net.Conn, request observeRequest) {
	if request.Channel == "" {
		d.sendObserveError(clientConnection, "channel is required for query_layout")
		return
	}

	// Use a generous timeout for the Matrix queries (alias resolution +
	// state event fetch + membership fetch).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	d.logger.Info("query_layout requested",
		"observer", request.Observer,
		"channel", request.Channel,
	)

	// Resolve the channel alias to a room ID.
	channelAlias, err := ref.ParseRoomAlias(request.Channel)
	if err != nil {
		d.sendObserveError(clientConnection,
			fmt.Sprintf("invalid channel alias %q: %v", request.Channel, err))
		return
	}
	roomID, err := d.session.ResolveAlias(ctx, channelAlias)
	if err != nil {
		d.sendObserveError(clientConnection,
			fmt.Sprintf("resolve channel %q: %v", request.Channel, err))
		return
	}

	// Fetch the m.bureau.layout state event from the channel room.
	// Channel-level layouts use an empty state key (as opposed to
	// per-principal layouts which use the principal's localpart).
	rawLayout, err := d.session.GetStateEvent(ctx, roomID, schema.EventTypeLayout, "")
	if err != nil {
		d.sendObserveError(clientConnection,
			fmt.Sprintf("fetch layout for %q: %v", request.Channel, err))
		return
	}

	var layoutContent schema.LayoutContent
	if err := json.Unmarshal(rawLayout, &layoutContent); err != nil {
		d.sendObserveError(clientConnection,
			fmt.Sprintf("parse layout for %q: %v", request.Channel, err))
		return
	}

	layout := observe.SchemaToLayout(layoutContent)

	// Fetch room members for ObserveMembers pane expansion.
	matrixMembers, err := d.session.GetRoomMembers(ctx, roomID)
	if err != nil {
		d.sendObserveError(clientConnection,
			fmt.Sprintf("fetch members for %q: %v", request.Channel, err))
		return
	}

	// Build a lookup table of labels from the local machine's config.
	// Cross-machine members won't have labels populated (their labels
	// are only known to their own machine's daemon), so label-based
	// filters only match principals the daemon has config for.
	principalLabels := make(map[string]map[string]string)
	if d.lastConfig != nil {
		for _, assignment := range d.lastConfig.Principals {
			if len(assignment.Labels) > 0 {
				principalLabels[assignment.Principal.AccountLocalpart()] = assignment.Labels
			}
		}
	}

	// Convert messaging.RoomMember to observe.RoomMember. Only include
	// joined members — invited, left, and banned members are excluded.
	//
	// Member user IDs may be fleet-scoped (e.g., @bureau/fleet/prod/agent/pm:server).
	// The principalLabels map and observe system use account localparts
	// (e.g., "agent/pm"). Extract the account localpart via ExtractEntityName
	// so labels match and the Observe pane field is compatible with
	// handleObserveSession's NewEntityFromAccountLocalpart. Non-fleet
	// members (e.g., @admin:bureau.local) fall back to the raw localpart.
	var observeMembers []observe.RoomMember
	for _, member := range matrixMembers {
		if member.Membership != "join" {
			continue
		}
		localpart, localpartErr := principal.LocalpartFromMatrixID(member.UserID.String())
		if localpartErr != nil {
			// Skip members whose user IDs don't match the Bureau
			// @localpart:server convention.
			continue
		}
		// Extract the account localpart from fleet-scoped localparts.
		// Falls back to the raw localpart for non-fleet members (e.g.,
		// "admin" from @admin:bureau.local).
		observeLocalpart := localpart
		if entityType, entityName, err := ref.ExtractEntityName(localpart); err == nil {
			observeLocalpart = entityType + "/" + entityName
		}
		observeMembers = append(observeMembers, observe.RoomMember{
			Localpart: observeLocalpart,
			Labels:    principalLabels[observeLocalpart],
		})
	}

	expanded := observe.ExpandMembers(layout, observeMembers)

	d.logger.Info("query_layout completed",
		"channel", request.Channel,
		"room_id", roomID,
		"members", len(observeMembers),
		"windows", len(expanded.Windows),
	)

	response := observe.QueryLayoutResponse{
		OK:     true,
		Layout: expanded,
	}
	json.NewEncoder(clientConnection).Encode(response)
}

// handleMachineLayout handles a query_machine_layout request: generates a
// layout from the principals currently running on this machine, filtered
// by the observer's authorization.
//
// This is the "show me everything on this machine" entry point — the
// daemon generates the layout dynamically from d.running rather than
// fetching a stored layout from Matrix.
//
// Authentication is handled by handleObserveClient before dispatch.
// This is pure request/response — the connection is closed after the response.
func (d *Daemon) handleMachineLayout(clientConnection net.Conn, request observeRequest) {
	d.logger.Info("query_machine_layout requested",
		"observer", request.Observer,
	)

	// Snapshot d.running under RLock to avoid racing with background
	// goroutines that modify the map.
	runningSnapshot := d.runningConsumers()

	// Parse the verified observer identity for authorization checks.
	observer, parseErr := ref.ParseUserID(request.Observer)
	if parseErr != nil {
		d.sendObserveError(clientConnection,
			fmt.Sprintf("invalid observer identity %q: %v", request.Observer, parseErr))
		return
	}

	// Collect running principals that the observer is authorized to see.
	var authorizedPrincipals []string
	for _, principal := range runningSnapshot {
		if d.authorizeList(observer, principal.UserID()) {
			authorizedPrincipals = append(authorizedPrincipals, principal.AccountLocalpart())
		}
	}

	layout := observe.GenerateMachineLayout(d.machine.Localpart(), authorizedPrincipals)
	if layout == nil {
		d.sendObserveError(clientConnection,
			"no observable principals running on this machine")
		return
	}

	d.logger.Info("query_machine_layout completed",
		"observer", request.Observer,
		"machine", d.machine.Localpart(),
		"principals", len(authorizedPrincipals),
	)

	json.NewEncoder(clientConnection).Encode(observe.QueryLayoutResponse{
		OK:      true,
		Layout:  layout,
		Machine: d.machine.Localpart(),
	})
}

// handleList handles a "list" request: enumerates all known principals and
// machines, returning each with observability and location metadata.
//
// Authentication is handled by handleObserveClient before dispatch. Local
// principals are filtered by the observer's authorization — only principals
// whose allowances permit the observer appear in the list. Remote service
// directory entries are included without local authorization filtering
// because this daemon does not have the remote principal's allowance
// policy; the hosting machine's daemon will authorize when an actual
// observation session is established.
//
// Principals come from two sources:
//   - d.running: principals with active sandboxes on this machine (always observable)
//   - d.services: service directory entries across all machines (observable when
//     local, or remote with a reachable transport address)
//
// Machines come from d.peerAddresses plus the local machine.
func (d *Daemon) handleList(clientConnection net.Conn, request observeRequest) {
	d.logger.Info("list requested",
		"observer", request.Observer,
		"observable", request.Observable,
	)

	// Snapshot d.running under RLock to avoid racing with background
	// goroutines that modify the map. Build a set for O(1) membership
	// checks against service directory entries below.
	d.reconcileMu.RLock()
	runningSet := make(map[string]bool, len(d.running))
	for principal := range d.running {
		runningSet[principal.AccountLocalpart()] = true
	}
	d.reconcileMu.RUnlock()

	// Collect principals. Use a map to deduplicate between running
	// principals and d.services (a locally running service appears in
	// both).
	seen := make(map[string]bool)
	var principals []observe.ListPrincipal

	// Parse the verified observer identity for authorization checks.
	observer, parseErr := ref.ParseUserID(request.Observer)
	if parseErr != nil {
		d.sendObserveError(clientConnection,
			fmt.Sprintf("invalid observer identity %q: %v", request.Observer, parseErr))
		return
	}

	// Locally running principals — filtered by authorization.
	for accountLocalpart := range runningSet {
		seen[accountLocalpart] = true

		// Construct a typed user ID for the authorization check.
		principalEntity, entityErr := ref.NewEntityFromAccountLocalpart(d.fleet, accountLocalpart)
		if entityErr != nil {
			continue
		}
		if !d.authorizeList(observer, principalEntity.UserID()) {
			continue
		}

		principals = append(principals, observe.ListPrincipal{
			Localpart:  accountLocalpart,
			Machine:    d.machine.Localpart(),
			Observable: true,
			Local:      true,
		})
	}

	// Service directory entries. Remote entries are not filtered by
	// local authorization — the hosting machine gates actual access.
	for localpart, service := range d.services {
		if seen[localpart] {
			continue
		}
		seen[localpart] = true

		machineLocalpart := service.Machine.Localpart()

		isLocal := service.Machine == d.machine
		observable := isLocal && runningSet[localpart]
		if !isLocal {
			// Remote principal is observable if we have a transport
			// dialer and the peer machine has a known address.
			_, peerReachable := d.peerAddresses[service.Machine.UserID().String()]
			observable = d.transportDialer != nil && peerReachable
		}

		principals = append(principals, observe.ListPrincipal{
			Localpart:  localpart,
			Machine:    machineLocalpart,
			Observable: observable,
			Local:      isLocal,
		})
	}

	// Filter to observable if requested.
	if request.Observable {
		filtered := principals[:0]
		for _, entry := range principals {
			if entry.Observable {
				filtered = append(filtered, entry)
			}
		}
		principals = filtered
	}

	// Collect machines.
	var machines []observe.ListMachine
	machines = append(machines, observe.ListMachine{
		Name:      d.machine.Localpart(),
		UserID:    d.machine.UserID().String(),
		Self:      true,
		Reachable: true,
	})
	for machineUserID, address := range d.peerAddresses {
		machineLocalpart, err := principal.LocalpartFromMatrixID(machineUserID)
		if err != nil {
			machineLocalpart = machineUserID
		}
		machines = append(machines, observe.ListMachine{
			Name:      machineLocalpart,
			UserID:    machineUserID,
			Self:      false,
			Reachable: address != "",
		})
	}

	d.logger.Info("list completed",
		"observer", request.Observer,
		"principals", len(principals),
		"machines", len(machines),
	)

	json.NewEncoder(clientConnection).Encode(observe.ListResponse{
		OK:         true,
		Principals: principals,
		Machines:   machines,
	})
}

// handleLocalObserve handles observation of a principal running on this machine.
// It forks a relay process, sends the success response (including the granted
// mode), and bridges bytes between the client and the relay.
func (d *Daemon) handleLocalObserve(clientConnection net.Conn, request observeRequest, grantedMode string) {
	sessionName := "bureau/" + request.Principal
	readOnly := grantedMode == "readonly"

	relayConnection, relayCommand, err := d.forkObserveRelay(sessionName, readOnly)
	if err != nil {
		d.logger.Error("fork observe relay failed",
			"principal", request.Principal,
			"error", err,
		)
		d.sendObserveError(clientConnection,
			fmt.Sprintf("failed to start relay: %v", err))
		return
	}

	// Send success response and clear the handshake deadline before
	// entering the streaming bridge.
	response := observeResponse{
		OK:          true,
		Session:     sessionName,
		Machine:     d.machine.Localpart(),
		GrantedMode: grantedMode,
	}
	clientConnection.SetDeadline(time.Time{})
	if err := json.NewEncoder(clientConnection).Encode(response); err != nil {
		d.logger.Error("send observe response failed",
			"principal", request.Principal,
			"error", err,
		)
		relayConnection.Close()
		relayCommand.Process.Kill()
		relayCommand.Wait()
		return
	}

	startTime := time.Now()

	// Register the session so enforceObserveAllowanceChange can terminate
	// it if the observation allowances tighten while the session is active.
	// Parse the observer and principal strings into typed IDs. These were
	// validated earlier in the request pipeline (authenticateObserver and
	// NewEntityFromAccountLocalpart).
	observerID, _ := ref.ParseUserID(request.Observer)
	principalEntity, _ := ref.NewEntityFromAccountLocalpart(d.fleet, request.Principal)
	observeSession := &activeObserveSession{
		principal:   principalEntity.UserID(),
		observer:    observerID,
		grantedMode: grantedMode,
		clientConn:  clientConnection,
	}
	d.registerObserveSession(observeSession)
	defer d.unregisterObserveSession(observeSession)

	d.logger.Info("observation started",
		"observer", request.Observer,
		"principal", request.Principal,
		"session", sessionName,
		"mode", grantedMode,
	)

	// Bridge bytes zero-copy between client and relay. This blocks until
	// one side disconnects or errors.
	if err := netutil.BridgeConnections(clientConnection, relayConnection); err != nil {
		d.logger.Warn("observation bridge error",
			"observer", request.Observer,
			"principal", request.Principal,
			"session", sessionName,
			"mode", grantedMode,
			"error", err,
		)
	}

	// Clean up the relay process. Send SIGTERM first for graceful shutdown,
	// escalate to SIGKILL after a timeout.
	cleanupRelayProcess(relayCommand)

	d.logger.Info("observation ended",
		"observer", request.Observer,
		"principal", request.Principal,
		"duration", time.Since(startTime).String(),
	)
}

// handleRemoteObserve forwards an observation request to a remote daemon via
// the transport layer. It connects to the peer, performs an HTTP handshake
// with the remote daemon's /observe/ handler, then bridges the resulting
// raw connection to the client.
func (d *Daemon) handleRemoteObserve(clientConnection net.Conn, request observeRequest, peerAddress string) {
	d.logger.Info("forwarding observation to remote daemon",
		"principal", request.Principal,
		"peer", peerAddress,
	)

	// Dial the remote daemon via the transport layer (WebRTC data channel).
	dialContext, dialCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer dialCancel()
	rawConnection, err := d.transportDialer.DialContext(dialContext, peerAddress)
	if err != nil {
		d.sendObserveError(clientConnection,
			fmt.Sprintf("cannot reach peer at %s: %v", peerAddress, err))
		return
	}

	// Build and send an HTTP POST to the remote daemon's observation handler.
	// The principal localpart contains '/' which is fine in the URL path —
	// the remote handler strips the /observe/ prefix and takes the rest.
	// The Host header uses "transport" as a placeholder — the transport
	// layer ignores it since the connection is already routed to the peer.
	requestBody, _ := json.Marshal(request)
	httpRequest, err := http.NewRequest("POST",
		"http://transport/observe/"+request.Principal,
		bytes.NewReader(requestBody))
	if err != nil {
		rawConnection.Close()
		d.sendObserveError(clientConnection, fmt.Sprintf("build request: %v", err))
		return
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	if err := httpRequest.Write(rawConnection); err != nil {
		rawConnection.Close()
		d.sendObserveError(clientConnection,
			fmt.Sprintf("send request to peer: %v", err))
		return
	}

	// Read the HTTP response. The bufio.Reader may read ahead into the
	// post-HTTP observation stream — we preserve those bytes via
	// bufferedConn for the bridge.
	bufferedReader := bufio.NewReader(rawConnection)
	httpResponse, err := http.ReadResponse(bufferedReader, httpRequest)
	if err != nil {
		rawConnection.Close()
		d.sendObserveError(clientConnection,
			fmt.Sprintf("read peer response: %v", err))
		return
	}

	responseBody, _ := netutil.ReadResponse(httpResponse.Body)
	httpResponse.Body.Close()

	var peerResponse observeResponse
	if err := json.Unmarshal(responseBody, &peerResponse); err != nil {
		rawConnection.Close()
		d.sendObserveError(clientConnection,
			fmt.Sprintf("invalid peer response: %v", err))
		return
	}

	if !peerResponse.OK {
		rawConnection.Close()
		d.sendObserveError(clientConnection, peerResponse.Error)
		return
	}

	// Forward the success response to the client.
	clientConnection.SetDeadline(time.Time{})
	if err := json.NewEncoder(clientConnection).Encode(peerResponse); err != nil {
		rawConnection.Close()
		return
	}

	startTime := time.Now()

	d.logger.Info("remote observation established",
		"observer", request.Observer,
		"principal", request.Principal,
		"session", peerResponse.Session,
		"peer", peerAddress,
		"mode", peerResponse.GrantedMode,
	)

	// Bridge client ↔ remote daemon. Use bufferedConn to include any bytes
	// the bufio.Reader read ahead beyond the HTTP response.
	peerConn := &bufferedConn{reader: bufferedReader, Conn: rawConnection}
	if err := netutil.BridgeConnections(clientConnection, peerConn); err != nil {
		d.logger.Warn("remote observation bridge error",
			"observer", request.Observer,
			"principal", request.Principal,
			"peer", peerAddress,
			"mode", peerResponse.GrantedMode,
			"error", err,
		)
	}

	d.logger.Info("remote observation ended",
		"observer", request.Observer,
		"principal", request.Principal,
		"peer", peerAddress,
		"duration", time.Since(startTime).String(),
	)
}

// handleTransportObserve handles observation requests arriving from peer
// daemons over the transport listener. The peer sends an HTTP POST with an
// observeRequest body. On success, the handler hijacks the HTTP connection,
// forks a relay, writes the observeResponse, and bridges bytes between the
// hijacked connection and the relay.
//
// The originating daemon has already verified the observer's token. The
// request carries the verified observer identity in the Observer field.
// This daemon trusts the forwarded identity (transport connections are
// authenticated via a mutual Ed25519 challenge-response handshake; see
// transport.PeerAuthenticator) and checks its own local observation
// allowances to authorize the observation.
func (d *Daemon) handleTransportObserve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract principal from path: /observe/<localpart>
	principalLocalpart := strings.TrimPrefix(r.URL.Path, "/observe/")
	if principalLocalpart == "" {
		http.Error(w, "empty principal in path", http.StatusBadRequest)
		return
	}

	var request observeRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(observeResponse{
			Error: fmt.Sprintf("invalid request: %v", err),
		})
		return
	}

	// The principal in the path and body must match.
	if request.Principal != principalLocalpart {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(observeResponse{
			Error: fmt.Sprintf("principal mismatch: path=%q body=%q",
				principalLocalpart, request.Principal),
		})
		return
	}

	// The originating daemon must include the verified observer identity.
	if request.Observer == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(observeResponse{
			Error: "observer identity is required in forwarded request",
		})
		return
	}

	// Construct a fleet-scoped Entity from the request's account
	// localpart to look up in the Entity-keyed d.running map.
	transportEntity, err := ref.NewEntityFromAccountLocalpart(d.fleet, request.Principal)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(observeResponse{
			Error: fmt.Sprintf("invalid principal %q: %v", request.Principal, err),
		})
		return
	}

	// Parse the observer identity forwarded from the originating daemon.
	observer, observerParseErr := ref.ParseUserID(request.Observer)
	if observerParseErr != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(observeResponse{
			Error: fmt.Sprintf("invalid observer identity %q: %v", request.Observer, observerParseErr),
		})
		return
	}

	// Check local authorization for the forwarded observer identity.
	requestedMode := request.Mode
	authz := d.authorizeObserve(observer, transportEntity.UserID(), requestedMode)
	if !authz.Allowed {
		d.logger.Warn("transport observation denied",
			"observer", request.Observer,
			"principal", request.Principal,
			"requested_mode", requestedMode,
		)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(observeResponse{
			Error: fmt.Sprintf("not authorized to observe %q — the principal's authorization policy has no observe allowance matching your identity", request.Principal),
		})
		return
	}

	if requestedMode != authz.GrantedMode {
		d.logger.Info("transport observation mode downgraded",
			"observer", request.Observer,
			"principal", request.Principal,
			"requested_mode", requestedMode,
			"granted_mode", authz.GrantedMode,
		)
	}

	d.reconcileMu.RLock()
	principalRunning := d.running[transportEntity]
	d.reconcileMu.RUnlock()

	if !principalRunning {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(observeResponse{
			Error: fmt.Sprintf("principal %q not running on this machine", request.Principal),
		})
		return
	}

	sessionName := "bureau/" + request.Principal
	readOnly := authz.GrantedMode == "readonly"

	relayConnection, relayCommand, err := d.forkObserveRelay(sessionName, readOnly)
	if err != nil {
		d.logger.Error("fork observe relay for transport failed",
			"principal", request.Principal,
			"error", err,
		)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(observeResponse{
			Error: fmt.Sprintf("failed to start relay: %v", err),
		})
		return
	}

	// Hijack the HTTP connection to switch to the binary observation protocol.
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		d.logger.Error("response writer does not support hijacking")
		relayConnection.Close()
		relayCommand.Process.Kill()
		relayCommand.Wait()
		http.Error(w, "server does not support connection hijacking", http.StatusInternalServerError)
		return
	}

	transportConnection, transportBuffer, err := hijacker.Hijack()
	if err != nil {
		d.logger.Error("hijack transport connection failed", "error", err)
		relayConnection.Close()
		relayCommand.Process.Kill()
		relayCommand.Wait()
		return
	}

	// Clear any read/write deadlines set by http.Server during Hijack.
	// The server calls SetReadDeadline(aLongTimeAgo) to abort its internal
	// background reader before returning the raw connection. Without
	// clearing this, subsequent reads return os.ErrDeadlineExceeded
	// immediately and the bridge dies in microseconds.
	transportConnection.SetDeadline(time.Time{})

	// Write the HTTP response manually on the hijacked connection.
	responseJSON, _ := json.Marshal(observeResponse{
		OK:          true,
		Session:     sessionName,
		Machine:     d.machine.Localpart(),
		GrantedMode: authz.GrantedMode,
	})
	fmt.Fprintf(transportBuffer, "HTTP/1.1 200 OK\r\n")
	fmt.Fprintf(transportBuffer, "Content-Type: application/json\r\n")
	fmt.Fprintf(transportBuffer, "Content-Length: %d\r\n", len(responseJSON))
	fmt.Fprintf(transportBuffer, "\r\n")
	transportBuffer.Write(responseJSON)
	transportBuffer.Flush()

	startTime := time.Now()

	d.logger.Info("transport observation started",
		"observer", request.Observer,
		"principal", request.Principal,
		"session", sessionName,
		"mode", authz.GrantedMode,
	)

	// Bridge the hijacked connection to the relay.
	if err := netutil.BridgeConnections(transportConnection, relayConnection); err != nil {
		d.logger.Warn("transport observation bridge error",
			"observer", request.Observer,
			"principal", request.Principal,
			"session", sessionName,
			"mode", authz.GrantedMode,
			"error", err,
		)
	}

	// Clean up the relay process and log its exit status. If the bridge
	// returned very quickly, the relay likely failed during initialization
	// (PTY allocation, tmux attach, metadata query).
	bridgeDuration := time.Since(startTime)
	relayExitError := cleanupRelayProcess(relayCommand)

	d.logger.Info("transport observation ended",
		"observer", request.Observer,
		"principal", request.Principal,
		"duration", bridgeDuration.String(),
		"relay_exit", relayExitError,
	)
}

// forkObserveRelay creates a socketpair and forks the observation relay binary.
// Returns the daemon's end of the socketpair (as a net.Conn) and the relay's
// exec.Cmd. The relay receives the other end of the socketpair as fd 3.
//
// The relay binary is invoked as: bureau-observe-relay <session-name>
// Environment:
//   - BUREAU_TMUX_SOCKET: tmux server socket path
//   - BUREAU_OBSERVE_READONLY=1: if readOnly is true
func (d *Daemon) forkObserveRelay(sessionName string, readOnly bool) (net.Conn, *exec.Cmd, error) {
	// Create a socketpair. fds[0] goes to the relay as fd 3; fds[1] stays
	// with the daemon and is converted to a net.Conn.
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("creating socketpair: %w", err)
	}

	relayFile := os.NewFile(uintptr(fds[0]), "relay-socket")
	daemonFile := os.NewFile(uintptr(fds[1]), "daemon-socket")

	// Convert the daemon's end to a net.Conn for proper socket semantics
	// (deadline support, etc.). FileConn dups the fd internally, so we
	// close the original.
	daemonConnection, err := net.FileConn(daemonFile)
	daemonFile.Close()
	if err != nil {
		relayFile.Close()
		return nil, nil, fmt.Errorf("converting daemon socket to net.Conn: %w", err)
	}

	// Build environment for the relay process.
	environment := os.Environ()
	if d.tmuxServer != nil {
		environment = append(environment, "BUREAU_TMUX_SOCKET="+d.tmuxServer.SocketPath())
	}
	if readOnly {
		environment = append(environment, "BUREAU_OBSERVE_READONLY=1")
	}

	command := exec.Command(d.observeRelayBinary, sessionName)
	command.Env = environment
	command.ExtraFiles = []*os.File{relayFile} // becomes fd 3 in child
	command.Stderr = os.Stderr

	if err := command.Start(); err != nil {
		daemonConnection.Close()
		relayFile.Close()
		return nil, nil, fmt.Errorf("starting observe relay %q: %w", d.observeRelayBinary, err)
	}

	// Close the relay's end in the parent — the child has its own copy.
	relayFile.Close()

	return daemonConnection, command, nil
}

// findPrincipalPeer looks up which remote machine hosts a principal by
// checking the service directory. Returns the peer's transport address if
// the principal is a known remote service with a reachable peer.
//
// This provides best-effort remote observation for service principals. For
// non-service principals, a future principal directory would be needed.
func (d *Daemon) findPrincipalPeer(localpart string) (peerAddress string, ok bool) {
	if d.transportDialer == nil {
		return "", false
	}

	for _, service := range d.services {
		if service.Principal.AccountLocalpart() != localpart {
			continue
		}
		if service.Machine == d.machine {
			continue // Local, not remote.
		}
		address, exists := d.peerAddresses[service.Machine.UserID().String()]
		if !exists || address == "" {
			continue
		}
		return address, true
	}
	return "", false
}

// cleanupRelayProcess sends SIGTERM and waits for the relay to exit.
// If the relay doesn't exit within 5 seconds, it is killed with SIGKILL.
// Returns the relay's exit error (nil if it exited cleanly).
func cleanupRelayProcess(command *exec.Cmd) error {
	command.Process.Signal(syscall.SIGTERM)
	exitChannel := make(chan error, 1)
	go func() { exitChannel <- command.Wait() }()
	select {
	case err := <-exitChannel:
		return err
	case <-time.After(5 * time.Second):
		command.Process.Kill()
		return <-exitChannel
	}
}

// sendObserveError writes an error observeResponse to the connection.
func (d *Daemon) sendObserveError(connection net.Conn, message string) {
	d.logger.Warn("observe request failed", "error", message)
	json.NewEncoder(connection).Encode(observeResponse{Error: message})
}

// sendObserveJSON marshals any response value as JSON and writes it as a
// newline-terminated line to the client connection. Falls back to
// sendObserveError if marshaling fails. Used by query handlers that return
// typed response structs (auth queries, etc.).
func (d *Daemon) sendObserveJSON(clientConnection net.Conn, response any) {
	data, err := json.Marshal(response)
	if err != nil {
		d.sendObserveError(clientConnection, "marshaling observe response: "+err.Error())
		return
	}
	data = append(data, '\n')
	clientConnection.Write(data) //nolint:errcheck // best-effort response
}

// bufferedConn wraps a net.Conn with a buffered reader for reads while
// writing directly to the underlying connection. Used after HTTP response
// parsing where the bufio.Reader may have read ahead into the post-HTTP
// streaming data.
type bufferedConn struct {
	reader *bufio.Reader
	net.Conn
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}
