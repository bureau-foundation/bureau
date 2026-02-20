// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ref_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
)

const testServer = "bureau.local"

func TestNamespaceConstruction(t *testing.T) {
	tests := []struct {
		name      string
		server    string
		namespace string
		wantErr   bool
	}{
		{name: "simple", server: testServer, namespace: "my_bureau"},
		{name: "short", server: testServer, namespace: "acme"},
		{name: "with-dash", server: testServer, namespace: "my-ns"},
		{name: "with-dot", server: testServer, namespace: "my.ns"},
		{name: "with-digits", server: testServer, namespace: "bureau42"},
		{name: "empty-server", server: "", namespace: "my_bureau", wantErr: true},
		{name: "empty-namespace", server: testServer, namespace: "", wantErr: true},
		{name: "slash-in-namespace", server: testServer, namespace: "my/bureau", wantErr: true},
		{name: "uppercase", server: testServer, namespace: "MyBureau", wantErr: true},
		{name: "space-in-name", server: testServer, namespace: "my bureau", wantErr: true},
		{name: "dotdot", server: testServer, namespace: "..", wantErr: true},
		{name: "leading-dot", server: testServer, namespace: ".hidden", wantErr: true},
		{name: "at-in-server", server: "user@server", namespace: "ns", wantErr: true},
		{name: "hash-in-server", server: "#server", namespace: "ns", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns, err := ref.NewNamespace(tt.server, tt.namespace)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got namespace %v", ns)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ns.Server() != tt.server {
				t.Errorf("Server() = %q, want %q", ns.Server(), tt.server)
			}
			if ns.Name() != tt.namespace {
				t.Errorf("Name() = %q, want %q", ns.Name(), tt.namespace)
			}
			if ns.String() != tt.namespace {
				t.Errorf("String() = %q, want %q", ns.String(), tt.namespace)
			}
			if ns.IsZero() {
				t.Error("IsZero() = true for valid namespace")
			}
		})
	}
}

func TestNamespaceAliases(t *testing.T) {
	ns, err := ref.NewNamespace(testServer, "my_bureau")
	if err != nil {
		t.Fatalf("NewNamespace: %v", err)
	}

	type aliasTest struct {
		method string
		got    string
		want   string
	}
	tests := []aliasTest{
		{"SpaceAlias", ns.SpaceAlias(), "#my_bureau:" + testServer},
		{"SystemRoomAlias", ns.SystemRoomAlias(), "#my_bureau/system:" + testServer},
		{"TemplateRoomAlias", ns.TemplateRoomAlias(), "#my_bureau/template:" + testServer},
		{"PipelineRoomAlias", ns.PipelineRoomAlias(), "#my_bureau/pipeline:" + testServer},
		{"ArtifactRoomAlias", ns.ArtifactRoomAlias(), "#my_bureau/artifact:" + testServer},
		{"SpaceAliasLocalpart", ns.SpaceAliasLocalpart(), "my_bureau"},
		{"SystemRoomAliasLocalpart", ns.SystemRoomAliasLocalpart(), "my_bureau/system"},
		{"TemplateRoomAliasLocalpart", ns.TemplateRoomAliasLocalpart(), "my_bureau/template"},
		{"PipelineRoomAliasLocalpart", ns.PipelineRoomAliasLocalpart(), "my_bureau/pipeline"},
		{"ArtifactRoomAliasLocalpart", ns.ArtifactRoomAliasLocalpart(), "my_bureau/artifact"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s() = %q, want %q", tt.method, tt.got, tt.want)
		}
	}
}

func TestFleetConstruction(t *testing.T) {
	ns, err := ref.NewNamespace(testServer, "my_bureau")
	if err != nil {
		t.Fatalf("NewNamespace: %v", err)
	}

	tests := []struct {
		name    string
		fleet   string
		wantLp  string
		wantErr bool
	}{
		{name: "prod", fleet: "prod", wantLp: "my_bureau/fleet/prod"},
		{name: "dev", fleet: "dev", wantLp: "my_bureau/fleet/dev"},
		{name: "dashed", fleet: "us-east-gpu", wantLp: "my_bureau/fleet/us-east-gpu"},
		{name: "empty", fleet: "", wantErr: true},
		{name: "slash", fleet: "us/east", wantErr: true},
		{name: "uppercase", fleet: "Prod", wantErr: true},
		{name: "dotdot", fleet: "..", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fleet, err := ref.NewFleet(ns, tt.fleet)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got fleet %v", fleet)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if fleet.Localpart() != tt.wantLp {
				t.Errorf("Localpart() = %q, want %q", fleet.Localpart(), tt.wantLp)
			}
			if fleet.FleetName() != tt.fleet {
				t.Errorf("FleetName() = %q, want %q", fleet.FleetName(), tt.fleet)
			}
			if fleet.Namespace().Name() != "my_bureau" {
				t.Errorf("Namespace().Name() = %q, want %q", fleet.Namespace().Name(), "my_bureau")
			}
			if fleet.Server() != testServer {
				t.Errorf("Server() = %q, want %q", fleet.Server(), testServer)
			}
			if fleet.String() != tt.wantLp {
				t.Errorf("String() = %q, want %q", fleet.String(), tt.wantLp)
			}
			if fleet.IsZero() {
				t.Error("IsZero() = true for valid fleet")
			}
		})
	}
}

func TestFleetZeroNamespace(t *testing.T) {
	var ns ref.Namespace
	_, err := ref.NewFleet(ns, "prod")
	if err == nil {
		t.Fatal("expected error for zero-value namespace")
	}
}

func TestFleetAliases(t *testing.T) {
	ns, _ := ref.NewNamespace(testServer, "my_bureau")
	fleet, err := ref.NewFleet(ns, "prod")
	if err != nil {
		t.Fatalf("NewFleet: %v", err)
	}

	type aliasTest struct {
		method string
		got    string
		want   string
	}
	tests := []aliasTest{
		{"RoomAlias", fleet.RoomAlias(), "#my_bureau/fleet/prod:" + testServer},
		{"MachineRoomAlias", fleet.MachineRoomAlias(), "#my_bureau/fleet/prod/machine:" + testServer},
		{"ServiceRoomAlias", fleet.ServiceRoomAlias(), "#my_bureau/fleet/prod/service:" + testServer},
		{"MachineRoomAliasLocalpart", fleet.MachineRoomAliasLocalpart(), "my_bureau/fleet/prod/machine"},
		{"ServiceRoomAliasLocalpart", fleet.ServiceRoomAliasLocalpart(), "my_bureau/fleet/prod/service"},
		{"RunDir", fleet.RunDir("/run/bureau"), "/run/bureau/fleet/prod"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s() = %q, want %q", tt.method, tt.got, tt.want)
		}
	}
}

func TestParseFleet(t *testing.T) {
	tests := []struct {
		name      string
		localpart string
		server    string
		wantNS    string
		wantFleet string
		wantErr   bool
	}{
		{
			name:      "standard",
			localpart: "my_bureau/fleet/prod",
			server:    testServer,
			wantNS:    "my_bureau",
			wantFleet: "prod",
		},
		{
			name:      "different-namespace",
			localpart: "acme/fleet/staging",
			server:    testServer,
			wantNS:    "acme",
			wantFleet: "staging",
		},
		{
			name:      "namespace-named-fleet",
			localpart: "fleet/fleet/fleet",
			server:    testServer,
			wantNS:    "fleet",
			wantFleet: "fleet",
		},
		{
			name:      "missing-fleet-segment",
			localpart: "my_bureau/prod",
			server:    testServer,
			wantErr:   true,
		},
		{
			name:      "trailing-segments",
			localpart: "my_bureau/fleet/prod/machine",
			server:    testServer,
			wantErr:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fleet, err := ref.ParseFleet(tt.localpart, tt.server)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got fleet %v", fleet)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if fleet.Namespace().Name() != tt.wantNS {
				t.Errorf("Namespace().Name() = %q, want %q", fleet.Namespace().Name(), tt.wantNS)
			}
			if fleet.FleetName() != tt.wantFleet {
				t.Errorf("FleetName() = %q, want %q", fleet.FleetName(), tt.wantFleet)
			}
		})
	}
}

func TestParseFleetRoomAlias(t *testing.T) {
	fleet, err := ref.ParseFleetRoomAlias("#my_bureau/fleet/prod:" + testServer)
	if err != nil {
		t.Fatalf("ParseFleetRoomAlias: %v", err)
	}
	if fleet.Localpart() != "my_bureau/fleet/prod" {
		t.Errorf("Localpart() = %q", fleet.Localpart())
	}
	if fleet.Server() != testServer {
		t.Errorf("Server() = %q", fleet.Server())
	}
}

func TestMachineConstruction(t *testing.T) {
	ns, _ := ref.NewNamespace(testServer, "my_bureau")
	fleet, _ := ref.NewFleet(ns, "prod")

	tests := []struct {
		name    string
		machine string
		wantLp  string
		wantUID string
		wantRA  string
		wantErr bool
	}{
		{
			name:    "simple",
			machine: "gpu-box",
			wantLp:  "my_bureau/fleet/prod/machine/gpu-box",
			wantUID: "@my_bureau/fleet/prod/machine/gpu-box:" + testServer,
			wantRA:  "#my_bureau/fleet/prod/machine/gpu-box:" + testServer,
		},
		{
			name:    "workstation",
			machine: "workstation",
			wantLp:  "my_bureau/fleet/prod/machine/workstation",
			wantUID: "@my_bureau/fleet/prod/machine/workstation:" + testServer,
			wantRA:  "#my_bureau/fleet/prod/machine/workstation:" + testServer,
		},
		{name: "empty", machine: "", wantErr: true},
		{name: "uppercase", machine: "GPU-Box", wantErr: true},
		{name: "leading-dot", machine: ".hidden", wantErr: true},
		{name: "path-traversal", machine: "..", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := ref.NewMachine(fleet, tt.machine)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got machine %v", m)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if m.Localpart() != tt.wantLp {
				t.Errorf("Localpart() = %q, want %q", m.Localpart(), tt.wantLp)
			}
			if m.UserID() != tt.wantUID {
				t.Errorf("UserID() = %q, want %q", m.UserID(), tt.wantUID)
			}
			if m.RoomAlias() != tt.wantRA {
				t.Errorf("RoomAlias() = %q, want %q", m.RoomAlias(), tt.wantRA)
			}
			if m.Name() != tt.machine {
				t.Errorf("Name() = %q, want %q", m.Name(), tt.machine)
			}
			if m.Server() != testServer {
				t.Errorf("Server() = %q, want %q", m.Server(), testServer)
			}
			if m.Fleet().FleetName() != "prod" {
				t.Errorf("Fleet().FleetName() = %q", m.Fleet().FleetName())
			}
			if m.IsZero() {
				t.Error("IsZero() = true for valid machine")
			}
		})
	}
}

func TestServiceHierarchicalName(t *testing.T) {
	ns, _ := ref.NewNamespace(testServer, "acme")
	fleet, _ := ref.NewFleet(ns, "staging")

	service, err := ref.NewService(fleet, "stt/whisper")
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	if service.Localpart() != "acme/fleet/staging/service/stt/whisper" {
		t.Errorf("Localpart() = %q", service.Localpart())
	}
	if service.Name() != "stt/whisper" {
		t.Errorf("Name() = %q", service.Name())
	}
	if service.UserID() != "@acme/fleet/staging/service/stt/whisper:"+testServer {
		t.Errorf("UserID() = %q", service.UserID())
	}
}

func TestAgentConstruction(t *testing.T) {
	ns, _ := ref.NewNamespace(testServer, "my_bureau")
	fleet, _ := ref.NewFleet(ns, "prod")

	agent, err := ref.NewAgent(fleet, "code-reviewer")
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	if agent.Localpart() != "my_bureau/fleet/prod/agent/code-reviewer" {
		t.Errorf("Localpart() = %q", agent.Localpart())
	}
	if agent.UserID() != "@my_bureau/fleet/prod/agent/code-reviewer:"+testServer {
		t.Errorf("UserID() = %q", agent.UserID())
	}
}

func TestParseMachineUserID(t *testing.T) {
	tests := []struct {
		name      string
		userID    string
		wantName  string
		wantFleet string
		wantNS    string
		wantErr   bool
	}{
		{
			name:      "valid",
			userID:    "@my_bureau/fleet/prod/machine/gpu-box:" + testServer,
			wantName:  "gpu-box",
			wantFleet: "prod",
			wantNS:    "my_bureau",
		},
		{
			name:    "wrong-entity-type",
			userID:  "@my_bureau/fleet/prod/service/stt:" + testServer,
			wantErr: true,
		},
		{
			name:    "missing-sigil",
			userID:  "my_bureau/fleet/prod/machine/gpu-box:" + testServer,
			wantErr: true,
		},
		{
			name:    "missing-server",
			userID:  "@my_bureau/fleet/prod/machine/gpu-box",
			wantErr: true,
		},
		{
			name:    "too-few-segments",
			userID:  "@my_bureau/machine/gpu-box:" + testServer,
			wantErr: true,
		},
		{
			name:    "empty-string",
			userID:  "",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := ref.ParseMachineUserID(tt.userID)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got machine %v", m)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if m.Name() != tt.wantName {
				t.Errorf("Name() = %q, want %q", m.Name(), tt.wantName)
			}
			if m.Fleet().FleetName() != tt.wantFleet {
				t.Errorf("Fleet().FleetName() = %q, want %q", m.Fleet().FleetName(), tt.wantFleet)
			}
			if m.Fleet().Namespace().Name() != tt.wantNS {
				t.Errorf("Namespace().Name() = %q, want %q", m.Fleet().Namespace().Name(), tt.wantNS)
			}
		})
	}
}

func TestParseServiceUserID(t *testing.T) {
	service, err := ref.ParseServiceUserID("@acme/fleet/staging/service/stt/whisper:" + testServer)
	if err != nil {
		t.Fatalf("ParseServiceUserID: %v", err)
	}
	if service.Name() != "stt/whisper" {
		t.Errorf("Name() = %q, want %q", service.Name(), "stt/whisper")
	}
	if service.Fleet().FleetName() != "staging" {
		t.Errorf("Fleet().FleetName() = %q", service.Fleet().FleetName())
	}
	if service.Fleet().Namespace().Name() != "acme" {
		t.Errorf("Namespace().Name() = %q", service.Fleet().Namespace().Name())
	}
}

func TestParseMachineLocalpart(t *testing.T) {
	machine, err := ref.ParseMachine("my_bureau/fleet/prod/machine/gpu-box", testServer)
	if err != nil {
		t.Fatalf("ParseMachine: %v", err)
	}
	if machine.Name() != "gpu-box" {
		t.Errorf("Name() = %q", machine.Name())
	}
	if machine.UserID() != "@my_bureau/fleet/prod/machine/gpu-box:"+testServer {
		t.Errorf("UserID() = %q", machine.UserID())
	}
}

func TestSocketPaths(t *testing.T) {
	ns, _ := ref.NewNamespace(testServer, "my_bureau")
	fleet, _ := ref.NewFleet(ns, "prod")
	machine, _ := ref.NewMachine(fleet, "gpu-box")
	service, _ := ref.NewService(fleet, "stt/whisper")

	runDir := fleet.RunDir("/run/bureau")

	type pathTest struct {
		label string
		got   string
		want  string
	}
	tests := []pathTest{
		{"machine.SocketPath", machine.SocketPath(runDir), "/run/bureau/fleet/prod/machine/gpu-box.sock"},
		{"machine.AdminSocketPath", machine.AdminSocketPath(runDir), "/run/bureau/fleet/prod/machine/gpu-box.admin.sock"},
		{"service.SocketPath", service.SocketPath(runDir), "/run/bureau/fleet/prod/service/stt/whisper.sock"},
		{"service.AdminSocketPath", service.AdminSocketPath(runDir), "/run/bureau/fleet/prod/service/stt/whisper.admin.sock"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %q, want %q", tt.label, tt.got, tt.want)
		}
	}
}

func TestAtToHashRule(t *testing.T) {
	ns, _ := ref.NewNamespace(testServer, "my_bureau")
	fleet, _ := ref.NewFleet(ns, "prod")
	machine, _ := ref.NewMachine(fleet, "gpu-box")

	userID := machine.UserID()       // @localpart:server
	roomAlias := machine.RoomAlias() // #localpart:server

	// The @ -> # rule: swap the sigil and you get the room.
	if "#"+userID[1:] != roomAlias {
		t.Errorf("@ -> # rule violated:\n  userID:    %s\n  roomAlias: %s", userID, roomAlias)
	}
}

func TestParseEntityUserID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		userID     string
		wantType   string
		wantName   string
		wantSocket string // relative to fleet RunDir
		wantAdmin  string
		wantErr    bool
	}{
		{
			name:       "machine",
			userID:     "@my_bureau/fleet/prod/machine/gpu-box:" + testServer,
			wantType:   "machine",
			wantName:   "gpu-box",
			wantSocket: "/run/bureau/fleet/prod/machine/gpu-box.sock",
			wantAdmin:  "/run/bureau/fleet/prod/machine/gpu-box.admin.sock",
		},
		{
			name:       "service",
			userID:     "@my_bureau/fleet/prod/service/ticket:" + testServer,
			wantType:   "service",
			wantName:   "ticket",
			wantSocket: "/run/bureau/fleet/prod/service/ticket.sock",
			wantAdmin:  "/run/bureau/fleet/prod/service/ticket.admin.sock",
		},
		{
			name:       "hierarchical_service",
			userID:     "@my_bureau/fleet/prod/service/stt/whisper:" + testServer,
			wantType:   "service",
			wantName:   "stt/whisper",
			wantSocket: "/run/bureau/fleet/prod/service/stt/whisper.sock",
			wantAdmin:  "/run/bureau/fleet/prod/service/stt/whisper.admin.sock",
		},
		{
			name:       "agent",
			userID:     "@my_bureau/fleet/prod/agent/code-reviewer:" + testServer,
			wantType:   "agent",
			wantName:   "code-reviewer",
			wantSocket: "/run/bureau/fleet/prod/agent/code-reviewer.sock",
			wantAdmin:  "/run/bureau/fleet/prod/agent/code-reviewer.admin.sock",
		},
		{
			name:    "non_fleet_user_id",
			userID:  "@admin:" + testServer,
			wantErr: true,
		},
		{
			name:    "legacy_localpart",
			userID:  "@machine/workstation:" + testServer,
			wantErr: true,
		},
		{
			name:    "missing_sigil",
			userID:  "my_bureau/fleet/prod/machine/gpu-box:" + testServer,
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			entity, err := ref.ParseEntityUserID(test.userID)
			if test.wantErr {
				if err == nil {
					t.Fatalf("expected error, got entity %v", entity)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if entity.EntityType() != test.wantType {
				t.Errorf("EntityType() = %q, want %q", entity.EntityType(), test.wantType)
			}
			if entity.Name() != test.wantName {
				t.Errorf("Name() = %q, want %q", entity.Name(), test.wantName)
			}

			// Verify socket paths use fleet.RunDir layout.
			ns, _ := ref.NewNamespace(testServer, "my_bureau")
			fleet, _ := ref.NewFleet(ns, "prod")
			fleetRunDir := fleet.RunDir("/run/bureau")

			if got := entity.SocketPath(fleetRunDir); got != test.wantSocket {
				t.Errorf("SocketPath(%q) = %q, want %q", fleetRunDir, got, test.wantSocket)
			}
			if got := entity.AdminSocketPath(fleetRunDir); got != test.wantAdmin {
				t.Errorf("AdminSocketPath(%q) = %q, want %q", fleetRunDir, got, test.wantAdmin)
			}
		})
	}
}

func TestParseEntityLocalpart(t *testing.T) {
	t.Parallel()

	entity, err := ref.ParseEntityLocalpart("my_bureau/fleet/prod/machine/gpu-box", testServer)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if entity.EntityType() != "machine" {
		t.Errorf("EntityType() = %q, want %q", entity.EntityType(), "machine")
	}
	if entity.Localpart() != "my_bureau/fleet/prod/machine/gpu-box" {
		t.Errorf("Localpart() = %q, want %q", entity.Localpart(), "my_bureau/fleet/prod/machine/gpu-box")
	}
	if entity.UserID() != "@my_bureau/fleet/prod/machine/gpu-box:"+testServer {
		t.Errorf("UserID() = %q, want %q", entity.UserID(), "@my_bureau/fleet/prod/machine/gpu-box:"+testServer)
	}

	// Verify Fleet() is populated correctly.
	if entity.Fleet().FleetName() != "prod" {
		t.Errorf("Fleet().Name() = %q, want %q", entity.Fleet().FleetName(), "prod")
	}
}

func TestJSONRoundTripMachine(t *testing.T) {
	ns, _ := ref.NewNamespace(testServer, "my_bureau")
	fleet, _ := ref.NewFleet(ns, "prod")
	machine, _ := ref.NewMachine(fleet, "gpu-box")

	data, err := json.Marshal(machine)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	wantJSON := `"@my_bureau/fleet/prod/machine/gpu-box:` + testServer + `"`
	if string(data) != wantJSON {
		t.Fatalf("Marshal = %s, want %s", data, wantJSON)
	}

	var parsed ref.Machine
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if parsed.UserID() != machine.UserID() {
		t.Errorf("round-trip UserID() = %q, want %q", parsed.UserID(), machine.UserID())
	}
	if parsed.Name() != machine.Name() {
		t.Errorf("round-trip Name() = %q, want %q", parsed.Name(), machine.Name())
	}
	if parsed.Fleet().FleetName() != machine.Fleet().FleetName() {
		t.Errorf("round-trip FleetName() = %q, want %q", parsed.Fleet().FleetName(), machine.Fleet().FleetName())
	}
}

func TestJSONRoundTripService(t *testing.T) {
	ns, _ := ref.NewNamespace(testServer, "acme")
	fleet, _ := ref.NewFleet(ns, "staging")
	service, _ := ref.NewService(fleet, "stt/whisper")

	data, err := json.Marshal(service)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var parsed ref.Service
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if parsed.UserID() != service.UserID() {
		t.Errorf("round-trip UserID() = %q, want %q", parsed.UserID(), service.UserID())
	}
	if parsed.Name() != service.Name() {
		t.Errorf("round-trip Name() = %q, want %q", parsed.Name(), service.Name())
	}
}

func TestJSONRoundTripFleet(t *testing.T) {
	ns, _ := ref.NewNamespace(testServer, "my_bureau")
	fleet, _ := ref.NewFleet(ns, "prod")

	data, err := json.Marshal(fleet)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	wantJSON := `"#my_bureau/fleet/prod:` + testServer + `"`
	if string(data) != wantJSON {
		t.Fatalf("Marshal = %s, want %s", data, wantJSON)
	}

	var parsed ref.Fleet
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if parsed.Localpart() != fleet.Localpart() {
		t.Errorf("round-trip Localpart() = %q, want %q", parsed.Localpart(), fleet.Localpart())
	}
	if parsed.Server() != fleet.Server() {
		t.Errorf("round-trip Server() = %q, want %q", parsed.Server(), fleet.Server())
	}
}

func TestJSONRoundTripNamespace(t *testing.T) {
	ns, _ := ref.NewNamespace(testServer, "my_bureau")

	data, err := json.Marshal(ns)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	wantJSON := `"#my_bureau:` + testServer + `"`
	if string(data) != wantJSON {
		t.Fatalf("Marshal = %s, want %s", data, wantJSON)
	}

	var parsed ref.Namespace
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if parsed.Name() != ns.Name() {
		t.Errorf("round-trip Name() = %q, want %q", parsed.Name(), ns.Name())
	}
	if parsed.Server() != ns.Server() {
		t.Errorf("round-trip Server() = %q, want %q", parsed.Server(), ns.Server())
	}
}

func TestJSONInStructField(t *testing.T) {
	// Verify refs work correctly as struct fields in JSON.
	type config struct {
		Machine ref.Machine `json:"machine"`
		Service ref.Service `json:"service"`
	}

	ns, _ := ref.NewNamespace(testServer, "my_bureau")
	fleet, _ := ref.NewFleet(ns, "prod")
	machine, _ := ref.NewMachine(fleet, "gpu-box")
	service, _ := ref.NewService(fleet, "ticket")

	original := config{Machine: machine, Service: service}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var parsed config
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if parsed.Machine.UserID() != original.Machine.UserID() {
		t.Errorf("Machine.UserID() = %q, want %q", parsed.Machine.UserID(), original.Machine.UserID())
	}
	if parsed.Service.UserID() != original.Service.UserID() {
		t.Errorf("Service.UserID() = %q, want %q", parsed.Service.UserID(), original.Service.UserID())
	}
}

func TestZeroValues(t *testing.T) {
	var ns ref.Namespace
	var fleet ref.Fleet
	var machine ref.Machine
	var service ref.Service
	var agent ref.Agent

	if !ns.IsZero() {
		t.Error("Namespace should be zero")
	}
	if !fleet.IsZero() {
		t.Error("Fleet should be zero")
	}
	if !machine.IsZero() {
		t.Error("Machine should be zero")
	}
	if !service.IsZero() {
		t.Error("Service should be zero")
	}
	if !agent.IsZero() {
		t.Error("Agent should be zero")
	}

	// Namespace and Fleet zero values should fail to marshal — they have
	// no deregistration protocol that needs empty-string serialization.
	if _, err := ns.MarshalText(); err == nil {
		t.Error("marshaling zero Namespace should fail")
	}
	if _, err := fleet.MarshalText(); err == nil {
		t.Error("marshaling zero Fleet should fail")
	}

	// Entity types (Machine, Service, Agent, Entity) marshal zero values
	// as empty string. This supports the service deregistration protocol
	// where schema.Service{} is published to clear a directory entry.
	machineBytes, err := machine.MarshalText()
	if err != nil {
		t.Errorf("marshaling zero Machine should succeed: %v", err)
	}
	if len(machineBytes) != 0 {
		t.Errorf("zero Machine marshaled to %q, want empty", machineBytes)
	}
	serviceBytes, err := service.MarshalText()
	if err != nil {
		t.Errorf("marshaling zero Service should succeed: %v", err)
	}
	if len(serviceBytes) != 0 {
		t.Errorf("zero Service marshaled to %q, want empty", serviceBytes)
	}
}

func TestLocalpartLengthLimit(t *testing.T) {
	ns, _ := ref.NewNamespace(testServer, "my_bureau")
	fleet, _ := ref.NewFleet(ns, "prod")

	// "my_bureau/fleet/prod/machine/" is 30 characters.
	// Max localpart is 84 characters.
	// So max machine name is 84 - 30 = 54 characters.
	prefix := "my_bureau/fleet/prod/machine/"

	// Build a name that puts the localpart at exactly 84 characters.
	exactName := strings.Repeat("a", 84-len(prefix))
	_, err := ref.NewMachine(fleet, exactName)
	if err != nil {
		t.Fatalf("expected 84-char localpart to succeed: %v", err)
	}

	// One char longer should fail.
	tooLong := exactName + "a"
	_, err = ref.NewMachine(fleet, tooLong)
	if err == nil {
		t.Fatalf("expected error for 85-char localpart")
	}
}

func TestEntityTypeValidation(t *testing.T) {
	// Parsing a service user ID as a machine should fail.
	serviceUID := "@my_bureau/fleet/prod/service/ticket:" + testServer
	_, err := ref.ParseMachineUserID(serviceUID)
	if err == nil {
		t.Fatal("expected error parsing service UID as machine")
	}
	if !strings.Contains(err.Error(), "expected \"machine\"") {
		t.Errorf("error should mention expected type, got: %v", err)
	}

	// Parsing a machine user ID as a service should fail.
	machineUID := "@my_bureau/fleet/prod/machine/gpu-box:" + testServer
	_, err = ref.ParseServiceUserID(machineUID)
	if err == nil {
		t.Fatal("expected error parsing machine UID as service")
	}
}

func TestConstructionRoundTrip(t *testing.T) {
	// Construct via NewMachine, serialize, parse back, verify equality.
	ns, _ := ref.NewNamespace(testServer, "my_bureau")
	fleet, _ := ref.NewFleet(ns, "prod")
	original, _ := ref.NewMachine(fleet, "gpu-box")

	// Via user ID.
	parsed, err := ref.ParseMachineUserID(original.UserID())
	if err != nil {
		t.Fatalf("ParseMachineUserID: %v", err)
	}
	if parsed.UserID() != original.UserID() {
		t.Errorf("UserID mismatch: %q vs %q", parsed.UserID(), original.UserID())
	}
	if parsed.RoomAlias() != original.RoomAlias() {
		t.Errorf("RoomAlias mismatch: %q vs %q", parsed.RoomAlias(), original.RoomAlias())
	}
	if parsed.Name() != original.Name() {
		t.Errorf("Name mismatch: %q vs %q", parsed.Name(), original.Name())
	}
	if parsed.Fleet().FleetName() != original.Fleet().FleetName() {
		t.Errorf("FleetName mismatch: %q vs %q", parsed.Fleet().FleetName(), original.Fleet().FleetName())
	}

	// Via localpart + server.
	parsed2, err := ref.ParseMachine(original.Localpart(), original.Server())
	if err != nil {
		t.Fatalf("ParseMachine: %v", err)
	}
	if parsed2.UserID() != original.UserID() {
		t.Errorf("ParseMachine UserID mismatch: %q vs %q", parsed2.UserID(), original.UserID())
	}
}

func TestServerWithPort(t *testing.T) {
	// Matrix server names can include ports.
	ns, err := ref.NewNamespace("example.org:8448", "acme")
	if err != nil {
		t.Fatalf("NewNamespace with port: %v", err)
	}
	fleet, _ := ref.NewFleet(ns, "prod")
	machine, _ := ref.NewMachine(fleet, "box")

	// The colon in the server name should not confuse parsing.
	wantUID := "@acme/fleet/prod/machine/box:example.org:8448"
	if machine.UserID() != wantUID {
		t.Errorf("UserID() = %q, want %q", machine.UserID(), wantUID)
	}

	// Parse it back — uses first colon after @ as the separator.
	parsed, err := ref.ParseMachineUserID(wantUID)
	if err != nil {
		t.Fatalf("ParseMachineUserID with port: %v", err)
	}
	if parsed.Server() != "example.org:8448" {
		t.Errorf("Server() = %q, want %q", parsed.Server(), "example.org:8448")
	}
	if parsed.Name() != "box" {
		t.Errorf("Name() = %q, want %q", parsed.Name(), "box")
	}
}

func TestServerFromUserID(t *testing.T) {
	tests := []struct {
		name       string
		userID     string
		wantServer string
		wantErr    bool
	}{
		{name: "simple", userID: "@bureau-admin:bureau.local", wantServer: "bureau.local"},
		{name: "with-port", userID: "@admin:example.org:8448", wantServer: "example.org:8448"},
		{name: "entity", userID: "@my_bureau/fleet/prod/machine/box:bureau.local", wantServer: "bureau.local"},
		{name: "missing-sigil", userID: "bureau-admin:bureau.local", wantErr: true},
		{name: "missing-server", userID: "@bureau-admin", wantErr: true},
		{name: "empty-server", userID: "@bureau-admin:", wantErr: true},
		{name: "empty", userID: "", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, err := ref.ServerFromUserID(test.userID)
			if test.wantErr {
				if err == nil {
					t.Fatalf("expected error, got server %q", server)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if server != test.wantServer {
				t.Errorf("server = %q, want %q", server, test.wantServer)
			}
		})
	}
}

func TestExtractEntityName(t *testing.T) {
	tests := []struct {
		name           string
		localpart      string
		wantEntityType string
		wantEntityName string
		wantErr        bool
	}{
		{
			name:           "legacy machine simple",
			localpart:      "machine/workstation",
			wantEntityType: "machine",
			wantEntityName: "workstation",
		},
		{
			name:           "legacy machine multi-segment",
			localpart:      "machine/ec2/us-east-1/gpu-01",
			wantEntityType: "machine",
			wantEntityName: "ec2/us-east-1/gpu-01",
		},
		{
			name:           "legacy service simple",
			localpart:      "service/fleet/prod",
			wantEntityType: "service",
			wantEntityName: "fleet/prod",
		},
		{
			name:           "fleet-scoped machine",
			localpart:      "bureau/fleet/prod/machine/workstation",
			wantEntityType: "machine",
			wantEntityName: "workstation",
		},
		{
			name:           "fleet-scoped machine multi-segment",
			localpart:      "bureau/fleet/prod/machine/ec2/us-east-1/gpu-01",
			wantEntityType: "machine",
			wantEntityName: "ec2/us-east-1/gpu-01",
		},
		{
			name:           "fleet-scoped service",
			localpart:      "bureau/fleet/prod/service/stt/whisper",
			wantEntityType: "service",
			wantEntityName: "stt/whisper",
		},
		{
			name:           "fleet-scoped agent",
			localpart:      "acme/fleet/staging/agent/code-reviewer",
			wantEntityType: "agent",
			wantEntityName: "code-reviewer",
		},
		{
			name:      "single segment",
			localpart: "machine",
			wantErr:   true,
		},
		{
			name:      "empty",
			localpart: "",
			wantErr:   true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entityType, entityName, err := ref.ExtractEntityName(test.localpart)
			if test.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q/%q", entityType, entityName)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if entityType != test.wantEntityType {
				t.Errorf("entityType = %q, want %q", entityType, test.wantEntityType)
			}
			if entityName != test.wantEntityName {
				t.Errorf("entityName = %q, want %q", entityName, test.wantEntityName)
			}
		})
	}
}

func TestNewEntityFromAccountLocalpart(t *testing.T) {
	ns, err := ref.NewNamespace(testServer, "bureau")
	if err != nil {
		t.Fatalf("creating namespace: %v", err)
	}
	fleet, err := ref.NewFleet(ns, "prod")
	if err != nil {
		t.Fatalf("creating fleet: %v", err)
	}

	tests := []struct {
		name             string
		accountLocalpart string
		wantLocalpart    string
		wantEntityType   string
		wantEntityName   string
		wantSocketSuffix string
		wantErr          bool
	}{
		{
			name:             "simple agent",
			accountLocalpart: "agent/frontend",
			wantLocalpart:    "bureau/fleet/prod/agent/frontend",
			wantEntityType:   "agent",
			wantEntityName:   "frontend",
			wantSocketSuffix: "agent/frontend.sock",
		},
		{
			name:             "simple service",
			accountLocalpart: "service/ticket",
			wantLocalpart:    "bureau/fleet/prod/service/ticket",
			wantEntityType:   "service",
			wantEntityName:   "ticket",
			wantSocketSuffix: "service/ticket.sock",
		},
		{
			name:             "hierarchical service",
			accountLocalpart: "service/stt/whisper",
			wantLocalpart:    "bureau/fleet/prod/service/stt/whisper",
			wantEntityType:   "service",
			wantEntityName:   "stt/whisper",
			wantSocketSuffix: "service/stt/whisper.sock",
		},
		{
			name:             "machine",
			accountLocalpart: "machine/workstation",
			wantLocalpart:    "bureau/fleet/prod/machine/workstation",
			wantEntityType:   "machine",
			wantEntityName:   "workstation",
			wantSocketSuffix: "machine/workstation.sock",
		},
		{
			name:             "pipeline executor",
			accountLocalpart: "pipeline/deploy/1709234567123",
			wantLocalpart:    "bureau/fleet/prod/pipeline/deploy/1709234567123",
			wantEntityType:   "pipeline",
			wantEntityName:   "deploy/1709234567123",
			wantSocketSuffix: "pipeline/deploy/1709234567123.sock",
		},
		{
			name:             "single segment only",
			accountLocalpart: "frontend",
			wantErr:          true,
		},
		{
			name:             "empty",
			accountLocalpart: "",
			wantErr:          true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entity, err := ref.NewEntityFromAccountLocalpart(fleet, test.accountLocalpart)
			if test.wantErr {
				if err == nil {
					t.Fatalf("expected error, got entity with localpart %q", entity.Localpart())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got := entity.Localpart(); got != test.wantLocalpart {
				t.Errorf("Localpart() = %q, want %q", got, test.wantLocalpart)
			}
			if got := entity.EntityType(); got != test.wantEntityType {
				t.Errorf("EntityType() = %q, want %q", got, test.wantEntityType)
			}
			if got := entity.Name(); got != test.wantEntityName {
				t.Errorf("Name() = %q, want %q", got, test.wantEntityName)
			}
			if got := entity.AccountLocalpart(); got != test.accountLocalpart {
				t.Errorf("AccountLocalpart() = %q, want %q", got, test.accountLocalpart)
			}

			// Verify socket path ends with the expected suffix.
			socketPath := entity.SocketPath("/run/bureau/fleet/prod")
			if !strings.HasSuffix(socketPath, test.wantSocketSuffix) {
				t.Errorf("SocketPath() = %q, want suffix %q", socketPath, test.wantSocketSuffix)
			}

			// Verify the entity's fleet matches the input fleet.
			if got := entity.Fleet().Localpart(); got != fleet.Localpart() {
				t.Errorf("Fleet().Localpart() = %q, want %q", got, fleet.Localpart())
			}
		})
	}
}

func TestEntityUnmarshalText(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		wantLocalpart string
		wantType      string
		wantName      string
		wantErr       bool
	}{
		{
			name:          "agent",
			input:         "@bureau/fleet/prod/agent/frontend:bureau.local",
			wantLocalpart: "bureau/fleet/prod/agent/frontend",
			wantType:      "agent",
			wantName:      "frontend",
		},
		{
			name:          "service",
			input:         "@bureau/fleet/prod/service/ticket:bureau.local",
			wantLocalpart: "bureau/fleet/prod/service/ticket",
			wantType:      "service",
			wantName:      "ticket",
		},
		{
			name:          "hierarchical service",
			input:         "@bureau/fleet/prod/service/stt/whisper:bureau.local",
			wantLocalpart: "bureau/fleet/prod/service/stt/whisper",
			wantType:      "service",
			wantName:      "stt/whisper",
		},
		{
			name:    "bare localpart (not fleet-scoped)",
			input:   "@agent/frontend:bureau.local",
			wantErr: true,
		},
		{
			name:  "empty string produces zero value",
			input: "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var entity ref.Entity
			err := entity.UnmarshalText([]byte(test.input))
			if test.wantErr {
				if err == nil {
					t.Fatalf("expected error, got entity with localpart %q", entity.Localpart())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := entity.Localpart(); got != test.wantLocalpart {
				t.Errorf("Localpart() = %q, want %q", got, test.wantLocalpart)
			}
			if got := entity.EntityType(); got != test.wantType {
				t.Errorf("EntityType() = %q, want %q", got, test.wantType)
			}
			if got := entity.Name(); got != test.wantName {
				t.Errorf("Name() = %q, want %q", got, test.wantName)
			}
		})
	}
}

func TestEntityJSONRoundtrip(t *testing.T) {
	// Entity should serialize as a JSON string (the user ID) and
	// deserialize back to an equivalent Entity.
	type wrapper struct {
		Principal ref.Entity `json:"principal"`
	}

	original, err := ref.ParseEntityUserID("@bureau/fleet/prod/agent/frontend:bureau.local")
	if err != nil {
		t.Fatalf("creating entity: %v", err)
	}

	originalWrapper := wrapper{Principal: original}

	data, err := json.Marshal(originalWrapper)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Verify the JSON shape: the Entity should be a string, not an object.
	wantJSON := `{"principal":"@bureau/fleet/prod/agent/frontend:bureau.local"}`
	if string(data) != wantJSON {
		t.Errorf("JSON = %s, want %s", data, wantJSON)
	}

	var roundtripped wrapper
	if err := json.Unmarshal(data, &roundtripped); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if roundtripped.Principal.UserID() != original.UserID() {
		t.Errorf("roundtripped UserID = %q, want %q", roundtripped.Principal.UserID(), original.UserID())
	}
	if roundtripped.Principal.Localpart() != original.Localpart() {
		t.Errorf("roundtripped Localpart = %q, want %q", roundtripped.Principal.Localpart(), original.Localpart())
	}
	if roundtripped.Principal.EntityType() != "agent" {
		t.Errorf("roundtripped EntityType = %q, want %q", roundtripped.Principal.EntityType(), "agent")
	}
}

func TestMachineEntity(t *testing.T) {
	ns, err := ref.NewNamespace("bureau.local", "bureau")
	if err != nil {
		t.Fatalf("namespace: %v", err)
	}
	fleet, err := ref.NewFleet(ns, "prod")
	if err != nil {
		t.Fatalf("fleet: %v", err)
	}
	machine, err := ref.NewMachine(fleet, "gpu-box")
	if err != nil {
		t.Fatalf("machine: %v", err)
	}

	entity := machine.Entity()
	if entity.UserID() != machine.UserID() {
		t.Errorf("Entity().UserID() = %q, want %q", entity.UserID(), machine.UserID())
	}
	if entity.Localpart() != machine.Localpart() {
		t.Errorf("Entity().Localpart() = %q, want %q", entity.Localpart(), machine.Localpart())
	}
	if entity.SocketPath("/run/bureau") != machine.SocketPath("/run/bureau") {
		t.Errorf("Entity().SocketPath() = %q, want %q", entity.SocketPath("/run/bureau"), machine.SocketPath("/run/bureau"))
	}
	if entity.EntityType() != "machine" {
		t.Errorf("Entity().EntityType() = %q, want %q", entity.EntityType(), "machine")
	}
}

func TestServiceEntity(t *testing.T) {
	ns, err := ref.NewNamespace("bureau.local", "bureau")
	if err != nil {
		t.Fatalf("namespace: %v", err)
	}
	fleet, err := ref.NewFleet(ns, "prod")
	if err != nil {
		t.Fatalf("fleet: %v", err)
	}
	service, err := ref.NewService(fleet, "stt/whisper")
	if err != nil {
		t.Fatalf("service: %v", err)
	}

	entity := service.Entity()
	if entity.UserID() != service.UserID() {
		t.Errorf("Entity().UserID() = %q, want %q", entity.UserID(), service.UserID())
	}
	if entity.Localpart() != service.Localpart() {
		t.Errorf("Entity().Localpart() = %q, want %q", entity.Localpart(), service.Localpart())
	}
	if entity.SocketPath("/run/bureau") != service.SocketPath("/run/bureau") {
		t.Errorf("Entity().SocketPath() = %q, want %q", entity.SocketPath("/run/bureau"), service.SocketPath("/run/bureau"))
	}
	if entity.EntityType() != "service" {
		t.Errorf("Entity().EntityType() = %q, want %q", entity.EntityType(), "service")
	}
}

func TestRoomIDMarshalText(t *testing.T) {
	t.Parallel()
	roomID, err := ref.ParseRoomID("!abc123:bureau.local")
	if err != nil {
		t.Fatalf("ParseRoomID: %v", err)
	}

	data, err := roomID.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText: %v", err)
	}
	if string(data) != "!abc123:bureau.local" {
		t.Errorf("MarshalText = %q, want %q", data, "!abc123:bureau.local")
	}

	var roundtrip ref.RoomID
	if err := roundtrip.UnmarshalText(data); err != nil {
		t.Fatalf("UnmarshalText: %v", err)
	}
	if roundtrip != roomID {
		t.Errorf("roundtrip = %v, want %v", roundtrip, roomID)
	}
}

func TestRoomIDUnmarshalTextRejectsInvalid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"no_exclamation", "abc123:bureau.local"},
		{"alias_not_id", "#room:bureau.local"},
		{"no_server", "!abc123"},
		{"empty_local", "!:bureau.local"},
		{"empty_server", "!abc123:"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var roomID ref.RoomID
			if err := roomID.UnmarshalText([]byte(test.input)); err == nil {
				t.Errorf("UnmarshalText(%q) = nil, want error", test.input)
			}
		})
	}
}

func TestRoomIDJSONRoundtrip(t *testing.T) {
	t.Parallel()

	type testStruct struct {
		Room ref.RoomID `json:"room_id"`
	}

	original := testStruct{}
	roomID, err := ref.ParseRoomID("!test456:example.org")
	if err != nil {
		t.Fatalf("ParseRoomID: %v", err)
	}
	original.Room = roomID

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	// Verify the JSON uses the string representation.
	want := `{"room_id":"!test456:example.org"}`
	if string(data) != want {
		t.Errorf("json.Marshal = %s, want %s", data, want)
	}

	var decoded testStruct
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if decoded.Room != original.Room {
		t.Errorf("roundtrip = %v, want %v", decoded.Room, original.Room)
	}
}

func TestRoomIDJSONMapKey(t *testing.T) {
	t.Parallel()

	roomID, err := ref.ParseRoomID("!mapkey:bureau.local")
	if err != nil {
		t.Fatalf("ParseRoomID: %v", err)
	}

	original := map[ref.RoomID]string{roomID: "hello"}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	want := `{"!mapkey:bureau.local":"hello"}`
	if string(data) != want {
		t.Errorf("json.Marshal = %s, want %s", data, want)
	}

	var decoded map[ref.RoomID]string
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if decoded[roomID] != "hello" {
		t.Errorf("decoded[%v] = %q, want %q", roomID, decoded[roomID], "hello")
	}
}

func TestRoomIDZeroMarshalText(t *testing.T) {
	t.Parallel()
	var zero ref.RoomID
	data, err := zero.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText on zero: %v", err)
	}
	if len(data) != 0 {
		t.Errorf("MarshalText on zero = %q, want empty", data)
	}
}
