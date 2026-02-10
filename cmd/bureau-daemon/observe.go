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
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/observe"
)

// observeRequest mirrors observe.ObserveRequest. Defined locally to parallel
// the existing pattern with launcherIPCRequest — the JSON wire format is the
// contract between client and daemon.
type observeRequest struct {
	// Action selects the request type:
	//   - "" or "observe": streaming observation session
	//   - "query_layout": fetch and expand a channel's layout
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
	// when the ObservePolicy only grants the observer readonly access.
	GrantedMode string `json:"granted_mode,omitempty"`

	// Error describes why the request failed.
	Error string `json:"error,omitempty"`
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
// It reads the initial JSON line, dispatches on the action field, and
// either establishes a streaming observation session or handles a
// request/response query.
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

	switch request.Action {
	case "", "observe":
		d.handleObserveSession(clientConnection, request)
	case "query_layout":
		d.handleQueryLayout(clientConnection, request)
	case "list":
		d.handleList(clientConnection, request)
	default:
		d.sendObserveError(clientConnection,
			fmt.Sprintf("unknown action %q", request.Action))
	}
}

// handleObserveSession handles a streaming observation request. It validates
// the principal, determines whether it is local or remote, and either forks
// a relay or forwards through the transport.
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

	d.logger.Info("observation requested",
		"principal", request.Principal,
		"mode", request.Mode,
	)

	// Check if the principal is running locally.
	if d.running[request.Principal] {
		d.handleLocalObserve(clientConnection, request)
		return
	}

	// Check if the principal is a known service on a remote machine
	// reachable via the transport.
	if peerAddress, ok := d.findPrincipalPeer(request.Principal); ok {
		d.handleRemoteObserve(clientConnection, request, peerAddress)
		return
	}

	d.sendObserveError(clientConnection,
		fmt.Sprintf("principal %q not found", request.Principal))
}

// handleQueryLayout handles a query_layout request: resolves a channel alias
// to a room ID, fetches the m.bureau.layout state event and room membership,
// expands ObserveMembers panes, and returns the concrete layout as JSON.
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

	d.logger.Info("query_layout requested", "channel", request.Channel)

	// Resolve the channel alias to a room ID.
	roomID, err := d.session.ResolveAlias(ctx, request.Channel)
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

	// Convert messaging.RoomMember to observe.RoomMember. Only include
	// joined members — invited, left, and banned members are excluded.
	var observeMembers []observe.RoomMember
	for _, member := range matrixMembers {
		if member.Membership != "join" {
			continue
		}
		localpart, localpartErr := principal.LocalpartFromMatrixID(member.UserID)
		if localpartErr != nil {
			// Skip members whose user IDs don't match the Bureau
			// @localpart:server convention (e.g., the admin account
			// might be @admin:bureau.local without a hierarchical
			// localpart).
			continue
		}
		observeMembers = append(observeMembers, observe.RoomMember{
			Localpart: localpart,
			// Role is not yet populated from Matrix — Bureau role
			// metadata will come from a principal identity state
			// event. For now, role-based ObserveMembers filtering
			// matches no roles, so all-members filters work but
			// role-specific filters return empty.
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

// handleList handles a "list" request: enumerates all known principals and
// machines, returning each with observability and location metadata.
//
// Principals come from two sources:
//   - d.running: principals with active sandboxes on this machine (always observable)
//   - d.services: service directory entries across all machines (observable when
//     local, or remote with a reachable transport address)
//
// Machines come from d.peerAddresses plus the local machine.
func (d *Daemon) handleList(clientConnection net.Conn, request observeRequest) {
	d.logger.Info("list requested", "observable", request.Observable)

	// Collect principals. Use a map to deduplicate between d.running and
	// d.services (a locally running service appears in both).
	seen := make(map[string]bool)
	var principals []observe.ListPrincipal

	// Locally running principals.
	for localpart := range d.running {
		seen[localpart] = true
		principals = append(principals, observe.ListPrincipal{
			Localpart:  localpart,
			Machine:    d.machineName,
			Observable: true,
			Local:      true,
		})
	}

	// Service directory entries (may include remote services and local
	// services not yet in the seen set — though in practice, a local
	// service that's in d.services should also be in d.running).
	for localpart, service := range d.services {
		if seen[localpart] {
			continue
		}
		seen[localpart] = true

		machineLocalpart, err := principal.LocalpartFromMatrixID(service.Machine)
		if err != nil {
			machineLocalpart = service.Machine
		}

		isLocal := service.Machine == d.machineUserID
		observable := isLocal && d.running[localpart]
		if !isLocal {
			// Remote principal is observable if we have a transport
			// dialer and the peer machine has a known address.
			_, peerReachable := d.peerAddresses[service.Machine]
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
		for _, principal := range principals {
			if principal.Observable {
				filtered = append(filtered, principal)
			}
		}
		principals = filtered
	}

	// Collect machines.
	var machines []observe.ListMachine
	machines = append(machines, observe.ListMachine{
		Name:      d.machineName,
		UserID:    d.machineUserID,
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
// It forks a relay process, sends the success response, and bridges bytes
// between the client and the relay.
func (d *Daemon) handleLocalObserve(clientConnection net.Conn, request observeRequest) {
	sessionName := "bureau/" + request.Principal
	readOnly := request.Mode == "readonly"

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
		OK:      true,
		Session: sessionName,
		Machine: d.machineName,
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

	d.logger.Info("observation started",
		"principal", request.Principal,
		"session", sessionName,
	)

	// Bridge bytes zero-copy between client and relay. This blocks until
	// one side disconnects or errors.
	bridgeConnections(clientConnection, relayConnection)

	// Clean up the relay process. Send SIGTERM first for graceful shutdown,
	// escalate to SIGKILL after a timeout.
	cleanupRelayProcess(relayCommand)

	d.logger.Info("observation ended", "principal", request.Principal)
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

	responseBody, _ := io.ReadAll(httpResponse.Body)
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

	d.logger.Info("remote observation established",
		"principal", request.Principal,
		"session", peerResponse.Session,
		"peer", peerAddress,
	)

	// Bridge client ↔ remote daemon. Use bufferedConn to include any bytes
	// the bufio.Reader read ahead beyond the HTTP response.
	peerConn := &bufferedConn{reader: bufferedReader, Conn: rawConnection}
	bridgeConnections(clientConnection, peerConn)

	d.logger.Info("remote observation ended", "principal", request.Principal)
}

// handleTransportObserve handles observation requests arriving from peer
// daemons over the transport listener. The peer sends an HTTP POST with an
// observeRequest body. On success, the handler hijacks the HTTP connection,
// forks a relay, writes the observeResponse, and bridges bytes between the
// hijacked connection and the relay.
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

	if !d.running[request.Principal] {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(observeResponse{
			Error: fmt.Sprintf("principal %q not running on this machine", request.Principal),
		})
		return
	}

	sessionName := "bureau/" + request.Principal
	readOnly := request.Mode == "readonly"

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

	// Write the HTTP response manually on the hijacked connection.
	responseJSON, _ := json.Marshal(observeResponse{
		OK:      true,
		Session: sessionName,
		Machine: d.machineName,
	})
	fmt.Fprintf(transportBuffer, "HTTP/1.1 200 OK\r\n")
	fmt.Fprintf(transportBuffer, "Content-Type: application/json\r\n")
	fmt.Fprintf(transportBuffer, "Content-Length: %d\r\n", len(responseJSON))
	fmt.Fprintf(transportBuffer, "\r\n")
	transportBuffer.Write(responseJSON)
	transportBuffer.Flush()

	d.logger.Info("transport observation started",
		"principal", request.Principal,
		"session", sessionName,
	)

	// Bridge the hijacked connection to the relay.
	bridgeConnections(transportConnection, relayConnection)

	cleanupRelayProcess(relayCommand)

	d.logger.Info("transport observation ended", "principal", request.Principal)
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
	if d.tmuxServerSocket != "" {
		environment = append(environment, "BUREAU_TMUX_SOCKET="+d.tmuxServerSocket)
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
// non-service principals (regular agents), a future principal directory
// would be needed.
func (d *Daemon) findPrincipalPeer(localpart string) (peerAddress string, ok bool) {
	if d.transportDialer == nil {
		return "", false
	}

	for _, service := range d.services {
		serviceLocalpart, err := principal.LocalpartFromMatrixID(service.Principal)
		if err != nil {
			continue
		}
		if serviceLocalpart != localpart {
			continue
		}
		if service.Machine == d.machineUserID {
			continue // Local, not remote.
		}
		address, exists := d.peerAddresses[service.Machine]
		if !exists || address == "" {
			continue
		}
		return address, true
	}
	return "", false
}

// bridgeConnections copies bytes bidirectionally between two connections.
// Returns when either direction finishes (EOF, error, or closed connection).
// Both connections are closed before returning.
func bridgeConnections(a, b net.Conn) {
	done := make(chan struct{}, 2)

	go func() {
		io.Copy(a, b)
		done <- struct{}{}
	}()

	go func() {
		io.Copy(b, a)
		done <- struct{}{}
	}()

	// Wait for one direction to finish, then close both to unblock the other.
	<-done
	a.Close()
	b.Close()
	<-done
}

// cleanupRelayProcess sends SIGTERM and waits for the relay to exit.
// If the relay doesn't exit within 5 seconds, it is killed with SIGKILL.
func cleanupRelayProcess(command *exec.Cmd) {
	command.Process.Signal(syscall.SIGTERM)
	exitChannel := make(chan error, 1)
	go func() { exitChannel <- command.Wait() }()
	select {
	case <-exitChannel:
	case <-time.After(5 * time.Second):
		command.Process.Kill()
		<-exitChannel
	}
}

// sendObserveError writes an error observeResponse to the connection.
func (d *Daemon) sendObserveError(connection net.Conn, message string) {
	d.logger.Warn("observe request failed", "error", message)
	json.NewEncoder(connection).Encode(observeResponse{Error: message})
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
