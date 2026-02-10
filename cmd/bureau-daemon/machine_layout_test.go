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

	connection.SetDeadline(time.Now().Add(5 * time.Second))

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

	// Mark three principals as running.
	daemon.running["iree/amdgpu/test"] = true
	daemon.running["iree/amdgpu/pm"] = true
	daemon.running["iree/amdgpu/codegen"] = true

	response := sendMachineLayout(t, daemon.observeSocketPath)

	if !response.OK {
		t.Fatalf("expected OK, got error: %s", response.Error)
	}
	if response.Layout == nil {
		t.Fatal("response layout is nil")
	}
	if response.Machine != "machine/test" {
		t.Errorf("machine = %q, want %q", response.Machine, "machine/test")
	}
	if len(response.Layout.Windows) != 1 {
		t.Fatalf("window count = %d, want 1", len(response.Layout.Windows))
	}

	window := response.Layout.Windows[0]
	if window.Name != "machine/test" {
		t.Errorf("window name = %q, want %q", window.Name, "machine/test")
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

	// Restrictive DefaultObservePolicy: AllowedObservers only permits
	// observers matching "iree/**". The test observer is
	// "@ops/test-observer:bureau.local" (localpart "ops/test-observer"),
	// which does NOT match "iree/**". So no principals are visible to
	// this observer, and the daemon should return an error.
	daemon.lastConfig = &schema.MachineConfig{
		DefaultObservePolicy: &schema.ObservePolicy{
			AllowedObservers:   []string{"iree/**"},
			ReadWriteObservers: []string{"iree/**"},
		},
		Principals: []schema.PrincipalAssignment{
			{Localpart: "iree/amdgpu/pm"},
			{Localpart: "service/stt/whisper"},
			{Localpart: "infra/ci/runner"},
		},
	}

	daemon.running["iree/amdgpu/pm"] = true
	daemon.running["service/stt/whisper"] = true
	daemon.running["infra/ci/runner"] = true

	response := sendMachineLayout(t, daemon.observeSocketPath)

	if response.OK {
		t.Fatal("expected error (observer not in AllowedObservers), got OK")
	}
}

func TestMachineLayoutPerPrincipalAuthFiltering(t *testing.T) {
	daemon, _ := newTestDaemonWithQuery(t)

	// Default policy allows everyone. Per-principal policy on one
	// principal restricts it to a different observer.
	daemon.lastConfig = &schema.MachineConfig{
		DefaultObservePolicy: &schema.ObservePolicy{
			AllowedObservers:   []string{"**"},
			ReadWriteObservers: []string{"**"},
		},
		Principals: []schema.PrincipalAssignment{
			{Localpart: "iree/amdgpu/pm"},
			{
				Localpart: "secret/agent",
				ObservePolicy: &schema.ObservePolicy{
					AllowedObservers: []string{"@secret-admin:bureau.local"},
				},
			},
			{Localpart: "iree/amdgpu/codegen"},
		},
	}

	daemon.running["iree/amdgpu/pm"] = true
	daemon.running["secret/agent"] = true
	daemon.running["iree/amdgpu/codegen"] = true

	response := sendMachineLayout(t, daemon.observeSocketPath)

	if !response.OK {
		t.Fatalf("expected OK, got error: %s", response.Error)
	}

	// secret/agent should be filtered out — its ObservePolicy only
	// allows @secret-admin, not our test observer.
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
	daemon.running["test/agent"] = true

	response := sendMachineLayout(t, daemon.observeSocketPath)

	if !response.OK {
		t.Fatalf("expected OK, got error: %s", response.Error)
	}
	if response.Machine != "machine/test" {
		t.Errorf("machine = %q, want %q", response.Machine, "machine/test")
	}
}
