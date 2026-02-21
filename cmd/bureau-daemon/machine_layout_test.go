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

// sendMachineLayout sends a query_machine_layout request to the daemon's
// observe socket and returns the parsed response.
func sendMachineLayout(t *testing.T, socketPath string) observe.QueryLayoutResponse {
	t.Helper()

	connection, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		t.Fatalf("dial observe socket: %v", err)
	}
	defer connection.Close()

	connection.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:realclock // kernel I/O deadline

	request := observe.MachineLayoutRequest{
		Action:   "query_machine_layout",
		Observer: testObserverUserID,
		Token:    testObserverToken,
	}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		t.Fatalf("send query_machine_layout request: %v", err)
	}

	reader := bufio.NewReader(connection)
	responseLine, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read query_machine_layout response: %v", err)
	}

	var response observe.QueryLayoutResponse
	if err := json.Unmarshal(responseLine, &response); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	return response
}

func TestMachineLayoutBasic(t *testing.T) {
	daemon, _ := newTestDaemonWithQuery(t)

	// Mark three principals as running with permissive observe allowances.
	for _, localpart := range []string{"iree/amdgpu/test", "iree/amdgpu/pm", "iree/amdgpu/codegen"} {
		entity := testEntity(t, daemon.fleet, localpart)
		daemon.running[entity] = true
		daemon.authorizationIndex.SetPrincipal(entity.UserID(), permissiveObserveAllowances)
	}

	response := sendMachineLayout(t, daemon.observeSocketPath)

	if !response.OK {
		t.Fatalf("expected OK, got error: %s", response.Error)
	}
	if response.Layout == nil {
		t.Fatal("response layout is nil")
	}
	expectedMachine := daemon.machine.Localpart()
	if response.Machine != expectedMachine {
		t.Errorf("machine = %q, want %q", response.Machine, expectedMachine)
	}
	if len(response.Layout.Windows) != 1 {
		t.Fatalf("window count = %d, want 1", len(response.Layout.Windows))
	}

	window := response.Layout.Windows[0]
	if window.Name != expectedMachine {
		t.Errorf("window name = %q, want %q", window.Name, expectedMachine)
	}
	if len(window.Panes) != 3 {
		t.Fatalf("pane count = %d, want 3", len(window.Panes))
	}

	// Verify alphabetical sort order.
	if window.Panes[0].Observe != "iree/amdgpu/codegen" {
		t.Errorf("pane 0 observe = %q, want %q", window.Panes[0].Observe, "iree/amdgpu/codegen")
	}
	if window.Panes[1].Observe != "iree/amdgpu/pm" {
		t.Errorf("pane 1 observe = %q, want %q", window.Panes[1].Observe, "iree/amdgpu/pm")
	}
	if window.Panes[2].Observe != "iree/amdgpu/test" {
		t.Errorf("pane 2 observe = %q, want %q", window.Panes[2].Observe, "iree/amdgpu/test")
	}

	// First pane: no split. Subsequent: vertical.
	if window.Panes[0].Split != "" {
		t.Errorf("pane 0 split = %q, want empty", window.Panes[0].Split)
	}
	if window.Panes[1].Split != "vertical" {
		t.Errorf("pane 1 split = %q, want %q", window.Panes[1].Split, "vertical")
	}
	if window.Panes[2].Split != "vertical" {
		t.Errorf("pane 2 split = %q, want %q", window.Panes[2].Split, "vertical")
	}
}

func TestMachineLayoutAuthFiltering(t *testing.T) {
	daemon, _ := newTestDaemonWithQuery(t)

	// Restrictive observe allowances: only observers matching "iree/**"
	// may observe. The test observer is "@ops/test-observer:bureau.local"
	// (localpart "ops/test-observer"), which does NOT match "iree/**".
	// So no principals are visible to this observer, and the daemon
	// should return an error.
	restrictedPolicy := schema.AuthorizationPolicy{
		Allowances: []schema.Allowance{
			{Actions: []string{"observe"}, Actors: []string{"iree/**:**"}},
			{Actions: []string{"observe/read-write"}, Actors: []string{"iree/**:**"}},
		},
	}
	for _, localpart := range []string{"iree/amdgpu/pm", "service/stt/whisper", "infra/ci/runner"} {
		entity := testEntity(t, daemon.fleet, localpart)
		daemon.running[entity] = true
		daemon.authorizationIndex.SetPrincipal(entity.UserID(), restrictedPolicy)
	}

	response := sendMachineLayout(t, daemon.observeSocketPath)

	if response.OK {
		t.Fatal("expected error (observer not in observe allowances), got OK")
	}
}

func TestMachineLayoutPerPrincipalAuthFiltering(t *testing.T) {
	daemon, _ := newTestDaemonWithQuery(t)

	// Most principals allow any observer. secret/agent restricts
	// observation to a specific admin account that is NOT our test
	// observer, so it should be filtered from the layout.
	for _, localpart := range []string{"iree/amdgpu/pm", "iree/amdgpu/codegen"} {
		entity := testEntity(t, daemon.fleet, localpart)
		daemon.running[entity] = true
		daemon.authorizationIndex.SetPrincipal(entity.UserID(), permissiveObserveAllowances)
	}
	secretEntity := testEntity(t, daemon.fleet, "secret/agent")
	daemon.running[secretEntity] = true
	daemon.authorizationIndex.SetPrincipal(secretEntity.UserID(), schema.AuthorizationPolicy{
		Allowances: []schema.Allowance{
			{Actions: []string{"observe"}, Actors: []string{"secret-admin:bureau.local"}},
		},
	})

	response := sendMachineLayout(t, daemon.observeSocketPath)

	if !response.OK {
		t.Fatalf("expected OK, got error: %s", response.Error)
	}

	// secret/agent should be filtered out — its observe allowance only
	// permits @secret-admin, not our test observer.
	if len(response.Layout.Windows) != 1 {
		t.Fatalf("window count = %d, want 1", len(response.Layout.Windows))
	}
	panes := response.Layout.Windows[0].Panes
	if len(panes) != 2 {
		t.Fatalf("pane count = %d, want 2 (secret/agent filtered)", len(panes))
	}
	if panes[0].Observe != "iree/amdgpu/codegen" {
		t.Errorf("pane 0 observe = %q, want %q", panes[0].Observe, "iree/amdgpu/codegen")
	}
	if panes[1].Observe != "iree/amdgpu/pm" {
		t.Errorf("pane 1 observe = %q, want %q", panes[1].Observe, "iree/amdgpu/pm")
	}
}

func TestMachineLayoutNoPrincipals(t *testing.T) {
	daemon, _ := newTestDaemonWithQuery(t)

	// No principals running.
	response := sendMachineLayout(t, daemon.observeSocketPath)

	if response.OK {
		t.Error("expected error for no running principals, got OK")
	}
	if response.Error == "" {
		t.Error("expected error message, got empty string")
	}
}

func TestMachineLayoutMachineName(t *testing.T) {
	daemon, _ := newTestDaemonWithQuery(t)
	testAgentEntity := testEntity(t, daemon.fleet, "test/agent")
	daemon.running[testAgentEntity] = true
	daemon.authorizationIndex.SetPrincipal(testAgentEntity.UserID(), permissiveObserveAllowances)

	response := sendMachineLayout(t, daemon.observeSocketPath)

	if !response.OK {
		t.Fatalf("expected OK, got error: %s", response.Error)
	}
	expectedMachine := daemon.machine.Localpart()
	if response.Machine != expectedMachine {
		t.Errorf("machine = %q, want %q", response.Machine, expectedMachine)
	}
}
