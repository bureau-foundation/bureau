// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package proxyclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/messaging"
)

// mustRoomID parses a room ID string in test code, panicking on failure.
func mustRoomID(raw string) ref.RoomID {
	roomID, err := ref.ParseRoomID(raw)
	if err != nil {
		panic(fmt.Sprintf("invalid test room ID %q: %v", raw, err))
	}
	return roomID
}

// testProxySession creates a ProxySession backed by a testServer that handles
// the routes needed for a given test. The session's user ID is set to
// "@agent/test:test.local".
func testProxySession(t *testing.T, handler http.Handler) *ProxySession {
	t.Helper()
	client := testServer(t, handler)
	return NewProxySession(client, "@agent/test:test.local")
}

func TestProxySessionUserID(t *testing.T) {
	t.Parallel()

	session := NewProxySession(New("/tmp/nonexistent.sock", "test.local"), "@agent/x:test.local")
	if session.UserID() != "@agent/x:test.local" {
		t.Errorf("UserID = %q, want @agent/x:test.local", session.UserID())
	}
}

func TestProxySessionClose(t *testing.T) {
	t.Parallel()

	session := NewProxySession(New("/tmp/nonexistent.sock", "test.local"), "@agent/x:test.local")
	// Close is a no-op — should not error.
	if err := session.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestProxySessionClient(t *testing.T) {
	t.Parallel()

	client := New("/tmp/nonexistent.sock", "test.local")
	session := NewProxySession(client, "@agent/x:test.local")
	if session.Client() != client {
		t.Error("Client() did not return the underlying client")
	}
}

func TestProxySessionWhoAmI(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/matrix/whoami", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]string{"user_id": "@agent/test:test.local"})
	})

	session := testProxySession(t, mux)
	userID, err := session.WhoAmI(context.Background())
	if err != nil {
		t.Fatalf("WhoAmI: %v", err)
	}
	if userID != "@agent/test:test.local" {
		t.Errorf("UserID = %q, want @agent/test:test.local", userID)
	}
}

func TestProxySessionResolveAlias(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/matrix/resolve", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]string{"room_id": "!resolved:test.local"})
	})

	session := testProxySession(t, mux)
	roomID, err := session.ResolveAlias(context.Background(), "#test:test.local")
	if err != nil {
		t.Fatalf("ResolveAlias: %v", err)
	}
	if roomID.String() != "!resolved:test.local" {
		t.Errorf("RoomID = %q, want !resolved:test.local", roomID)
	}
}

func TestProxySessionGetStateEvent(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/matrix/state", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprint(writer, `{"description":"test state"}`)
	})

	session := testProxySession(t, mux)
	content, err := session.GetStateEvent(context.Background(), mustRoomID("!room:test"), "m.bureau.test", "key")
	if err != nil {
		t.Fatalf("GetStateEvent: %v", err)
	}
	var parsed map[string]string
	if err := json.Unmarshal(content, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed["description"] != "test state" {
		t.Errorf("description = %q, want test state", parsed["description"])
	}
}

func TestProxySessionSendStateEvent(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/matrix/state", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]string{"event_id": "$state1"})
	})

	session := testProxySession(t, mux)
	eventID, err := session.SendStateEvent(context.Background(), mustRoomID("!room:test"), "m.bureau.test", "key", map[string]string{"value": "test"})
	if err != nil {
		t.Fatalf("SendStateEvent: %v", err)
	}
	if eventID != "$state1" {
		t.Errorf("EventID = %q, want $state1", eventID)
	}
}

func TestProxySessionSendMessage(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/matrix/message", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]string{"event_id": "$msg1"})
	})

	session := testProxySession(t, mux)
	eventID, err := session.SendMessage(context.Background(), mustRoomID("!room:test"), messaging.NewTextMessage("hello"))
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if eventID != "$msg1" {
		t.Errorf("EventID = %q, want $msg1", eventID)
	}
}

func TestProxySessionSendEvent(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/matrix/event", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]string{"event_id": "$evt1"})
	})

	session := testProxySession(t, mux)
	eventID, err := session.SendEvent(context.Background(), mustRoomID("!room:test"), "m.bureau.test", map[string]string{"key": "value"})
	if err != nil {
		t.Fatalf("SendEvent: %v", err)
	}
	if eventID != "$evt1" {
		t.Errorf("EventID = %q, want $evt1", eventID)
	}
}

func TestProxySessionCreateRoom(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/matrix/room", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]string{"room_id": "!new:test.local"})
	})

	session := testProxySession(t, mux)
	response, err := session.CreateRoom(context.Background(), messaging.CreateRoomRequest{Name: "Test"})
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	if response.RoomID.String() != "!new:test.local" {
		t.Errorf("RoomID = %q, want !new:test.local", response.RoomID)
	}
}

func TestProxySessionJoinRoom(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/matrix/join", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]string{"room_id": "!joined:test.local"})
	})

	session := testProxySession(t, mux)
	roomID, err := session.JoinRoom(context.Background(), mustRoomID("!joined:test.local"))
	if err != nil {
		t.Fatalf("JoinRoom: %v", err)
	}
	if roomID.String() != "!joined:test.local" {
		t.Errorf("RoomID = %q, want !joined:test.local", roomID)
	}
}

func TestProxySessionInviteUser(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/matrix/invite", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Write([]byte("{}"))
	})

	session := testProxySession(t, mux)
	err := session.InviteUser(context.Background(), mustRoomID("!room:test"), "@other:test.local")
	if err != nil {
		t.Fatalf("InviteUser: %v", err)
	}
}

func TestProxySessionJoinedRooms(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/matrix/joined-rooms", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string][]string{
			"joined_rooms": {"!room1:test.local", "!room2:test.local"},
		})
	})

	session := testProxySession(t, mux)
	rooms, err := session.JoinedRooms(context.Background())
	if err != nil {
		t.Fatalf("JoinedRooms: %v", err)
	}
	if len(rooms) != 2 {
		t.Fatalf("got %d rooms, want 2", len(rooms))
	}
}

func TestProxySessionGetRoomMembers(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/matrix/room-members", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprint(writer, `{"chunk":[{"type":"m.room.member","state_key":"@a:test.local","sender":"@admin:test.local","content":{"membership":"join","displayname":"Agent A"}}]}`)
	})

	session := testProxySession(t, mux)
	members, err := session.GetRoomMembers(context.Background(), mustRoomID("!room:test"))
	if err != nil {
		t.Fatalf("GetRoomMembers: %v", err)
	}
	if len(members) != 1 {
		t.Fatalf("got %d members, want 1", len(members))
	}
	if members[0].DisplayName != "Agent A" {
		t.Errorf("DisplayName = %q, want Agent A", members[0].DisplayName)
	}
}

func TestProxySessionGetDisplayName(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/matrix/display-name", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprint(writer, `{"displayname":"Test Agent"}`)
	})

	session := testProxySession(t, mux)
	name, err := session.GetDisplayName(context.Background(), "@agent:test.local")
	if err != nil {
		t.Fatalf("GetDisplayName: %v", err)
	}
	if name != "Test Agent" {
		t.Errorf("DisplayName = %q, want Test Agent", name)
	}
}

func TestProxySessionGetRoomState(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/matrix/room-state", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprint(writer, `[{"type":"m.room.topic","state_key":"","sender":"@admin:test.local","content":{"topic":"test"}}]`)
	})

	session := testProxySession(t, mux)
	events, err := session.GetRoomState(context.Background(), mustRoomID("!room:test"))
	if err != nil {
		t.Fatalf("GetRoomState: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Type != "m.room.topic" {
		t.Errorf("event type = %q, want m.room.topic", events[0].Type)
	}
}

func TestProxySessionRoomMessages(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/matrix/messages", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprint(writer, `{"start":"s1","end":"s2","chunk":[]}`)
	})

	session := testProxySession(t, mux)
	response, err := session.RoomMessages(context.Background(), mustRoomID("!room:test"), messaging.RoomMessagesOptions{Direction: "b"})
	if err != nil {
		t.Fatalf("RoomMessages: %v", err)
	}
	if response.Start != "s1" {
		t.Errorf("Start = %q, want s1", response.Start)
	}
}

func TestProxySessionThreadMessages(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/matrix/thread-messages", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprint(writer, `{"chunk":[],"next_batch":"nb1"}`)
	})

	session := testProxySession(t, mux)
	response, err := session.ThreadMessages(context.Background(), mustRoomID("!room:test"), "$root", messaging.ThreadMessagesOptions{})
	if err != nil {
		t.Fatalf("ThreadMessages: %v", err)
	}
	if response.NextBatch != "nb1" {
		t.Errorf("NextBatch = %q, want nb1", response.NextBatch)
	}
}
