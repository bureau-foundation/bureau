// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"encoding/json"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// --- Matcher tests ---

func TestHasCapability(t *testing.T) {
	match := HasCapability("streaming")

	t.Run("matches when capability is present", func(t *testing.T) {
		s := schema.Service{Capabilities: []string{"streaming", "batch"}}
		if !match(s) {
			t.Error("expected match")
		}
	})

	t.Run("does not match when capability is absent", func(t *testing.T) {
		s := schema.Service{Capabilities: []string{"batch", "diarization"}}
		if match(s) {
			t.Error("expected no match")
		}
	})

	t.Run("does not match empty capabilities", func(t *testing.T) {
		s := schema.Service{}
		if match(s) {
			t.Error("expected no match for empty capabilities")
		}
	})
}

func TestHasAllCapabilities(t *testing.T) {
	match := HasAllCapabilities("streaming", "diarization")

	t.Run("matches when all capabilities are present", func(t *testing.T) {
		s := schema.Service{Capabilities: []string{"streaming", "diarization", "batch"}}
		if !match(s) {
			t.Error("expected match")
		}
	})

	t.Run("does not match when only some are present", func(t *testing.T) {
		s := schema.Service{Capabilities: []string{"streaming", "batch"}}
		if match(s) {
			t.Error("expected no match")
		}
	})

	t.Run("does not match when none are present", func(t *testing.T) {
		s := schema.Service{Capabilities: []string{"batch"}}
		if match(s) {
			t.Error("expected no match")
		}
	})

	t.Run("empty requirement matches all services", func(t *testing.T) {
		matchAll := HasAllCapabilities()
		if !matchAll(schema.Service{}) {
			t.Error("expected empty requirement to match")
		}
	})
}

func TestOnMachine(t *testing.T) {
	machine1 := mustParseMachine(t, "@bureau/fleet/test/machine/box1:test.local")
	machine2 := mustParseMachine(t, "@bureau/fleet/test/machine/box2:test.local")
	match := OnMachine(machine1)

	t.Run("matches same machine", func(t *testing.T) {
		s := schema.Service{Machine: machine1}
		if !match(s) {
			t.Error("expected match for same machine")
		}
	})

	t.Run("does not match different machine", func(t *testing.T) {
		s := schema.Service{Machine: machine2}
		if match(s) {
			t.Error("expected no match for different machine")
		}
	})
}

func TestAnd(t *testing.T) {
	machine := mustParseMachine(t, "@bureau/fleet/test/machine/box1:test.local")

	t.Run("all matchers pass", func(t *testing.T) {
		match := And(OnMachine(machine), HasCapability("streaming"))
		s := schema.Service{Machine: machine, Capabilities: []string{"streaming"}}
		if !match(s) {
			t.Error("expected match when all matchers pass")
		}
	})

	t.Run("one matcher fails", func(t *testing.T) {
		match := And(OnMachine(machine), HasCapability("streaming"))
		s := schema.Service{Machine: machine, Capabilities: []string{"batch"}}
		if match(s) {
			t.Error("expected no match when one matcher fails")
		}
	})

	t.Run("empty matcher list matches everything", func(t *testing.T) {
		match := And()
		if !match(schema.Service{}) {
			t.Error("expected empty And to match all services")
		}
	})
}

func TestCustomMatcher(t *testing.T) {
	// Callers can use any func(schema.Service) bool as a Matcher.
	match := Matcher(func(s schema.Service) bool {
		return s.Protocol == "grpc"
	})

	if !match(schema.Service{Protocol: "grpc"}) {
		t.Error("expected custom matcher to accept grpc service")
	}
	if match(schema.Service{Protocol: "cbor"}) {
		t.Error("expected custom matcher to reject cbor service")
	}
}

// --- FindAll / FindFirst tests ---

func TestFindAll(t *testing.T) {
	services := []schema.Service{
		{Capabilities: []string{"streaming"}, Protocol: "grpc"},
		{Capabilities: []string{"batch"}, Protocol: "cbor"},
		{Capabilities: []string{"streaming", "batch"}, Protocol: "grpc"},
	}
	match := HasCapability("streaming")

	t.Run("returns all matches", func(t *testing.T) {
		result := FindAll(services, match)
		if length := len(result); length != 2 {
			t.Fatalf("expected 2 matches, got %d", length)
		}
		if result[0].Protocol != "grpc" || result[1].Protocol != "grpc" {
			t.Error("unexpected match contents")
		}
	})

	t.Run("returns nil when nothing matches", func(t *testing.T) {
		result := FindAll(services, HasCapability("nonexistent"))
		if result != nil {
			t.Errorf("expected nil, got %v", result)
		}
	})

	t.Run("handles empty input", func(t *testing.T) {
		result := FindAll(nil, match)
		if result != nil {
			t.Errorf("expected nil for empty input, got %v", result)
		}
	})
}

func TestFindFirst(t *testing.T) {
	services := []schema.Service{
		{Capabilities: []string{"batch"}, Protocol: "cbor"},
		{Capabilities: []string{"streaming"}, Protocol: "grpc"},
		{Capabilities: []string{"streaming"}, Protocol: "http"},
	}
	match := HasCapability("streaming")

	t.Run("returns first match preserving order", func(t *testing.T) {
		result, found := FindFirst(services, match)
		if !found {
			t.Fatal("expected to find a match")
		}
		if result.Protocol != "grpc" {
			t.Errorf("expected first streaming service (grpc), got %s", result.Protocol)
		}
	})

	t.Run("returns false when nothing matches", func(t *testing.T) {
		_, found := FindFirst(services, HasCapability("nonexistent"))
		if found {
			t.Error("expected no match")
		}
	})

	t.Run("returns false for empty input", func(t *testing.T) {
		_, found := FindFirst(nil, match)
		if found {
			t.Error("expected no match for empty input")
		}
	})
}

// --- ParseServiceDirectory tests ---

func TestParseServiceDirectory(t *testing.T) {
	machine := mustParseMachine(t, "@bureau/fleet/test/machine/box1:test.local")
	entity := mustParseEntity(t, "@bureau/fleet/test/service/ticket/main:test.local")

	validService := schema.Service{
		Principal:    entity,
		Machine:      machine,
		Capabilities: []string{"dependency-graph"},
		Protocol:     "cbor",
	}

	t.Run("parses valid service events", func(t *testing.T) {
		stateKey := "service/ticket/main"
		events := []messaging.Event{
			{
				Type:     schema.EventTypeService,
				StateKey: &stateKey,
				Content:  serviceToContent(t, validService),
			},
		}
		services := ParseServiceDirectory(events)
		if length := len(services); length != 1 {
			t.Fatalf("expected 1 service, got %d", length)
		}
		if services[0].Protocol != "cbor" {
			t.Errorf("expected protocol cbor, got %s", services[0].Protocol)
		}
		if len(services[0].Capabilities) != 1 || services[0].Capabilities[0] != "dependency-graph" {
			t.Errorf("unexpected capabilities: %v", services[0].Capabilities)
		}
	})

	t.Run("skips non-service events", func(t *testing.T) {
		stateKey := "something"
		events := []messaging.Event{
			{
				Type:     "m.room.member",
				StateKey: &stateKey,
				Content:  serviceToContent(t, validService),
			},
		}
		services := ParseServiceDirectory(events)
		if services != nil {
			t.Errorf("expected nil, got %v", services)
		}
	})

	t.Run("skips events without state key", func(t *testing.T) {
		events := []messaging.Event{
			{
				Type:    schema.EventTypeService,
				Content: serviceToContent(t, validService),
			},
		}
		services := ParseServiceDirectory(events)
		if services != nil {
			t.Errorf("expected nil, got %v", services)
		}
	})

	t.Run("skips deregistered services with zero principal", func(t *testing.T) {
		stateKey := "service/old"
		// A deregistered service has an empty content map — no principal.
		events := []messaging.Event{
			{
				Type:     schema.EventTypeService,
				StateKey: &stateKey,
				Content:  map[string]any{},
			},
		}
		services := ParseServiceDirectory(events)
		if services != nil {
			t.Errorf("expected nil for deregistered service, got %v", services)
		}
	})

	t.Run("skips events with unparseable content", func(t *testing.T) {
		stateKey := "service/bad"
		events := []messaging.Event{
			{
				Type:     schema.EventTypeService,
				StateKey: &stateKey,
				Content: map[string]any{
					"principal": 12345, // wrong type — string expected
				},
			},
		}
		services := ParseServiceDirectory(events)
		if services != nil {
			t.Errorf("expected nil for unparseable content, got %v", services)
		}
	})

	t.Run("parses multiple services from mixed events", func(t *testing.T) {
		serviceKey := "service/ticket/main"
		memberKey := "@user:test.local"

		entity2 := mustParseEntity(t, "@bureau/fleet/test/service/artifact/main:test.local")
		artifactKey := "service/artifact/main"
		artifactService := schema.Service{
			Principal:    entity2,
			Machine:      machine,
			Capabilities: []string{"content-addressed-store"},
			Protocol:     "cbor",
		}

		events := []messaging.Event{
			{Type: "m.room.member", StateKey: &memberKey, Content: map[string]any{"membership": "join"}},
			{Type: schema.EventTypeService, StateKey: &serviceKey, Content: serviceToContent(t, validService)},
			{Type: schema.EventTypeService, StateKey: &artifactKey, Content: serviceToContent(t, artifactService)},
		}

		services := ParseServiceDirectory(events)
		if length := len(services); length != 2 {
			t.Fatalf("expected 2 services, got %d", length)
		}
	})
}

// --- Test helpers ---

func mustParseMachine(t *testing.T, userID string) ref.Machine {
	t.Helper()
	machine, err := ref.ParseMachineUserID(userID)
	if err != nil {
		t.Fatalf("ParseMachineUserID(%q): %v", userID, err)
	}
	return machine
}

func mustParseEntity(t *testing.T, userID string) ref.Entity {
	t.Helper()
	entity, err := ref.ParseEntityUserID(userID)
	if err != nil {
		t.Fatalf("ParseEntityUserID(%q): %v", userID, err)
	}
	return entity
}

// serviceToContent converts a typed Service to the map[string]any
// representation used in messaging.Event.Content, simulating how
// state events look after JSON round-tripping through the homeserver.
func serviceToContent(t *testing.T, s schema.Service) map[string]any {
	t.Helper()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal service: %v", err)
	}
	var content map[string]any
	if err := json.Unmarshal(data, &content); err != nil {
		t.Fatalf("unmarshal to content map: %v", err)
	}
	return content
}
