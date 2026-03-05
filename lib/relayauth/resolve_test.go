// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package relayauth

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
)

func testFleet(t *testing.T) ref.Fleet {
	t.Helper()
	server := ref.MustParseServerName(testServer)
	namespace, err := ref.NewNamespace(server, "my_bureau")
	if err != nil {
		t.Fatal(err)
	}
	fleet, err := ref.NewFleet(namespace, "prod")
	if err != nil {
		t.Fatal(err)
	}
	return fleet
}

func TestResolveOpsRoom_Machine(t *testing.T) {
	fleet := testFleet(t)
	resource := schema.ResourceRef{
		Type:   schema.ResourceMachine,
		Target: "gpu-box",
	}

	alias, err := ResolveOpsRoom(resource, fleet)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := "#my_bureau/fleet/prod/machine/gpu-box/ops:" + testServer
	if alias.String() != want {
		t.Errorf("ResolveOpsRoom() = %q, want %q", alias.String(), want)
	}
}

func TestResolveOpsRoom_Machine_InvalidName(t *testing.T) {
	fleet := testFleet(t)
	resource := schema.ResourceRef{
		Type:   schema.ResourceMachine,
		Target: "GPU-BOX", // Uppercase is invalid.
	}

	_, err := ResolveOpsRoom(resource, fleet)
	if err == nil {
		t.Fatal("expected error for invalid machine name")
	}
}

func TestResolveOpsRoom_UnsupportedTypes(t *testing.T) {
	fleet := testFleet(t)

	unsupported := []schema.ResourceType{
		schema.ResourceQuota,
		schema.ResourceHuman,
		schema.ResourceService,
	}

	for _, resourceType := range unsupported {
		resource := schema.ResourceRef{
			Type:   resourceType,
			Target: "something",
		}
		_, err := ResolveOpsRoom(resource, fleet)
		if err == nil {
			t.Errorf("resource type %q should return error (no ops rooms yet)", resourceType)
		}
	}
}

func TestResolveOpsRoom_UnknownType(t *testing.T) {
	fleet := testFleet(t)
	resource := schema.ResourceRef{
		Type:   schema.ResourceType("future_type"),
		Target: "something",
	}

	_, err := ResolveOpsRoom(resource, fleet)
	if err == nil {
		t.Fatal("unknown resource type should return error")
	}
}
