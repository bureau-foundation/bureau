// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package matrix

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/messaging"
)

func TestRoomCreate_MissingAlias(t *testing.T) {
	command := roomCreateCommand()
	err := command.Execute([]string{
		"--homeserver", "http://localhost:6167",
		"--token", "tok",
		"--user-id", "@a:b",
		"--space", "!space:local",
	})
	if err == nil {
		t.Fatal("expected error for missing alias")
	}
	if !strings.Contains(err.Error(), "room alias is required") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestRoomCreate_TooManyArgs(t *testing.T) {
	command := roomCreateCommand()
	err := command.Execute([]string{
		"--homeserver", "http://localhost:6167",
		"--token", "tok",
		"--user-id", "@a:b",
		"--space", "!space:local",
		"bureau/agents", "extra",
	})
	if err == nil {
		t.Fatal("expected error for extra argument")
	}
}

func TestRoomCreate_MissingSpace(t *testing.T) {
	command := roomCreateCommand()
	err := command.Execute([]string{
		"--homeserver", "http://localhost:6167",
		"--token", "tok",
		"--user-id", "@a:b",
		"bureau/agents",
	})
	if err == nil {
		t.Fatal("expected error for missing --space")
	}
	if !strings.Contains(err.Error(), "--space is required") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestRoomCreate_Success(t *testing.T) {
	var (
		gotCreateRoom    bool
		gotSpaceChild    bool
		capturedPowerLvl map[string]any
	)

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")

		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/_matrix/client/v3/createRoom":
			gotCreateRoom = true
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatalf("decode createRoom: %v", err)
			}
			if body["room_alias_name"] != "bureau/agents" {
				t.Errorf("unexpected alias: %v", body["room_alias_name"])
			}
			if body["name"] != "Bureau Agents" {
				t.Errorf("unexpected name: %v", body["name"])
			}
			if body["topic"] != "Agent registry" {
				t.Errorf("unexpected topic: %v", body["topic"])
			}
			if body["preset"] != "private_chat" {
				t.Errorf("unexpected preset: %v", body["preset"])
			}
			if powerLevels, ok := body["power_level_content_override"].(map[string]any); ok {
				capturedPowerLvl = powerLevels
			}
			json.NewEncoder(writer).Encode(messaging.CreateRoomResponse{RoomID: "!newroom:local"})

		case request.Method == http.MethodPut && strings.Contains(request.URL.Path, "/state/m.space.child/"):
			gotSpaceChild = true
			// The state key should be the new room ID (URL-encoded).
			if !strings.HasSuffix(request.URL.RawPath, "%21newroom%3Alocal") &&
				!strings.HasSuffix(request.URL.Path, "/!newroom:local") {
				t.Errorf("unexpected state key path: raw=%s decoded=%s", request.URL.RawPath, request.URL.Path)
			}
			// Verify the via field.
			var content map[string]any
			if err := json.NewDecoder(request.Body).Decode(&content); err != nil {
				t.Fatalf("decode m.space.child: %v", err)
			}
			via, ok := content["via"].([]any)
			if !ok || len(via) != 1 || via[0] != "bureau.local" {
				t.Errorf("unexpected via: %v", content["via"])
			}
			json.NewEncoder(writer).Encode(messaging.SendEventResponse{EventID: "$evt1"})

		default:
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
			http.Error(writer, `{"errcode":"M_UNKNOWN"}`, http.StatusNotFound)
		}
	}))
	defer server.Close()

	command := roomCreateCommand()
	err := command.Execute([]string{
		"--homeserver", server.URL,
		"--token", "tok",
		"--user-id", "@admin:local",
		"--space", "!space:local",
		"--name", "Bureau Agents",
		"--topic", "Agent registry",
		"--member-state-event", "m.bureau.machine_key",
		"bureau/agents",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !gotCreateRoom {
		t.Error("createRoom was not called")
	}
	if !gotSpaceChild {
		t.Error("m.space.child state event was not sent")
	}

	// Verify that the member-state-event flag resulted in power_level 0 for that type.
	if capturedPowerLvl != nil {
		if events, ok := capturedPowerLvl["events"].(map[string]any); ok {
			if level, exists := events["m.bureau.machine_key"]; !exists {
				t.Error("m.bureau.machine_key not found in power level events")
			} else if level != float64(0) {
				t.Errorf("expected m.bureau.machine_key power level 0, got %v", level)
			}
		}
	}
}

func TestRoomCreate_DefaultNameFromAlias(t *testing.T) {
	var capturedName string

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")

		if request.Method == http.MethodPost && request.URL.Path == "/_matrix/client/v3/createRoom" {
			var body map[string]any
			json.NewDecoder(request.Body).Decode(&body)
			capturedName, _ = body["name"].(string)
			json.NewEncoder(writer).Encode(messaging.CreateRoomResponse{RoomID: "!room:local"})
			return
		}
		if strings.Contains(request.URL.Path, "/state/m.space.child/") {
			json.NewEncoder(writer).Encode(messaging.SendEventResponse{EventID: "$e1"})
			return
		}
		http.Error(writer, `{"errcode":"M_UNKNOWN"}`, http.StatusNotFound)
	}))
	defer server.Close()

	command := roomCreateCommand()
	err := command.Execute([]string{
		"--homeserver", server.URL,
		"--token", "tok",
		"--user-id", "@admin:local",
		"--space", "!space:local",
		"iree/amdgpu/general",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedName != "iree/amdgpu/general" {
		t.Errorf("expected name to default to alias, got %q", capturedName)
	}
}

func TestRoomCreate_SpaceResolvedByAlias(t *testing.T) {
	var gotResolve bool

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")

		if strings.HasPrefix(request.URL.Path, "/_matrix/client/v3/directory/room/") {
			gotResolve = true
			json.NewEncoder(writer).Encode(messaging.ResolveAliasResponse{
				RoomID: "!resolved-space:local",
			})
			return
		}
		if request.URL.Path == "/_matrix/client/v3/createRoom" {
			json.NewEncoder(writer).Encode(messaging.CreateRoomResponse{RoomID: "!room:local"})
			return
		}
		if strings.Contains(request.URL.Path, "/state/m.space.child/") {
			json.NewEncoder(writer).Encode(messaging.SendEventResponse{EventID: "$e1"})
			return
		}
		http.Error(writer, `{"errcode":"M_UNKNOWN"}`, http.StatusNotFound)
	}))
	defer server.Close()

	command := roomCreateCommand()
	err := command.Execute([]string{
		"--homeserver", server.URL,
		"--token", "tok",
		"--user-id", "@admin:local",
		"--space", "#bureau:bureau.local",
		"bureau/test",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !gotResolve {
		t.Error("expected alias resolution for space")
	}
}

func TestRoomList_UnexpectedArg(t *testing.T) {
	command := roomListCommand()
	err := command.Execute([]string{
		"--homeserver", "http://localhost:6167",
		"--token", "tok",
		"--user-id", "@a:b",
		"extra",
	})
	if err == nil {
		t.Fatal("expected error for unexpected argument")
	}
}

func TestRoomList_WithSpace(t *testing.T) {
	emptyStateKey := ""
	childKey1 := "!child1:local"
	childKey2 := "!child2:local"

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")

		switch {
		// State of the space room: two children.
		case strings.Contains(request.URL.Path, "/rooms/%21space%3Alocal/state") ||
			strings.Contains(request.URL.Path, "/rooms/!space:local/state"):
			events := []messaging.Event{
				{Type: "m.room.create", StateKey: &emptyStateKey, Content: map[string]any{"type": "m.space"}},
				{Type: "m.space.child", StateKey: &childKey1, Content: map[string]any{"via": []string{"local"}}},
				{Type: "m.space.child", StateKey: &childKey2, Content: map[string]any{"via": []string{"local"}}},
			}
			json.NewEncoder(writer).Encode(events)

		// State of child1.
		case strings.Contains(request.URL.Path, "/rooms/%21child1%3Alocal/state") ||
			strings.Contains(request.URL.Path, "/rooms/!child1:local/state"):
			events := []messaging.Event{
				{Type: "m.room.name", StateKey: &emptyStateKey, Content: map[string]any{"name": "Room One"}},
				{Type: "m.room.canonical_alias", StateKey: &emptyStateKey, Content: map[string]any{"alias": "#one:local"}},
				{Type: "m.room.topic", StateKey: &emptyStateKey, Content: map[string]any{"topic": "First room"}},
			}
			json.NewEncoder(writer).Encode(events)

		// State of child2.
		case strings.Contains(request.URL.Path, "/rooms/%21child2%3Alocal/state") ||
			strings.Contains(request.URL.Path, "/rooms/!child2:local/state"):
			events := []messaging.Event{
				{Type: "m.room.name", StateKey: &emptyStateKey, Content: map[string]any{"name": "Room Two"}},
			}
			json.NewEncoder(writer).Encode(events)

		default:
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
			http.Error(writer, `{"errcode":"M_UNKNOWN"}`, http.StatusNotFound)
		}
	}))
	defer server.Close()

	command := roomListCommand()
	err := command.Execute([]string{
		"--homeserver", server.URL,
		"--token", "tok",
		"--user-id", "@admin:local",
		"--space", "!space:local",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRoomList_AllRooms(t *testing.T) {
	emptyStateKey := ""

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")

		switch {
		case request.URL.Path == "/_matrix/client/v3/joined_rooms":
			json.NewEncoder(writer).Encode(messaging.JoinedRoomsResponse{
				JoinedRooms: []string{"!space1:local", "!room1:local", "!room2:local"},
			})

		// space1 is a space — should be excluded.
		case strings.Contains(request.URL.Path, "/rooms/%21space1%3Alocal/state") ||
			strings.Contains(request.URL.Path, "/rooms/!space1:local/state"):
			events := []messaging.Event{
				{Type: "m.room.create", StateKey: &emptyStateKey, Content: map[string]any{"type": "m.space"}},
				{Type: "m.room.name", StateKey: &emptyStateKey, Content: map[string]any{"name": "Space"}},
			}
			json.NewEncoder(writer).Encode(events)

		// room1 is a regular room.
		case strings.Contains(request.URL.Path, "/rooms/%21room1%3Alocal/state") ||
			strings.Contains(request.URL.Path, "/rooms/!room1:local/state"):
			events := []messaging.Event{
				{Type: "m.room.create", StateKey: &emptyStateKey, Content: map[string]any{"room_version": "11"}},
				{Type: "m.room.name", StateKey: &emptyStateKey, Content: map[string]any{"name": "Agents"}},
				{Type: "m.room.canonical_alias", StateKey: &emptyStateKey, Content: map[string]any{"alias": "#agents:local"}},
			}
			json.NewEncoder(writer).Encode(events)

		// room2 is a regular room with no name.
		case strings.Contains(request.URL.Path, "/rooms/%21room2%3Alocal/state") ||
			strings.Contains(request.URL.Path, "/rooms/!room2:local/state"):
			events := []messaging.Event{
				{Type: "m.room.create", StateKey: &emptyStateKey, Content: map[string]any{"room_version": "11"}},
			}
			json.NewEncoder(writer).Encode(events)

		default:
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
			http.Error(writer, `{"errcode":"M_UNKNOWN"}`, http.StatusNotFound)
		}
	}))
	defer server.Close()

	command := roomListCommand()
	err := command.Execute([]string{
		"--homeserver", server.URL,
		"--token", "tok",
		"--user-id", "@admin:local",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRoomDelete_MissingTarget(t *testing.T) {
	command := roomDeleteCommand()
	err := command.Execute([]string{
		"--homeserver", "http://localhost:6167",
		"--token", "tok",
		"--user-id", "@a:b",
	})
	if err == nil {
		t.Fatal("expected error for missing target")
	}
}

func TestRoomDelete_TooManyArgs(t *testing.T) {
	command := roomDeleteCommand()
	err := command.Execute([]string{
		"--homeserver", "http://localhost:6167",
		"--token", "tok",
		"--user-id", "@a:b",
		"!room:local", "extra",
	})
	if err == nil {
		t.Fatal("expected error for extra argument")
	}
}

func TestRoomDelete_Success(t *testing.T) {
	var gotLeave bool

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")

		if request.Method == http.MethodPost && strings.Contains(request.URL.Path, "/leave") {
			gotLeave = true
			json.NewEncoder(writer).Encode(struct{}{})
			return
		}
		t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
	}))
	defer server.Close()

	command := roomDeleteCommand()
	err := command.Execute([]string{
		"--homeserver", server.URL,
		"--token", "tok",
		"--user-id", "@admin:local",
		"!room:local",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !gotLeave {
		t.Error("leave endpoint was not called")
	}
}

func TestRoomMembers_MissingTarget(t *testing.T) {
	command := roomMembersCommand()
	err := command.Execute([]string{
		"--homeserver", "http://localhost:6167",
		"--token", "tok",
		"--user-id", "@a:b",
	})
	if err == nil {
		t.Fatal("expected error for missing target")
	}
}

func TestRoomMembers_TooManyArgs(t *testing.T) {
	command := roomMembersCommand()
	err := command.Execute([]string{
		"--homeserver", "http://localhost:6167",
		"--token", "tok",
		"--user-id", "@a:b",
		"!room:local", "extra",
	})
	if err == nil {
		t.Fatal("expected error for extra argument")
	}
}

func TestRoomMembers_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")

		if strings.Contains(request.URL.Path, "/members") {
			response := map[string]any{
				"chunk": []map[string]any{
					{
						"type":      "m.room.member",
						"state_key": "@alice:local",
						"sender":    "@alice:local",
						"content":   map[string]any{"membership": "join", "displayname": "Alice"},
					},
					{
						"type":      "m.room.member",
						"state_key": "@bob:local",
						"sender":    "@alice:local",
						"content":   map[string]any{"membership": "invite"},
					},
				},
			}
			json.NewEncoder(writer).Encode(response)
			return
		}
		t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
	}))
	defer server.Close()

	command := roomMembersCommand()
	err := command.Execute([]string{
		"--homeserver", server.URL,
		"--token", "tok",
		"--user-id", "@admin:local",
		"!room:local",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestInspectRoomState(t *testing.T) {
	t.Run("all fields present", func(t *testing.T) {
		emptyStateKey := ""
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			events := []messaging.Event{
				{Type: "m.room.name", StateKey: &emptyStateKey, Content: map[string]any{"name": "Test Room"}},
				{Type: "m.room.canonical_alias", StateKey: &emptyStateKey, Content: map[string]any{"alias": "#test:local"}},
				{Type: "m.room.topic", StateKey: &emptyStateKey, Content: map[string]any{"topic": "A topic"}},
			}
			writer.Header().Set("Content-Type", "application/json")
			json.NewEncoder(writer).Encode(events)
		}))
		defer server.Close()

		client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: server.URL})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}
		session := client.SessionFromToken("@test:local", "test-token")

		name, alias, topic := inspectRoomState(t.Context(), session, "!room:local")
		if name != "Test Room" {
			t.Errorf("expected name 'Test Room', got %q", name)
		}
		if alias != "#test:local" {
			t.Errorf("expected alias '#test:local', got %q", alias)
		}
		if topic != "A topic" {
			t.Errorf("expected topic 'A topic', got %q", topic)
		}
	})

	t.Run("no fields set", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			json.NewEncoder(writer).Encode([]messaging.Event{})
		}))
		defer server.Close()

		client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: server.URL})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}
		session := client.SessionFromToken("@test:local", "test-token")

		name, alias, topic := inspectRoomState(t.Context(), session, "!room:local")
		if name != "" {
			t.Errorf("expected empty name, got %q", name)
		}
		if alias != "" {
			t.Errorf("expected empty alias, got %q", alias)
		}
		if topic != "" {
			t.Errorf("expected empty topic, got %q", topic)
		}
	})

	t.Run("server error returns empty", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			http.Error(writer, `{"errcode":"M_FORBIDDEN"}`, http.StatusForbidden)
		}))
		defer server.Close()

		client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: server.URL})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}
		session := client.SessionFromToken("@test:local", "test-token")

		name, alias, topic := inspectRoomState(t.Context(), session, "!room:local")
		if name != "" || alias != "" || topic != "" {
			t.Errorf("expected all empty on error, got name=%q alias=%q topic=%q", name, alias, topic)
		}
	})
}

