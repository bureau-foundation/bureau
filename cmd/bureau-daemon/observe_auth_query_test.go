// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/observe"
)

// sendAuthorizationQuery sends a query_authorization request to the daemon's
// observe socket and returns the parsed response.
func sendAuthorizationQuery(t *testing.T, socketPath string, actor, authAction, target string) observe.AuthorizationResponse {
	t.Helper()

	connection, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		t.Fatalf("dial observe socket: %v", err)
	}
	defer connection.Close()

	connection.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:realclock // kernel I/O deadline

	request := map[string]any{
		"action":      "query_authorization",
		"actor":       actor,
		"auth_action": authAction,
		"target":      target,
		"observer":    testObserverUserID,
		"token":       testObserverToken,
	}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		t.Fatalf("send query_authorization request: %v", err)
	}

	reader := bufio.NewReader(connection)
	responseLine, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read query_authorization response: %v", err)
	}

	var response observe.AuthorizationResponse
	if err := json.Unmarshal(responseLine, &response); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	return response
}

// sendGrantsQuery sends a query_grants request to the daemon's observe
// socket and returns the parsed response.
func sendGrantsQuery(t *testing.T, socketPath string, principal string) observe.GrantsResponse {
	t.Helper()

	connection, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		t.Fatalf("dial observe socket: %v", err)
	}
	defer connection.Close()

	connection.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:realclock // kernel I/O deadline

	request := map[string]any{
		"action":    "query_grants",
		"principal": principal,
		"observer":  testObserverUserID,
		"token":     testObserverToken,
	}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		t.Fatalf("send query_grants request: %v", err)
	}

	reader := bufio.NewReader(connection)
	responseLine, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read query_grants response: %v", err)
	}

	var response observe.GrantsResponse
	if err := json.Unmarshal(responseLine, &response); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	return response
}

// sendAllowancesQuery sends a query_allowances request to the daemon's
// observe socket and returns the parsed response.
func sendAllowancesQuery(t *testing.T, socketPath string, principal string) observe.AllowancesResponse {
	t.Helper()

	connection, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		t.Fatalf("dial observe socket: %v", err)
	}
	defer connection.Close()

	connection.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:realclock // kernel I/O deadline

	request := map[string]any{
		"action":    "query_allowances",
		"principal": principal,
		"observer":  testObserverUserID,
		"token":     testObserverToken,
	}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		t.Fatalf("send query_allowances request: %v", err)
	}

	reader := bufio.NewReader(connection)
	responseLine, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read query_allowances response: %v", err)
	}

	var response observe.AllowancesResponse
	if err := json.Unmarshal(responseLine, &response); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	return response
}

func TestQueryAuthorizationAllow(t *testing.T) {
	daemon, _ := newTestDaemonWithQuery(t)

	// Set up actor with observe grant and target with observe allowance.
	daemon.authorizationIndex.SetPrincipal(testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(), schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{"observe/**"}, Targets: []string{"**/iree/*/*:**"}},
		},
	})
	daemon.authorizationIndex.SetPrincipal(testEntity(t, daemon.fleet, "iree/amdgpu/compiler").UserID(), schema.AuthorizationPolicy{
		Allowances: []schema.Allowance{
			{Actions: []string{"observe/**"}, Actors: []string{"**/iree/*/*:**"}},
		},
	})

	actorUserID := testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID().String()
	targetUserID := testEntity(t, daemon.fleet, "iree/amdgpu/compiler").UserID().String()
	response := sendAuthorizationQuery(t, daemon.observeSocketPath,
		actorUserID, "observe", targetUserID)

	if !response.OK {
		t.Fatalf("expected OK, got error: %s", response.Error)
	}
	if response.Decision != "allow" {
		t.Errorf("decision = %q, want allow", response.Decision)
	}
	if response.MatchedGrant == nil {
		t.Error("expected matched grant")
	}
	if response.MatchedAllowance == nil {
		t.Error("expected matched allowance")
	}
	if len(response.ActorGrants) != 1 {
		t.Errorf("actor grants = %d, want 1", len(response.ActorGrants))
	}
	if len(response.TargetAllowances) != 1 {
		t.Errorf("target allowances = %d, want 1", len(response.TargetAllowances))
	}
}

func TestQueryAuthorizationDenyNoGrant(t *testing.T) {
	daemon, _ := newTestDaemonWithQuery(t)

	// Actor has no grants at all.
	daemon.authorizationIndex.SetPrincipal(testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(), schema.AuthorizationPolicy{})

	actorUserID := testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID().String()
	targetUserID := testEntity(t, daemon.fleet, "iree/amdgpu/compiler").UserID().String()
	response := sendAuthorizationQuery(t, daemon.observeSocketPath,
		actorUserID, "observe", targetUserID)

	if !response.OK {
		t.Fatalf("expected OK, got error: %s", response.Error)
	}
	if response.Decision != "deny" {
		t.Errorf("decision = %q, want deny", response.Decision)
	}
	if response.Reason != "no matching grant" {
		t.Errorf("reason = %q, want 'no matching grant'", response.Reason)
	}
	if response.MatchedGrant != nil {
		t.Error("expected no matched grant")
	}
}

func TestQueryAuthorizationDenyExplicitDenial(t *testing.T) {
	daemon, _ := newTestDaemonWithQuery(t)

	// Actor has a broad grant but also an explicit denial for the action.
	daemon.authorizationIndex.SetPrincipal(testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(), schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{"**"}, Targets: []string{"**:**"}},
		},
		Denials: []schema.Denial{
			{Actions: []string{"observe/read-write"}, Targets: []string{"**/secret/*:**"}},
		},
	})

	actorUserID := testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID().String()
	targetUserID := testEntity(t, daemon.fleet, "secret/database").UserID().String()
	response := sendAuthorizationQuery(t, daemon.observeSocketPath,
		actorUserID, "observe/read-write", targetUserID)

	if !response.OK {
		t.Fatalf("expected OK, got error: %s", response.Error)
	}
	if response.Decision != "deny" {
		t.Errorf("decision = %q, want deny", response.Decision)
	}
	if response.Reason != "explicit denial" {
		t.Errorf("reason = %q, want 'explicit denial'", response.Reason)
	}
	if response.MatchedGrant == nil {
		t.Error("expected matched grant (grant matched before denial)")
	}
	if response.MatchedDenial == nil {
		t.Error("expected matched denial")
	}
	if len(response.ActorDenials) != 1 {
		t.Errorf("actor denials = %d, want 1", len(response.ActorDenials))
	}
}

func TestQueryAuthorizationSelfService(t *testing.T) {
	daemon, _ := newTestDaemonWithQuery(t)

	daemon.authorizationIndex.SetPrincipal(testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(), schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{"command/pipeline/list"}},
		},
	})

	// Self-service action: no target.
	actorUserID := testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID().String()
	response := sendAuthorizationQuery(t, daemon.observeSocketPath,
		actorUserID, "command/pipeline/list", "")

	if !response.OK {
		t.Fatalf("expected OK, got error: %s", response.Error)
	}
	if response.Decision != "allow" {
		t.Errorf("decision = %q, want allow", response.Decision)
	}
	// Target-side fields should be empty for self-service.
	if len(response.TargetAllowances) != 0 {
		t.Errorf("target allowances = %d, want 0 for self-service", len(response.TargetAllowances))
	}
}

func TestQueryAuthorizationMissingActor(t *testing.T) {
	daemon, _ := newTestDaemonWithQuery(t)

	connection, err := net.DialTimeout("unix", daemon.observeSocketPath, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer connection.Close()
	connection.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:realclock // kernel I/O deadline

	request := map[string]any{
		"action":      "query_authorization",
		"auth_action": "observe",
		"observer":    testObserverUserID,
		"token":       testObserverToken,
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
	if response["ok"] != false {
		t.Errorf("expected ok=false, got %v", response["ok"])
	}
}

func TestQueryGrantsReturnsPolicy(t *testing.T) {
	daemon, _ := newTestDaemonWithQuery(t)

	daemon.authorizationIndex.SetPrincipal(testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(), schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{"observe/**"}, Targets: []string{"**/iree/**:**"}},
			{Actions: []string{"command/pipeline/list"}},
		},
		Denials: []schema.Denial{
			{Actions: []string{"interrupt/**"}},
		},
	})

	pmUserID := testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID().String()
	response := sendGrantsQuery(t, daemon.observeSocketPath, pmUserID)

	if !response.OK {
		t.Fatalf("expected OK, got error: %s", response.Error)
	}
	if response.Principal != pmUserID {
		t.Errorf("principal = %q, want %q", response.Principal, pmUserID)
	}
	if len(response.Grants) != 2 {
		t.Errorf("grants = %d, want 2", len(response.Grants))
	}
	if len(response.Denials) != 1 {
		t.Errorf("denials = %d, want 1", len(response.Denials))
	}
}

// TestQueryGrantsSourceProvenance verifies that the Source field on grants
// and denials is preserved through the observe wire protocol.
func TestQueryGrantsSourceProvenance(t *testing.T) {
	daemon, _ := newTestDaemonWithQuery(t)

	daemon.authorizationIndex.SetPrincipal(testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(), schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{"observe/**"}, Targets: []string{"**/iree/**:**"}, Source: schema.SourceMachineDefault},
			{Actions: []string{"command/pipeline/list"}, Source: schema.SourcePrincipal},
		},
		Denials: []schema.Denial{
			{Actions: []string{"interrupt/**"}, Source: schema.SourceMachineDefault},
		},
	})

	pmUserID := testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID().String()
	response := sendGrantsQuery(t, daemon.observeSocketPath, pmUserID)

	if !response.OK {
		t.Fatalf("expected OK, got error: %s", response.Error)
	}
	if len(response.Grants) != 2 {
		t.Fatalf("grants = %d, want 2", len(response.Grants))
	}
	if response.Grants[0].Source != schema.SourceMachineDefault {
		t.Errorf("grants[0].Source = %q, want %q", response.Grants[0].Source, schema.SourceMachineDefault)
	}
	if response.Grants[1].Source != schema.SourcePrincipal {
		t.Errorf("grants[1].Source = %q, want %q", response.Grants[1].Source, schema.SourcePrincipal)
	}
	if len(response.Denials) != 1 {
		t.Fatalf("denials = %d, want 1", len(response.Denials))
	}
	if response.Denials[0].Source != schema.SourceMachineDefault {
		t.Errorf("denials[0].Source = %q, want %q", response.Denials[0].Source, schema.SourceMachineDefault)
	}
}

func TestQueryGrantsEmptyPrincipal(t *testing.T) {
	daemon, _ := newTestDaemonWithQuery(t)

	// Principal not in index — should return empty lists, not error.
	nonexistentUserID := testEntity(t, daemon.fleet, "nonexistent/agent").UserID().String()
	response := sendGrantsQuery(t, daemon.observeSocketPath, nonexistentUserID)

	if !response.OK {
		t.Fatalf("expected OK, got error: %s", response.Error)
	}
	if len(response.Grants) != 0 {
		t.Errorf("grants = %d, want 0", len(response.Grants))
	}
	if len(response.Denials) != 0 {
		t.Errorf("denials = %d, want 0", len(response.Denials))
	}
}

func TestQueryGrantsMissingPrincipal(t *testing.T) {
	daemon, _ := newTestDaemonWithQuery(t)

	connection, err := net.DialTimeout("unix", daemon.observeSocketPath, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer connection.Close()
	connection.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:realclock // kernel I/O deadline

	request := map[string]any{
		"action":   "query_grants",
		"observer": testObserverUserID,
		"token":    testObserverToken,
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
	if response["ok"] != false {
		t.Errorf("expected ok=false, got %v", response["ok"])
	}
}

func TestQueryAllowancesReturnsPolicy(t *testing.T) {
	daemon, _ := newTestDaemonWithQuery(t)

	daemon.authorizationIndex.SetPrincipal(testEntity(t, daemon.fleet, "iree/amdgpu/compiler").UserID(), schema.AuthorizationPolicy{
		Allowances: []schema.Allowance{
			{Actions: []string{"observe/**"}, Actors: []string{"iree/**:**"}},
			{Actions: []string{"interrupt"}, Actors: []string{"iree/amdgpu/pm:**"}},
		},
		AllowanceDenials: []schema.AllowanceDenial{
			{Actions: []string{"observe/read-write"}, Actors: []string{"untrusted/**:**"}},
		},
	})

	principalUserID := testEntity(t, daemon.fleet, "iree/amdgpu/compiler").UserID().String()
	response := sendAllowancesQuery(t, daemon.observeSocketPath, principalUserID)

	if !response.OK {
		t.Fatalf("expected OK, got error: %s", response.Error)
	}
	if response.Principal != principalUserID {
		t.Errorf("principal = %q, want %q", response.Principal, principalUserID)
	}
	if len(response.Allowances) != 2 {
		t.Errorf("allowances = %d, want 2", len(response.Allowances))
	}
	if len(response.AllowanceDenials) != 1 {
		t.Errorf("allowance denials = %d, want 1", len(response.AllowanceDenials))
	}
}

func TestQueryAllowancesMissingPrincipal(t *testing.T) {
	daemon, _ := newTestDaemonWithQuery(t)

	connection, err := net.DialTimeout("unix", daemon.observeSocketPath, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer connection.Close()
	connection.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:realclock // kernel I/O deadline

	request := map[string]any{
		"action":   "query_allowances",
		"observer": testObserverUserID,
		"token":    testObserverToken,
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
	if response["ok"] != false {
		t.Errorf("expected ok=false, got %v", response["ok"])
	}
}
