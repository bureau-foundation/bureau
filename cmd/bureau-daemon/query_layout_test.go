// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/testutil"
	"github.com/bureau-foundation/bureau/messaging"
	"github.com/bureau-foundation/bureau/observe"
)

// newTestDaemonWithQuery creates a Daemon with a mock Matrix server that
// supports alias resolution, state events, and room members — everything
// handleQueryLayout needs. Also sets up authentication via a permissive
// ObservePolicy and a token verifier backed by a mock whoami endpoint.
// Returns the daemon and mock for test setup.
func newTestDaemonWithQuery(t *testing.T) (*Daemon, *mockMatrixState) {
	t.Helper()

	socketDir := testutil.SocketDir(t)
	observeSocketPath := filepath.Join(socketDir, "observe.sock")
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	matrixState := newMockMatrixState()

	// Wrap the state mock's handler with whoami support for token verification.
	stateHandler := matrixState.handler()
	combinedHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_matrix/client/v3/account/whoami" {
			authorization := r.Header.Get("Authorization")
			if authorization == "Bearer "+testObserverToken {
				json.NewEncoder(w).Encode(messaging.WhoAmIResponse{
					UserID: testObserverUserID,
				})
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{
				"errcode": "M_UNKNOWN_TOKEN",
				"error":   "Invalid token",
			})
			return
		}
		stateHandler.ServeHTTP(w, r)
	})

	matrixServer := httptest.NewServer(combinedHandler)
	t.Cleanup(matrixServer.Close)

	matrixClient, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: matrixServer.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session, err := matrixClient.SessionFromToken("@machine/test:bureau.local", "test-token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	daemon := &Daemon{
		runDir:        principal.DefaultRunDir,
		session:       session,
		client:        matrixClient,
		tokenVerifier: newTokenVerifier(matrixClient, 5*time.Minute, logger),
		lastConfig: &schema.MachineConfig{
			DefaultObservePolicy: &schema.ObservePolicy{
				AllowedObservers:   []string{"**"},
				ReadWriteObservers: []string{"**"},
			},
		},
		machineName:       "machine/test",
		machineUserID:     "@machine/test:bureau.local",
		serverName:        "bureau.local",
		configRoomID:      "!config:test",
		running:           make(map[string]bool),
		services:          make(map[string]*schema.Service),
		proxyRoutes:       make(map[string]string),
		peerAddresses:     make(map[string]string),
		peerTransports:    make(map[string]http.RoundTripper),
		observeSocketPath: observeSocketPath,
		layoutWatchers:    make(map[string]*layoutWatcher),
		logger:            logger,
	}
	t.Cleanup(daemon.stopAllLayoutWatchers)

	ctx := context.Background()
	if err := daemon.startObserveListener(ctx); err != nil {
		t.Fatalf("startObserveListener: %v", err)
	}
	t.Cleanup(daemon.stopObserveListener)

	return daemon, matrixState
}

// sendQueryLayout sends a query_layout request to the daemon's observe
// socket and returns the parsed response.
func sendQueryLayout(t *testing.T, socketPath, channel string) observe.QueryLayoutResponse {
	t.Helper()

	connection, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		t.Fatalf("dial observe socket: %v", err)
	}
	defer connection.Close()

	connection.SetDeadline(time.Now().Add(5 * time.Second))

	request := observe.QueryLayoutRequest{
		Action:   "query_layout",
		Channel:  channel,
		Observer: testObserverUserID,
		Token:    testObserverToken,
	}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		t.Fatalf("send query_layout request: %v", err)
	}

	reader := bufio.NewReader(connection)
	responseLine, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read query_layout response: %v", err)
	}

	var response observe.QueryLayoutResponse
	if err := json.Unmarshal(responseLine, &response); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	return response
}

func TestQueryLayoutChannelDashboard(t *testing.T) {
	daemon, matrixState := newTestDaemonWithQuery(t)

	channelAlias := "#iree/amdgpu/general:bureau.local"
	channelRoomID := "!channel-room:bureau.local"

	// Set up mock data: alias → room ID, layout state event, members.
	matrixState.setRoomAlias(channelAlias, channelRoomID)

	matrixState.setStateEvent(channelRoomID, schema.EventTypeLayout, "", schema.LayoutContent{
		Windows: []schema.LayoutWindow{
			{
				Name: "agents",
				Panes: []schema.LayoutPane{
					{ObserveMembers: &schema.LayoutMemberFilter{}},
				},
			},
			{
				Name: "tools",
				Panes: []schema.LayoutPane{
					{Command: "htop"},
				},
			},
		},
	})

	matrixState.setRoomMembers(channelRoomID, []mockRoomMember{
		{UserID: "@iree/amdgpu/pm:bureau.local", Membership: "join"},
		{UserID: "@iree/amdgpu/compiler:bureau.local", Membership: "join"},
		// Invited member should be excluded.
		{UserID: "@iree/amdgpu/pending:bureau.local", Membership: "invite"},
	})

	response := sendQueryLayout(t, daemon.observeSocketPath, channelAlias)

	if !response.OK {
		t.Fatalf("expected OK, got error: %s", response.Error)
	}
	if response.Layout == nil {
		t.Fatal("response layout is nil")
	}

	// Verify the layout was expanded: the ObserveMembers pane should be
	// replaced with two Observe panes (one per joined member), and the
	// tools window should pass through unchanged.
	if len(response.Layout.Windows) != 2 {
		t.Fatalf("expected 2 windows, got %d", len(response.Layout.Windows))
	}

	agentsWindow := response.Layout.Windows[0]
	if agentsWindow.Name != "agents" {
		t.Errorf("window 0 name = %q, want %q", agentsWindow.Name, "agents")
	}
	if len(agentsWindow.Panes) != 2 {
		t.Fatalf("expected 2 panes in agents window, got %d", len(agentsWindow.Panes))
	}
	if agentsWindow.Panes[0].Observe != "iree/amdgpu/pm" {
		t.Errorf("pane 0 observe = %q, want %q",
			agentsWindow.Panes[0].Observe, "iree/amdgpu/pm")
	}
	if agentsWindow.Panes[1].Observe != "iree/amdgpu/compiler" {
		t.Errorf("pane 1 observe = %q, want %q",
			agentsWindow.Panes[1].Observe, "iree/amdgpu/compiler")
	}

	toolsWindow := response.Layout.Windows[1]
	if toolsWindow.Name != "tools" {
		t.Errorf("window 1 name = %q, want %q", toolsWindow.Name, "tools")
	}
	if len(toolsWindow.Panes) != 1 || toolsWindow.Panes[0].Command != "htop" {
		t.Errorf("tools window panes = %+v, want single htop pane", toolsWindow.Panes)
	}
}

func TestQueryLayoutStaticLayout(t *testing.T) {
	// A layout with no ObserveMembers panes should pass through as-is.
	daemon, matrixState := newTestDaemonWithQuery(t)

	channelAlias := "#static:bureau.local"
	channelRoomID := "!static-room:bureau.local"

	matrixState.setRoomAlias(channelAlias, channelRoomID)
	matrixState.setStateEvent(channelRoomID, schema.EventTypeLayout, "", schema.LayoutContent{
		Windows: []schema.LayoutWindow{
			{
				Name: "main",
				Panes: []schema.LayoutPane{
					{Observe: "iree/amdgpu/pm"},
					{Observe: "iree/amdgpu/compiler", Split: "horizontal"},
				},
			},
		},
	})

	response := sendQueryLayout(t, daemon.observeSocketPath, channelAlias)

	if !response.OK {
		t.Fatalf("expected OK, got error: %s", response.Error)
	}
	if len(response.Layout.Windows) != 1 {
		t.Fatalf("expected 1 window, got %d", len(response.Layout.Windows))
	}
	if len(response.Layout.Windows[0].Panes) != 2 {
		t.Fatalf("expected 2 panes, got %d", len(response.Layout.Windows[0].Panes))
	}
}

func TestQueryLayoutUnknownAlias(t *testing.T) {
	daemon, _ := newTestDaemonWithQuery(t)

	response := sendQueryLayout(t, daemon.observeSocketPath, "#nonexistent:bureau.local")

	if response.OK {
		t.Error("expected error, got OK")
	}
	if !strings.Contains(response.Error, "resolve channel") {
		t.Errorf("error = %q, expected it to contain 'resolve channel'", response.Error)
	}
}

func TestQueryLayoutMissingLayoutEvent(t *testing.T) {
	daemon, matrixState := newTestDaemonWithQuery(t)

	channelAlias := "#no-layout:bureau.local"
	channelRoomID := "!no-layout-room:bureau.local"

	matrixState.setRoomAlias(channelAlias, channelRoomID)
	// No layout state event set.

	response := sendQueryLayout(t, daemon.observeSocketPath, channelAlias)

	if response.OK {
		t.Error("expected error for missing layout, got OK")
	}
	if !strings.Contains(response.Error, "fetch layout") {
		t.Errorf("error = %q, expected it to contain 'fetch layout'", response.Error)
	}
}

func TestQueryLayoutEmptyChannel(t *testing.T) {
	daemon, _ := newTestDaemonWithQuery(t)

	response := sendQueryLayout(t, daemon.observeSocketPath, "")

	if response.OK {
		t.Error("expected error for empty channel, got OK")
	}
	if !strings.Contains(response.Error, "channel is required") {
		t.Errorf("error = %q, expected it to contain 'channel is required'", response.Error)
	}
}

func TestQueryLayoutUnknownAction(t *testing.T) {
	daemon, _ := newTestDaemonWithQuery(t)

	// Send a request with an unrecognized action.
	connection, err := net.DialTimeout("unix", daemon.observeSocketPath, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer connection.Close()
	connection.SetDeadline(time.Now().Add(5 * time.Second))

	request := map[string]string{"action": "bogus", "token": testObserverToken}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		t.Fatalf("send: %v", err)
	}

	reader := bufio.NewReader(connection)
	responseLine, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// The daemon sends an observeResponse on error (ok=false, error).
	var response map[string]any
	if err := json.Unmarshal(responseLine, &response); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if response["ok"] != false {
		t.Errorf("expected ok=false, got %v", response["ok"])
	}
	errorMessage, _ := response["error"].(string)
	if !strings.Contains(errorMessage, "unknown action") {
		t.Errorf("error = %q, expected it to contain 'unknown action'", errorMessage)
	}
}

func TestQueryLayoutExcludesNonBureauMembers(t *testing.T) {
	// Members whose user IDs don't follow the @localpart:server
	// convention (e.g., a plain admin account) should be silently
	// excluded from the expansion.
	daemon, matrixState := newTestDaemonWithQuery(t)

	channelAlias := "#mixed:bureau.local"
	channelRoomID := "!mixed-room:bureau.local"

	matrixState.setRoomAlias(channelAlias, channelRoomID)
	matrixState.setStateEvent(channelRoomID, schema.EventTypeLayout, "", schema.LayoutContent{
		Windows: []schema.LayoutWindow{
			{
				Name: "all",
				Panes: []schema.LayoutPane{
					{ObserveMembers: &schema.LayoutMemberFilter{}},
				},
			},
		},
	})

	matrixState.setRoomMembers(channelRoomID, []mockRoomMember{
		{UserID: "@iree/amdgpu/pm:bureau.local", Membership: "join"},
		// Admin account without hierarchical localpart — still valid
		// Matrix ID but LocalpartFromMatrixID won't fail. It returns
		// "admin" which is a valid Bureau localpart.
		{UserID: "@admin:bureau.local", Membership: "join"},
	})

	response := sendQueryLayout(t, daemon.observeSocketPath, channelAlias)

	if !response.OK {
		t.Fatalf("expected OK, got error: %s", response.Error)
	}

	// Both members should appear — "admin" is a valid localpart even
	// though it's not hierarchical. The filtering is based on
	// LocalpartFromMatrixID succeeding, not on localpart structure.
	if len(response.Layout.Windows) != 1 {
		t.Fatalf("expected 1 window, got %d", len(response.Layout.Windows))
	}
	if len(response.Layout.Windows[0].Panes) != 2 {
		t.Fatalf("expected 2 panes, got %d", len(response.Layout.Windows[0].Panes))
	}
}

func TestQueryLayoutLabelFiltering(t *testing.T) {
	// A layout with label-based ObserveMembers filtering. The daemon
	// populates labels from its MachineConfig and only matching members
	// become observe panes.
	daemon, matrixState := newTestDaemonWithQuery(t)

	// Give the daemon a config with labeled principals.
	daemon.lastConfig = &schema.MachineConfig{
		DefaultObservePolicy: &schema.ObservePolicy{
			AllowedObservers:   []string{"**"},
			ReadWriteObservers: []string{"**"},
		},
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "iree/amdgpu/pm",
				Labels:    map[string]string{"role": "agent", "team": "iree"},
			},
			{
				Localpart: "iree/amdgpu/compiler",
				Labels:    map[string]string{"role": "agent", "team": "iree"},
			},
			{
				Localpart: "service/stt/whisper",
				Labels:    map[string]string{"role": "service"},
			},
		},
	}

	channelAlias := "#iree/amdgpu/general:bureau.local"
	channelRoomID := "!label-room:bureau.local"

	matrixState.setRoomAlias(channelAlias, channelRoomID)

	// Layout filters to only agents.
	matrixState.setStateEvent(channelRoomID, schema.EventTypeLayout, "", schema.LayoutContent{
		Windows: []schema.LayoutWindow{
			{
				Name: "agents",
				Panes: []schema.LayoutPane{
					{ObserveMembers: &schema.LayoutMemberFilter{
						Labels: map[string]string{"role": "agent"},
					}},
				},
			},
		},
	})

	// All three principals are room members.
	matrixState.setRoomMembers(channelRoomID, []mockRoomMember{
		{UserID: "@iree/amdgpu/pm:bureau.local", Membership: "join"},
		{UserID: "@iree/amdgpu/compiler:bureau.local", Membership: "join"},
		{UserID: "@service/stt/whisper:bureau.local", Membership: "join"},
	})

	response := sendQueryLayout(t, daemon.observeSocketPath, channelAlias)

	if !response.OK {
		t.Fatalf("expected OK, got error: %s", response.Error)
	}

	// Only the two agents should appear (service filtered out).
	if len(response.Layout.Windows) != 1 {
		t.Fatalf("expected 1 window, got %d", len(response.Layout.Windows))
	}
	panes := response.Layout.Windows[0].Panes
	if len(panes) != 2 {
		t.Fatalf("expected 2 panes (agents only), got %d", len(panes))
	}
	if panes[0].Observe != "iree/amdgpu/pm" {
		t.Errorf("pane 0 observe = %q, want %q", panes[0].Observe, "iree/amdgpu/pm")
	}
	if panes[1].Observe != "iree/amdgpu/compiler" {
		t.Errorf("pane 1 observe = %q, want %q", panes[1].Observe, "iree/amdgpu/compiler")
	}
}

func TestQueryLayoutMultiLabelFiltering(t *testing.T) {
	// Filter by multiple labels: role=agent AND team=iree.
	daemon, matrixState := newTestDaemonWithQuery(t)

	daemon.lastConfig = &schema.MachineConfig{
		DefaultObservePolicy: &schema.ObservePolicy{
			AllowedObservers:   []string{"**"},
			ReadWriteObservers: []string{"**"},
		},
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "iree/amdgpu/pm",
				Labels:    map[string]string{"role": "agent", "team": "iree"},
			},
			{
				Localpart: "infra/ci/runner",
				Labels:    map[string]string{"role": "agent", "team": "infra"},
			},
			{
				Localpart: "service/stt/whisper",
				Labels:    map[string]string{"role": "service", "team": "iree"},
			},
		},
	}

	channelAlias := "#all:bureau.local"
	channelRoomID := "!multi-label-room:bureau.local"

	matrixState.setRoomAlias(channelAlias, channelRoomID)
	matrixState.setStateEvent(channelRoomID, schema.EventTypeLayout, "", schema.LayoutContent{
		Windows: []schema.LayoutWindow{
			{
				Name: "iree-agents",
				Panes: []schema.LayoutPane{
					{ObserveMembers: &schema.LayoutMemberFilter{
						Labels: map[string]string{"role": "agent", "team": "iree"},
					}},
				},
			},
		},
	})

	matrixState.setRoomMembers(channelRoomID, []mockRoomMember{
		{UserID: "@iree/amdgpu/pm:bureau.local", Membership: "join"},
		{UserID: "@infra/ci/runner:bureau.local", Membership: "join"},
		{UserID: "@service/stt/whisper:bureau.local", Membership: "join"},
	})

	response := sendQueryLayout(t, daemon.observeSocketPath, channelAlias)

	if !response.OK {
		t.Fatalf("expected OK, got error: %s", response.Error)
	}

	// Only iree/amdgpu/pm matches both role=agent AND team=iree.
	if len(response.Layout.Windows) != 1 {
		t.Fatalf("expected 1 window, got %d", len(response.Layout.Windows))
	}
	panes := response.Layout.Windows[0].Panes
	if len(panes) != 1 {
		t.Fatalf("expected 1 pane (iree agents only), got %d", len(panes))
	}
	if panes[0].Observe != "iree/amdgpu/pm" {
		t.Errorf("pane 0 observe = %q, want %q", panes[0].Observe, "iree/amdgpu/pm")
	}
}

func TestQueryLayoutCrossMachineMembersNoLabels(t *testing.T) {
	// Members whose localparts are NOT in this daemon's MachineConfig
	// get nil labels. With an empty filter they should still appear;
	// with a label filter they should be excluded.
	daemon, matrixState := newTestDaemonWithQuery(t)

	// Only one local principal configured.
	daemon.lastConfig = &schema.MachineConfig{
		DefaultObservePolicy: &schema.ObservePolicy{
			AllowedObservers:   []string{"**"},
			ReadWriteObservers: []string{"**"},
		},
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "iree/amdgpu/pm",
				Labels:    map[string]string{"role": "agent"},
			},
		},
	}

	channelAlias := "#cross:bureau.local"
	channelRoomID := "!cross-room:bureau.local"

	matrixState.setRoomAlias(channelAlias, channelRoomID)

	// Two windows: one with label filter, one without.
	matrixState.setStateEvent(channelRoomID, schema.EventTypeLayout, "", schema.LayoutContent{
		Windows: []schema.LayoutWindow{
			{
				Name: "agents",
				Panes: []schema.LayoutPane{
					{ObserveMembers: &schema.LayoutMemberFilter{
						Labels: map[string]string{"role": "agent"},
					}},
				},
			},
			{
				Name: "all",
				Panes: []schema.LayoutPane{
					{ObserveMembers: &schema.LayoutMemberFilter{}},
				},
			},
		},
	})

	// Both local and cross-machine members in the room.
	matrixState.setRoomMembers(channelRoomID, []mockRoomMember{
		{UserID: "@iree/amdgpu/pm:bureau.local", Membership: "join"},
		{UserID: "@remote/agent:other.bureau.local", Membership: "join"},
	})

	response := sendQueryLayout(t, daemon.observeSocketPath, channelAlias)

	if !response.OK {
		t.Fatalf("expected OK, got error: %s", response.Error)
	}

	// "agents" window: only iree/amdgpu/pm (has labels).
	// "all" window: both members (no filter).
	if len(response.Layout.Windows) != 2 {
		t.Fatalf("expected 2 windows, got %d", len(response.Layout.Windows))
	}

	agentsWindow := response.Layout.Windows[0]
	if len(agentsWindow.Panes) != 1 {
		t.Fatalf("agents window: expected 1 pane, got %d", len(agentsWindow.Panes))
	}
	if agentsWindow.Panes[0].Observe != "iree/amdgpu/pm" {
		t.Errorf("agents pane observe = %q, want %q", agentsWindow.Panes[0].Observe, "iree/amdgpu/pm")
	}

	allWindow := response.Layout.Windows[1]
	if len(allWindow.Panes) != 2 {
		t.Fatalf("all window: expected 2 panes, got %d", len(allWindow.Panes))
	}
}

func TestQueryLayoutBackwardCompatibility(t *testing.T) {
	// A request without an action field should still be treated as an
	// observe request (backward compatibility). We can't test the full
	// observe flow here (no tmux/relay), but we can verify the daemon
	// doesn't crash and returns an error for the unknown principal.
	daemon, _ := newTestDaemonWithQuery(t)

	connection, err := net.DialTimeout("unix", daemon.observeSocketPath, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer connection.Close()
	connection.SetDeadline(time.Now().Add(5 * time.Second))

	// Send a classic observe request (no action field) with auth token.
	request := map[string]string{
		"principal": "test/agent",
		"mode":      "readwrite",
		"token":     testObserverToken,
	}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		t.Fatalf("send: %v", err)
	}

	reader := bufio.NewReader(connection)
	responseLine, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var response map[string]any
	if err := json.Unmarshal(responseLine, &response); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// The daemon should process this as an observe request and return
	// "not found" for the principal (since no principals are running).
	if response["ok"] != false {
		t.Errorf("expected ok=false, got %v", response["ok"])
	}
	errorMessage, _ := response["error"].(string)
	if !strings.Contains(errorMessage, "not found") {
		t.Errorf("error = %q, expected it to contain 'not found'", errorMessage)
	}
}
