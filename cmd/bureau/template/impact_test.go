// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package template

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// impactTestState extends the mock Matrix server pattern from
// lib/template/resolve_test.go with JoinedRooms and GetRoomState support
// needed by impact analysis.
type impactTestState struct {
	mu sync.Mutex

	// joinedRooms is the list of room IDs returned by JoinedRooms.
	joinedRooms []string

	// roomAliases maps full aliases ("#bureau/config/machine:test.local") to room IDs.
	roomAliases map[string]string

	// roomEvents maps roomID to the list of state events returned by GetRoomState.
	roomEvents map[string][]messaging.Event

	// stateEvents maps "roomID\x00eventType\x00stateKey" to raw JSON content
	// for individual state event fetches (used by template resolution).
	stateEvents map[string]json.RawMessage
}

func newImpactTestState() *impactTestState {
	return &impactTestState{
		roomAliases: make(map[string]string),
		roomEvents:  make(map[string][]messaging.Event),
		stateEvents: make(map[string]json.RawMessage),
	}
}

// addConfigRoom sets up a config room with a canonical alias and MachineConfig.
func (s *impactTestState) addConfigRoom(roomID, machineName, serverName string, config schema.MachineConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()

	alias := fmt.Sprintf("#bureau/config/%s:%s", machineName, serverName)
	s.roomAliases[alias] = roomID
	s.joinedRooms = append(s.joinedRooms, roomID)

	// Build state events for GetRoomState.
	configContent, err := json.Marshal(config)
	if err != nil {
		panic(fmt.Sprintf("marshaling MachineConfig: %v", err))
	}
	var configMap map[string]any
	if err := json.Unmarshal(configContent, &configMap); err != nil {
		panic(fmt.Sprintf("unmarshaling MachineConfig to map: %v", err))
	}

	stateKey := machineName
	s.roomEvents[roomID] = []messaging.Event{
		{
			Type:    "m.room.canonical_alias",
			Content: map[string]any{"alias": alias},
		},
		{
			Type:     schema.EventTypeMachineConfig,
			StateKey: &stateKey,
			Content:  configMap,
		},
	}
}

// addNonConfigRoom adds a room the operator has joined that is NOT a config room.
func (s *impactTestState) addNonConfigRoom(roomID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.joinedRooms = append(s.joinedRooms, roomID)
	s.roomEvents[roomID] = []messaging.Event{
		{
			Type:    "m.room.canonical_alias",
			Content: map[string]any{"alias": "#bureau/template:test.local"},
		},
	}
}

// addTemplate registers a template for both room alias resolution and state
// event fetching.
func (s *impactTestState) addTemplate(roomAlias, roomID, templateName string, content schema.TemplateContent) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.roomAliases[roomAlias] = roomID
	data, err := json.Marshal(content)
	if err != nil {
		panic(fmt.Sprintf("marshaling template: %v", err))
	}
	key := roomID + "\x00" + schema.EventTypeTemplate + "\x00" + templateName
	s.stateEvents[key] = data
}

func (s *impactTestState) handler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		path := request.URL.RawPath
		if path == "" {
			path = request.URL.Path
		}

		s.mu.Lock()
		defer s.mu.Unlock()

		switch {
		case path == "/_matrix/client/v3/joined_rooms":
			s.handleJoinedRooms(writer)
		case strings.HasPrefix(path, "/_matrix/client/v3/directory/room/"):
			s.handleResolveAlias(writer, path)
		case strings.HasSuffix(path, "/state") && strings.HasPrefix(path, "/_matrix/client/v3/rooms/"):
			// GetRoomState: /_matrix/client/v3/rooms/{roomID}/state
			// Must NOT match GetStateEvent which has /state/{type}/{key}.
			s.handleGetRoomState(writer, path)
		case strings.Contains(path, "/state/"):
			// GetStateEvent: /_matrix/client/v3/rooms/{roomID}/state/{type}/{key}
			s.handleGetStateEvent(writer, path)
		default:
			http.Error(writer, fmt.Sprintf(`{"errcode":"M_UNRECOGNIZED","error":"unknown path: %s"}`, path), http.StatusNotFound)
		}
	})
}

func (s *impactTestState) handleJoinedRooms(writer http.ResponseWriter) {
	response := struct {
		JoinedRooms []string `json:"joined_rooms"`
	}{JoinedRooms: s.joinedRooms}
	writer.Header().Set("Content-Type", "application/json")
	json.NewEncoder(writer).Encode(response)
}

func (s *impactTestState) handleResolveAlias(writer http.ResponseWriter, path string) {
	encoded := strings.TrimPrefix(path, "/_matrix/client/v3/directory/room/")
	alias, err := url.PathUnescape(encoded)
	if err != nil {
		http.Error(writer, `{"errcode":"M_INVALID_PARAM","error":"bad alias encoding"}`, http.StatusBadRequest)
		return
	}
	roomID, exists := s.roomAliases[alias]
	if !exists {
		writer.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(writer, `{"errcode":"M_NOT_FOUND","error":"room alias %q not found"}`, alias)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(writer, `{"room_id":"%s"}`, roomID)
}

func (s *impactTestState) handleGetRoomState(writer http.ResponseWriter, path string) {
	trimmed := strings.TrimPrefix(path, "/_matrix/client/v3/rooms/")
	roomIDEncoded := strings.TrimSuffix(trimmed, "/state")
	roomID, err := url.PathUnescape(roomIDEncoded)
	if err != nil {
		http.Error(writer, `{"errcode":"M_INVALID_PARAM","error":"bad roomId encoding"}`, http.StatusBadRequest)
		return
	}
	events, exists := s.roomEvents[roomID]
	if !exists {
		writer.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(writer, `{"errcode":"M_NOT_FOUND","error":"room %q not found"}`, roomID)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	json.NewEncoder(writer).Encode(events)
}

func (s *impactTestState) handleGetStateEvent(writer http.ResponseWriter, path string) {
	trimmed := strings.TrimPrefix(path, "/_matrix/client/v3/rooms/")
	parts := strings.SplitN(trimmed, "/state/", 2)
	if len(parts) != 2 {
		http.Error(writer, `{"errcode":"M_UNRECOGNIZED","error":"bad state path"}`, http.StatusBadRequest)
		return
	}
	roomID, err := url.PathUnescape(parts[0])
	if err != nil {
		http.Error(writer, `{"errcode":"M_INVALID_PARAM","error":"bad roomId encoding"}`, http.StatusBadRequest)
		return
	}
	eventAndKey := parts[1]
	slashIndex := strings.Index(eventAndKey, "/")
	if slashIndex < 0 {
		http.Error(writer, `{"errcode":"M_INVALID_PARAM","error":"missing state key"}`, http.StatusBadRequest)
		return
	}
	eventType, err := url.PathUnescape(eventAndKey[:slashIndex])
	if err != nil {
		http.Error(writer, `{"errcode":"M_INVALID_PARAM","error":"bad eventType encoding"}`, http.StatusBadRequest)
		return
	}
	stateKey, err := url.PathUnescape(eventAndKey[slashIndex+1:])
	if err != nil {
		http.Error(writer, `{"errcode":"M_INVALID_PARAM","error":"bad stateKey encoding"}`, http.StatusBadRequest)
		return
	}

	key := roomID + "\x00" + eventType + "\x00" + stateKey
	content, exists := s.stateEvents[key]
	if !exists {
		writer.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(writer, `{"errcode":"M_NOT_FOUND","error":"state event not found: %s/%s in %s"}`, eventType, stateKey, roomID)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Write(content)
}

func newImpactTestSession(t *testing.T, state *impactTestState) *messaging.Session {
	t.Helper()
	server := httptest.NewServer(state.handler())
	t.Cleanup(server.Close)

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: server.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session, err := client.SessionFromToken("@operator:test.local", "operator-token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

const impactServerName = "test.local"

func TestImpactDirectReference(t *testing.T) {
	t.Parallel()

	state := newImpactTestState()

	// Template that the principal uses directly.
	state.addTemplate(
		"#bureau/template:test.local", "!template:test",
		"agent", schema.TemplateContent{
			Description: "Agent template",
			Command:     []string{"/usr/bin/agent"},
		},
	)

	// One machine with one principal using that template directly.
	state.addConfigRoom("!config-ws:test", "machine/workstation", impactServerName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "test/agent",
				Template:  "bureau/template:agent",
			},
		},
	})

	session := newImpactTestSession(t, state)
	analyzer := &impactAnalyzer{
		session:    session,
		serverName: impactServerName,
		ctx:        t.Context(),
		cache:      make(map[string]*schema.TemplateContent),
	}

	configs, err := analyzer.discoverMachineConfigs()
	if err != nil {
		t.Fatalf("discoverMachineConfigs: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("configs count = %d, want 1", len(configs))
	}
	if configs[0].machine != "machine/workstation" {
		t.Errorf("machine = %q, want machine/workstation", configs[0].machine)
	}

	affected, err := analyzer.findAffected("bureau/template:agent", configs)
	if err != nil {
		t.Fatalf("findAffected: %v", err)
	}
	if len(affected) != 1 {
		t.Fatalf("affected count = %d, want 1", len(affected))
	}
	if affected[0].Principal != "test/agent" {
		t.Errorf("Principal = %q, want test/agent", affected[0].Principal)
	}
	if affected[0].Depth != 0 {
		t.Errorf("Depth = %d, want 0 (direct reference)", affected[0].Depth)
	}
	if affected[0].Machine != "machine/workstation" {
		t.Errorf("Machine = %q, want machine/workstation", affected[0].Machine)
	}
}

func TestImpactTransitiveInheritance(t *testing.T) {
	t.Parallel()

	state := newImpactTestState()

	// Base template.
	state.addTemplate(
		"#bureau/template:test.local", "!template:test",
		"base", schema.TemplateContent{
			Description: "Base template",
			Command:     []string{"/bin/bash"},
			Namespaces:  &schema.TemplateNamespaces{PID: true},
		},
	)

	// Child template inherits from base.
	state.addTemplate(
		"#bureau/template:test.local", "!template:test",
		"agent", schema.TemplateContent{
			Inherits:    "bureau/template:base",
			Description: "Agent template",
			Command:     []string{"/usr/bin/agent"},
		},
	)

	// Grandchild template inherits from child.
	state.addTemplate(
		"#bureau/template:test.local", "!template:test",
		"llm-agent", schema.TemplateContent{
			Inherits:    "bureau/template:agent",
			Description: "LLM agent template",
			EnvironmentVariables: map[string]string{
				"MODEL": "gpt-4",
			},
		},
	)

	// Principal uses the grandchild template. Impact analysis targets the base.
	state.addConfigRoom("!config-ws:test", "machine/workstation", impactServerName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{Localpart: "test/llm", Template: "bureau/template:llm-agent"},
			{Localpart: "test/plain", Template: "bureau/template:agent"},
		},
	})

	session := newImpactTestSession(t, state)
	analyzer := &impactAnalyzer{
		session:    session,
		serverName: impactServerName,
		ctx:        t.Context(),
		cache:      make(map[string]*schema.TemplateContent),
	}

	configs, err := analyzer.discoverMachineConfigs()
	if err != nil {
		t.Fatalf("discoverMachineConfigs: %v", err)
	}

	affected, err := analyzer.findAffected("bureau/template:base", configs)
	if err != nil {
		t.Fatalf("findAffected: %v", err)
	}
	if len(affected) != 2 {
		t.Fatalf("affected count = %d, want 2", len(affected))
	}

	// Find each result by principal name.
	var llmResult, plainResult *impactResult
	for _, result := range affected {
		switch result.Principal {
		case "test/llm":
			llmResult = result
		case "test/plain":
			plainResult = result
		}
	}

	if llmResult == nil {
		t.Fatal("test/llm not found in affected results")
	}
	if llmResult.Depth != 2 {
		t.Errorf("test/llm depth = %d, want 2 (grandchild → child → base)", llmResult.Depth)
	}

	if plainResult == nil {
		t.Fatal("test/plain not found in affected results")
	}
	if plainResult.Depth != 1 {
		t.Errorf("test/plain depth = %d, want 1 (child → base)", plainResult.Depth)
	}
}

func TestImpactNoAffectedPrincipals(t *testing.T) {
	t.Parallel()

	state := newImpactTestState()

	// Template that exists but no principal references.
	state.addTemplate(
		"#bureau/template:test.local", "!template:test",
		"unused", schema.TemplateContent{
			Description: "Nobody uses this",
		},
	)

	// Template that the principal actually uses.
	state.addTemplate(
		"#bureau/template:test.local", "!template:test",
		"agent", schema.TemplateContent{
			Description: "Agent template",
			Command:     []string{"/usr/bin/agent"},
		},
	)

	state.addConfigRoom("!config-ws:test", "machine/workstation", impactServerName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{Localpart: "test/agent", Template: "bureau/template:agent"},
		},
	})

	session := newImpactTestSession(t, state)
	analyzer := &impactAnalyzer{
		session:    session,
		serverName: impactServerName,
		ctx:        t.Context(),
		cache:      make(map[string]*schema.TemplateContent),
	}

	configs, err := analyzer.discoverMachineConfigs()
	if err != nil {
		t.Fatalf("discoverMachineConfigs: %v", err)
	}

	affected, err := analyzer.findAffected("bureau/template:unused", configs)
	if err != nil {
		t.Fatalf("findAffected: %v", err)
	}
	if len(affected) != 0 {
		t.Errorf("affected count = %d, want 0 (no principal references the unused template)", len(affected))
	}
}

func TestImpactMultipleMachines(t *testing.T) {
	t.Parallel()

	state := newImpactTestState()

	state.addTemplate(
		"#bureau/template:test.local", "!template:test",
		"base", schema.TemplateContent{
			Description: "Base template",
			Command:     []string{"/bin/bash"},
		},
	)

	// Two machines, each with principals referencing the same template.
	state.addConfigRoom("!config-ws:test", "machine/workstation", impactServerName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{Localpart: "ws/agent-a", Template: "bureau/template:base"},
			{Localpart: "ws/agent-b", Template: "bureau/template:base"},
		},
	})

	state.addConfigRoom("!config-gpu:test", "machine/gpu-server", impactServerName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{Localpart: "gpu/training", Template: "bureau/template:base"},
		},
	})

	// Also a non-config room to verify it gets filtered out.
	state.addNonConfigRoom("!template:test")

	session := newImpactTestSession(t, state)
	analyzer := &impactAnalyzer{
		session:    session,
		serverName: impactServerName,
		ctx:        t.Context(),
		cache:      make(map[string]*schema.TemplateContent),
	}

	configs, err := analyzer.discoverMachineConfigs()
	if err != nil {
		t.Fatalf("discoverMachineConfigs: %v", err)
	}
	if len(configs) != 2 {
		t.Fatalf("configs count = %d, want 2", len(configs))
	}

	affected, err := analyzer.findAffected("bureau/template:base", configs)
	if err != nil {
		t.Fatalf("findAffected: %v", err)
	}
	if len(affected) != 3 {
		t.Fatalf("affected count = %d, want 3 (2 on workstation + 1 on gpu-server)", len(affected))
	}

	// Verify machine distribution.
	machineCount := make(map[string]int)
	for _, result := range affected {
		machineCount[result.Machine]++
	}
	if machineCount["machine/workstation"] != 2 {
		t.Errorf("workstation count = %d, want 2", machineCount["machine/workstation"])
	}
	if machineCount["machine/gpu-server"] != 1 {
		t.Errorf("gpu-server count = %d, want 1", machineCount["machine/gpu-server"])
	}
}

func TestImpactInstanceOverrideAnnotation(t *testing.T) {
	t.Parallel()

	state := newImpactTestState()

	state.addTemplate(
		"#bureau/template:test.local", "!template:test",
		"agent", schema.TemplateContent{
			Description: "Agent template",
			Command:     []string{"/usr/bin/agent"},
		},
	)

	state.addConfigRoom("!config-ws:test", "machine/workstation", impactServerName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart:       "test/overridden",
				Template:        "bureau/template:agent",
				CommandOverride: []string{"/usr/bin/custom-agent"},
				ExtraEnvironmentVariables: map[string]string{
					"CUSTOM": "true",
				},
				Payload: map[string]any{
					"project": "custom",
				},
			},
			{
				Localpart:           "test/env-override",
				Template:            "bureau/template:agent",
				EnvironmentOverride: "/nix/store/custom-env",
			},
			{
				Localpart: "test/no-overrides",
				Template:  "bureau/template:agent",
			},
		},
	})

	session := newImpactTestSession(t, state)
	analyzer := &impactAnalyzer{
		session:    session,
		serverName: impactServerName,
		ctx:        t.Context(),
		cache:      make(map[string]*schema.TemplateContent),
	}

	configs, err := analyzer.discoverMachineConfigs()
	if err != nil {
		t.Fatalf("discoverMachineConfigs: %v", err)
	}

	affected, err := analyzer.findAffected("bureau/template:agent", configs)
	if err != nil {
		t.Fatalf("findAffected: %v", err)
	}
	if len(affected) != 3 {
		t.Fatalf("affected count = %d, want 3", len(affected))
	}

	// Find each result by principal name.
	resultMap := make(map[string]*impactResult)
	for _, result := range affected {
		resultMap[result.Principal] = result
	}

	overridden := resultMap["test/overridden"]
	if overridden == nil {
		t.Fatal("test/overridden not found")
	}
	if len(overridden.Overrides) != 3 {
		t.Errorf("test/overridden overrides = %v, want 3 entries", overridden.Overrides)
	}
	expectedOverrides := map[string]bool{
		"command_override":            true,
		"extra_environment_variables": true,
		"payload":                     true,
	}
	for _, override := range overridden.Overrides {
		if !expectedOverrides[override] {
			t.Errorf("unexpected override %q", override)
		}
	}

	envOverride := resultMap["test/env-override"]
	if envOverride == nil {
		t.Fatal("test/env-override not found")
	}
	if len(envOverride.Overrides) != 1 || envOverride.Overrides[0] != "environment_override" {
		t.Errorf("test/env-override overrides = %v, want [environment_override]", envOverride.Overrides)
	}

	noOverrides := resultMap["test/no-overrides"]
	if noOverrides == nil {
		t.Fatal("test/no-overrides not found")
	}
	if len(noOverrides.Overrides) != 0 {
		t.Errorf("test/no-overrides overrides = %v, want empty", noOverrides.Overrides)
	}
}

func TestImpactChangeClassificationStructural(t *testing.T) {
	t.Parallel()

	state := newImpactTestState()

	// Current template in Matrix.
	state.addTemplate(
		"#bureau/template:test.local", "!template:test",
		"agent", schema.TemplateContent{
			Description: "Agent template",
			Command:     []string{"/usr/bin/agent"},
			Namespaces:  &schema.TemplateNamespaces{PID: true, Net: true},
		},
	)

	state.addConfigRoom("!config-ws:test", "machine/workstation", impactServerName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{Localpart: "test/agent", Template: "bureau/template:agent"},
		},
	})

	session := newImpactTestSession(t, state)
	analyzer := &impactAnalyzer{
		session:    session,
		serverName: impactServerName,
		ctx:        t.Context(),
		cache:      make(map[string]*schema.TemplateContent),
	}

	configs, err := analyzer.discoverMachineConfigs()
	if err != nil {
		t.Fatalf("discoverMachineConfigs: %v", err)
	}

	affected, err := analyzer.findAffected("bureau/template:agent", configs)
	if err != nil {
		t.Fatalf("findAffected: %v", err)
	}

	// Proposed change: different command (structural change).
	proposed := &schema.TemplateContent{
		Description: "Agent template",
		Command:     []string{"/usr/bin/new-agent", "--verbose"},
		Namespaces:  &schema.TemplateNamespaces{PID: true, Net: true},
	}

	if err := analyzer.classifyChanges(affected, "bureau/template:agent", proposed); err != nil {
		t.Fatalf("classifyChanges: %v", err)
	}

	if len(affected) != 1 {
		t.Fatalf("affected count = %d, want 1", len(affected))
	}
	if affected[0].Change != "structural" {
		t.Errorf("Change = %q, want structural", affected[0].Change)
	}
	if len(affected[0].ChangedFields) != 1 || affected[0].ChangedFields[0] != "command" {
		t.Errorf("ChangedFields = %v, want [command]", affected[0].ChangedFields)
	}
}

func TestImpactChangeClassificationPayloadOnly(t *testing.T) {
	t.Parallel()

	state := newImpactTestState()

	state.addTemplate(
		"#bureau/template:test.local", "!template:test",
		"agent", schema.TemplateContent{
			Description:    "Agent template",
			Command:        []string{"/usr/bin/agent"},
			DefaultPayload: map[string]any{"model": "gpt-3"},
		},
	)

	state.addConfigRoom("!config-ws:test", "machine/workstation", impactServerName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{Localpart: "test/agent", Template: "bureau/template:agent"},
		},
	})

	session := newImpactTestSession(t, state)
	analyzer := &impactAnalyzer{
		session:    session,
		serverName: impactServerName,
		ctx:        t.Context(),
		cache:      make(map[string]*schema.TemplateContent),
	}

	configs, err := analyzer.discoverMachineConfigs()
	if err != nil {
		t.Fatalf("discoverMachineConfigs: %v", err)
	}
	affected, err := analyzer.findAffected("bureau/template:agent", configs)
	if err != nil {
		t.Fatalf("findAffected: %v", err)
	}

	// Only change default_payload.
	proposed := &schema.TemplateContent{
		Description:    "Agent template",
		Command:        []string{"/usr/bin/agent"},
		DefaultPayload: map[string]any{"model": "gpt-4"},
	}

	if err := analyzer.classifyChanges(affected, "bureau/template:agent", proposed); err != nil {
		t.Fatalf("classifyChanges: %v", err)
	}

	if affected[0].Change != "payload-only" {
		t.Errorf("Change = %q, want payload-only", affected[0].Change)
	}
}

func TestImpactChangeClassificationMetadata(t *testing.T) {
	t.Parallel()

	state := newImpactTestState()

	state.addTemplate(
		"#bureau/template:test.local", "!template:test",
		"agent", schema.TemplateContent{
			Description: "Agent template",
			Command:     []string{"/usr/bin/agent"},
		},
	)

	state.addConfigRoom("!config-ws:test", "machine/workstation", impactServerName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{Localpart: "test/agent", Template: "bureau/template:agent"},
		},
	})

	session := newImpactTestSession(t, state)
	analyzer := &impactAnalyzer{
		session:    session,
		serverName: impactServerName,
		ctx:        t.Context(),
		cache:      make(map[string]*schema.TemplateContent),
	}

	configs, err := analyzer.discoverMachineConfigs()
	if err != nil {
		t.Fatalf("discoverMachineConfigs: %v", err)
	}
	affected, err := analyzer.findAffected("bureau/template:agent", configs)
	if err != nil {
		t.Fatalf("findAffected: %v", err)
	}

	// Only change description — metadata only.
	proposed := &schema.TemplateContent{
		Description: "Updated agent template description",
		Command:     []string{"/usr/bin/agent"},
	}

	if err := analyzer.classifyChanges(affected, "bureau/template:agent", proposed); err != nil {
		t.Fatalf("classifyChanges: %v", err)
	}

	if affected[0].Change != "metadata" {
		t.Errorf("Change = %q, want metadata", affected[0].Change)
	}
	if len(affected[0].ChangedFields) != 1 || affected[0].ChangedFields[0] != "description" {
		t.Errorf("ChangedFields = %v, want [description]", affected[0].ChangedFields)
	}
}

func TestImpactChangeClassificationNoChange(t *testing.T) {
	t.Parallel()

	state := newImpactTestState()

	state.addTemplate(
		"#bureau/template:test.local", "!template:test",
		"agent", schema.TemplateContent{
			Description: "Agent template",
			Command:     []string{"/usr/bin/agent"},
		},
	)

	state.addConfigRoom("!config-ws:test", "machine/workstation", impactServerName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{Localpart: "test/agent", Template: "bureau/template:agent"},
		},
	})

	session := newImpactTestSession(t, state)
	analyzer := &impactAnalyzer{
		session:    session,
		serverName: impactServerName,
		ctx:        t.Context(),
		cache:      make(map[string]*schema.TemplateContent),
	}

	configs, err := analyzer.discoverMachineConfigs()
	if err != nil {
		t.Fatalf("discoverMachineConfigs: %v", err)
	}
	affected, err := analyzer.findAffected("bureau/template:agent", configs)
	if err != nil {
		t.Fatalf("findAffected: %v", err)
	}

	// Identical template — no change.
	proposed := &schema.TemplateContent{
		Description: "Agent template",
		Command:     []string{"/usr/bin/agent"},
	}

	if err := analyzer.classifyChanges(affected, "bureau/template:agent", proposed); err != nil {
		t.Fatalf("classifyChanges: %v", err)
	}

	if affected[0].Change != "no change" {
		t.Errorf("Change = %q, want 'no change'", affected[0].Change)
	}
	if len(affected[0].ChangedFields) != 0 {
		t.Errorf("ChangedFields = %v, want empty", affected[0].ChangedFields)
	}
}

func TestImpactChangeWithInheritance(t *testing.T) {
	t.Parallel()

	state := newImpactTestState()

	// Base template provides namespaces and filesystem.
	state.addTemplate(
		"#bureau/template:test.local", "!template:test",
		"base", schema.TemplateContent{
			Description: "Base template",
			Command:     []string{"/bin/bash"},
			Namespaces:  &schema.TemplateNamespaces{PID: true, Net: true},
			Filesystem: []schema.TemplateMount{
				{Source: "/usr", Dest: "/usr", Mode: "ro"},
			},
		},
	)

	// Child inherits from base, adds its own command.
	state.addTemplate(
		"#bureau/template:test.local", "!template:test",
		"agent", schema.TemplateContent{
			Inherits:    "bureau/template:base",
			Description: "Agent template",
			Command:     []string{"/usr/bin/agent"},
			EnvironmentVariables: map[string]string{
				"AGENT": "true",
			},
		},
	)

	// Principal uses the child template. Impact targets the base.
	state.addConfigRoom("!config-ws:test", "machine/workstation", impactServerName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{Localpart: "test/agent", Template: "bureau/template:agent"},
		},
	})

	session := newImpactTestSession(t, state)
	analyzer := &impactAnalyzer{
		session:    session,
		serverName: impactServerName,
		ctx:        t.Context(),
		cache:      make(map[string]*schema.TemplateContent),
	}

	configs, err := analyzer.discoverMachineConfigs()
	if err != nil {
		t.Fatalf("discoverMachineConfigs: %v", err)
	}
	affected, err := analyzer.findAffected("bureau/template:base", configs)
	if err != nil {
		t.Fatalf("findAffected: %v", err)
	}
	if len(affected) != 1 {
		t.Fatalf("affected count = %d, want 1", len(affected))
	}

	// Change the base template's namespace config — structural change visible
	// through the inheritance chain.
	proposed := &schema.TemplateContent{
		Description: "Base template",
		Command:     []string{"/bin/bash"},
		Namespaces:  &schema.TemplateNamespaces{PID: true, Net: false, IPC: true},
		Filesystem: []schema.TemplateMount{
			{Source: "/usr", Dest: "/usr", Mode: "ro"},
		},
	}

	if err := analyzer.classifyChanges(affected, "bureau/template:base", proposed); err != nil {
		t.Fatalf("classifyChanges: %v", err)
	}

	if affected[0].Change != "structural" {
		t.Errorf("Change = %q, want structural", affected[0].Change)
	}
	foundNamespaces := false
	for _, field := range affected[0].ChangedFields {
		if field == "namespaces" {
			foundNamespaces = true
		}
	}
	if !foundNamespaces {
		t.Errorf("ChangedFields = %v, expected to contain 'namespaces'", affected[0].ChangedFields)
	}
}

func TestImpactSkipsPrincipalsWithoutTemplate(t *testing.T) {
	t.Parallel()

	state := newImpactTestState()

	state.addTemplate(
		"#bureau/template:test.local", "!template:test",
		"agent", schema.TemplateContent{
			Description: "Agent template",
			Command:     []string{"/usr/bin/agent"},
		},
	)

	state.addConfigRoom("!config-ws:test", "machine/workstation", impactServerName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{Localpart: "test/agent", Template: "bureau/template:agent"},
			{Localpart: "test/no-template", Template: ""},
		},
	})

	session := newImpactTestSession(t, state)
	analyzer := &impactAnalyzer{
		session:    session,
		serverName: impactServerName,
		ctx:        t.Context(),
		cache:      make(map[string]*schema.TemplateContent),
	}

	configs, err := analyzer.discoverMachineConfigs()
	if err != nil {
		t.Fatalf("discoverMachineConfigs: %v", err)
	}

	affected, err := analyzer.findAffected("bureau/template:agent", configs)
	if err != nil {
		t.Fatalf("findAffected: %v", err)
	}
	if len(affected) != 1 {
		t.Fatalf("affected count = %d, want 1 (principal without template should be skipped)", len(affected))
	}
	if affected[0].Principal != "test/agent" {
		t.Errorf("Principal = %q, want test/agent", affected[0].Principal)
	}
}

func TestClassifyTemplateChange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		current        *schema.TemplateContent
		proposed       *schema.TemplateContent
		wantChange     string
		wantFieldCount int
	}{
		{
			name: "no change",
			current: &schema.TemplateContent{
				Description: "Test",
				Command:     []string{"/bin/test"},
			},
			proposed: &schema.TemplateContent{
				Description: "Test",
				Command:     []string{"/bin/test"},
			},
			wantChange:     "no change",
			wantFieldCount: 0,
		},
		{
			name: "description only is metadata",
			current: &schema.TemplateContent{
				Description: "Old",
				Command:     []string{"/bin/test"},
			},
			proposed: &schema.TemplateContent{
				Description: "New",
				Command:     []string{"/bin/test"},
			},
			wantChange:     "metadata",
			wantFieldCount: 1,
		},
		{
			name: "default_payload only is payload-only",
			current: &schema.TemplateContent{
				DefaultPayload: map[string]any{"model": "a"},
			},
			proposed: &schema.TemplateContent{
				DefaultPayload: map[string]any{"model": "b"},
			},
			wantChange:     "payload-only",
			wantFieldCount: 1,
		},
		{
			name: "description plus payload is payload-only",
			current: &schema.TemplateContent{
				Description:    "Old",
				DefaultPayload: map[string]any{"model": "a"},
			},
			proposed: &schema.TemplateContent{
				Description:    "New",
				DefaultPayload: map[string]any{"model": "b"},
			},
			wantChange:     "payload-only",
			wantFieldCount: 2,
		},
		{
			name: "command change is structural",
			current: &schema.TemplateContent{
				Command: []string{"/bin/old"},
			},
			proposed: &schema.TemplateContent{
				Command: []string{"/bin/new"},
			},
			wantChange:     "structural",
			wantFieldCount: 1,
		},
		{
			name: "environment_variables change is structural",
			current: &schema.TemplateContent{
				EnvironmentVariables: map[string]string{"A": "1"},
			},
			proposed: &schema.TemplateContent{
				EnvironmentVariables: map[string]string{"A": "2"},
			},
			wantChange:     "structural",
			wantFieldCount: 1,
		},
		{
			name: "filesystem change is structural",
			current: &schema.TemplateContent{
				Filesystem: []schema.TemplateMount{{Dest: "/old"}},
			},
			proposed: &schema.TemplateContent{
				Filesystem: []schema.TemplateMount{{Dest: "/new"}},
			},
			wantChange:     "structural",
			wantFieldCount: 1,
		},
		{
			name: "mixed structural and payload is structural",
			current: &schema.TemplateContent{
				Command:        []string{"/bin/old"},
				DefaultPayload: map[string]any{"k": "v1"},
			},
			proposed: &schema.TemplateContent{
				Command:        []string{"/bin/new"},
				DefaultPayload: map[string]any{"k": "v2"},
			},
			wantChange:     "structural",
			wantFieldCount: 2,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			change, fields := classifyTemplateChange(test.current, test.proposed)
			if change != test.wantChange {
				t.Errorf("change = %q, want %q", change, test.wantChange)
			}
			if len(fields) != test.wantFieldCount {
				t.Errorf("field count = %d, want %d (fields: %v)", len(fields), test.wantFieldCount, fields)
			}
		})
	}
}
