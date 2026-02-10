// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package matrix

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bureau-foundation/bureau/messaging"
)

func TestSpaceCreate_MissingAlias(t *testing.T) {
	command := spaceCreateCommand()
	err := command.Execute([]string{
		"--homeserver", "http://localhost:6167",
		"--token", "tok",
		"--user-id", "@a:b",
	})
	if err == nil {
		t.Fatal("expected error for missing space alias")
	}
}

func TestSpaceCreate_TooManyArgs(t *testing.T) {
	command := spaceCreateCommand()
	err := command.Execute([]string{
		"--homeserver", "http://localhost:6167",
		"--token", "tok",
		"--user-id", "@a:b",
		"my-project", "extra",
	})
	if err == nil {
		t.Fatal("expected error for extra argument")
	}
}

func TestSpaceCreate_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/_matrix/client/v3/createRoom" {
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatalf("failed to decode request: %v", err)
			}
			creationContent, ok := body["creation_content"].(map[string]any)
			if !ok {
				t.Fatal("missing creation_content")
			}
			if creationContent["type"] != "m.space" {
				t.Errorf("expected m.space creation type, got %v", creationContent["type"])
			}
			if body["name"] != "My Project" {
				t.Errorf("unexpected name: %v", body["name"])
			}
			if body["room_alias_name"] != "my-project" {
				t.Errorf("unexpected alias: %v", body["room_alias_name"])
			}
			writer.Header().Set("Content-Type", "application/json")
			json.NewEncoder(writer).Encode(messaging.CreateRoomResponse{RoomID: "!space1:local"})
			return
		}
		t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
	}))
	defer server.Close()

	command := spaceCreateCommand()
	err := command.Execute([]string{
		"--homeserver", server.URL,
		"--token", "tok",
		"--user-id", "@admin:local",
		"--name", "My Project",
		"my-project",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSpaceCreate_NameDefaultsToAlias(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/_matrix/client/v3/createRoom" {
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatalf("failed to decode request: %v", err)
			}
			if body["name"] != "iree" {
				t.Errorf("expected name to default to alias 'iree', got %v", body["name"])
			}
			if body["room_alias_name"] != "iree" {
				t.Errorf("unexpected alias: %v", body["room_alias_name"])
			}
			writer.Header().Set("Content-Type", "application/json")
			json.NewEncoder(writer).Encode(messaging.CreateRoomResponse{RoomID: "!space2:local"})
			return
		}
		t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
	}))
	defer server.Close()

	command := spaceCreateCommand()
	err := command.Execute([]string{
		"--homeserver", server.URL,
		"--token", "tok",
		"--user-id", "@admin:local",
		"iree",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSpaceList_UnexpectedArg(t *testing.T) {
	command := spaceListCommand()
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

func TestSpaceDelete_MissingTarget(t *testing.T) {
	command := spaceDeleteCommand()
	err := command.Execute([]string{
		"--homeserver", "http://localhost:6167",
		"--token", "tok",
		"--user-id", "@a:b",
	})
	if err == nil {
		t.Fatal("expected error for missing target")
	}
}

func TestSpaceMembers_MissingTarget(t *testing.T) {
	command := spaceMembersCommand()
	err := command.Execute([]string{
		"--homeserver", "http://localhost:6167",
		"--token", "tok",
		"--user-id", "@a:b",
	})
	if err == nil {
		t.Fatal("expected error for missing target")
	}
}

func TestInspectSpaceState(t *testing.T) {
	t.Run("is a space", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			emptyStateKey := ""
			events := []messaging.Event{
				{
					Type:     "m.room.create",
					StateKey: &emptyStateKey,
					Content:  map[string]any{"type": "m.space"},
				},
				{
					Type:     "m.room.name",
					StateKey: &emptyStateKey,
					Content:  map[string]any{"name": "Test Space"},
				},
				{
					Type:     "m.room.canonical_alias",
					StateKey: &emptyStateKey,
					Content:  map[string]any{"alias": "#test:local"},
				},
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

		isSpace, name, alias := inspectSpaceState(t.Context(), session, "!room1:local")
		if !isSpace {
			t.Error("expected isSpace to be true")
		}
		if name != "Test Space" {
			t.Errorf("expected name 'Test Space', got %q", name)
		}
		if alias != "#test:local" {
			t.Errorf("expected alias '#test:local', got %q", alias)
		}
	})

	t.Run("not a space", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			emptyStateKey := ""
			events := []messaging.Event{
				{
					Type:     "m.room.create",
					StateKey: &emptyStateKey,
					Content:  map[string]any{"room_version": "11"},
				},
				{
					Type:     "m.room.name",
					StateKey: &emptyStateKey,
					Content:  map[string]any{"name": "Regular Room"},
				},
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

		isSpace, _, _ := inspectSpaceState(t.Context(), session, "!room1:local")
		if isSpace {
			t.Error("expected isSpace to be false for a regular room")
		}
	})
}
