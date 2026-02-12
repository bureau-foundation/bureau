// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package messaging

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestSession creates a Client and Session pointing at a test server.
func newTestSession(t *testing.T, handler http.Handler) (*Client, *Session) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client, err := NewClient(ClientConfig{HomeserverURL: server.URL})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	session, err := client.SessionFromToken("@test:local", "test-token")
	if err != nil {
		t.Fatalf("SessionFromToken failed: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return client, session
}

func TestWhoAmI(t *testing.T) {
	_, session := newTestSession(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertAuth(t, request, "test-token")
		if request.URL.Path != "/_matrix/client/v3/account/whoami" {
			t.Errorf("unexpected path: %s", request.URL.Path)
		}
		writeJSON(writer, WhoAmIResponse{UserID: "@test:local", DeviceID: "DEV1"})
	}))

	userID, err := session.WhoAmI(context.Background())
	if err != nil {
		t.Fatalf("WhoAmI failed: %v", err)
	}
	if userID != "@test:local" {
		t.Errorf("unexpected user ID: %s", userID)
	}
}

func TestCreateRoom(t *testing.T) {
	t.Run("basic room", func(t *testing.T) {
		_, session := newTestSession(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			assertAuth(t, request, "test-token")
			if request.URL.Path != "/_matrix/client/v3/createRoom" {
				t.Errorf("unexpected path: %s", request.URL.Path)
			}

			var body CreateRoomRequest
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatalf("failed to decode request: %v", err)
			}
			if body.Name != "Test Room" {
				t.Errorf("unexpected name: %s", body.Name)
			}
			if body.Alias != "test" {
				t.Errorf("unexpected alias: %s", body.Alias)
			}

			writeJSON(writer, CreateRoomResponse{RoomID: "!room1:local"})
		}))

		response, err := session.CreateRoom(context.Background(), CreateRoomRequest{
			Name:   "Test Room",
			Alias:  "test",
			Preset: "public_chat",
		})
		if err != nil {
			t.Fatalf("CreateRoom failed: %v", err)
		}
		if response.RoomID != "!room1:local" {
			t.Errorf("unexpected room ID: %s", response.RoomID)
		}
	})

	t.Run("space creation", func(t *testing.T) {
		_, session := newTestSession(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatalf("failed to decode request: %v", err)
			}
			creationContent, ok := body["creation_content"].(map[string]any)
			if !ok {
				t.Fatal("missing creation_content")
			}
			if creationContent["type"] != "m.space" {
				t.Errorf("unexpected creation_content type: %v", creationContent["type"])
			}
			writeJSON(writer, CreateRoomResponse{RoomID: "!space1:local"})
		}))

		response, err := session.CreateRoom(context.Background(), CreateRoomRequest{
			Name:  "Test Space",
			Alias: "space",
			CreationContent: map[string]any{
				"type": "m.space",
			},
		})
		if err != nil {
			t.Fatalf("CreateRoom (space) failed: %v", err)
		}
		if response.RoomID != "!space1:local" {
			t.Errorf("unexpected room ID: %s", response.RoomID)
		}
	})
}

func TestJoinRoom(t *testing.T) {
	_, session := newTestSession(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertAuth(t, request, "test-token")
		// The room ID is URL-encoded in the path.
		if !strings.HasPrefix(request.URL.Path, "/_matrix/client/v3/join/") {
			t.Errorf("unexpected path: %s", request.URL.Path)
		}
		writeJSON(writer, map[string]string{"room_id": "!room1:local"})
	}))

	roomID, err := session.JoinRoom(context.Background(), "#test:local")
	if err != nil {
		t.Fatalf("JoinRoom failed: %v", err)
	}
	if roomID != "!room1:local" {
		t.Errorf("unexpected room ID: %s", roomID)
	}
}

func TestInviteUser(t *testing.T) {
	_, session := newTestSession(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertAuth(t, request, "test-token")

		var body InviteRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatalf("failed to decode invite: %v", err)
		}
		if body.UserID != "@alice:local" {
			t.Errorf("unexpected invite target: %s", body.UserID)
		}
		writeJSON(writer, map[string]any{})
	}))

	err := session.InviteUser(context.Background(), "!room1:local", "@alice:local")
	if err != nil {
		t.Fatalf("InviteUser failed: %v", err)
	}
}

func TestSendMessage(t *testing.T) {
	t.Run("plain text message", func(t *testing.T) {
		_, session := newTestSession(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			assertAuth(t, request, "test-token")
			if request.Method != http.MethodPut {
				t.Errorf("expected PUT, got %s", request.Method)
			}
			if !strings.Contains(request.URL.Path, "/send/m.room.message/") {
				t.Errorf("unexpected path: %s", request.URL.Path)
			}

			var body MessageContent
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatalf("failed to decode message: %v", err)
			}
			if body.MsgType != "m.text" {
				t.Errorf("unexpected msgtype: %s", body.MsgType)
			}
			if body.Body != "hello world" {
				t.Errorf("unexpected body: %s", body.Body)
			}
			if body.RelatesTo != nil {
				t.Error("plain message should not have relates_to")
			}

			writeJSON(writer, SendEventResponse{EventID: "$event1"})
		}))

		eventID, err := session.SendMessage(context.Background(), "!room1:local", NewTextMessage("hello world"))
		if err != nil {
			t.Fatalf("SendMessage failed: %v", err)
		}
		if eventID != "$event1" {
			t.Errorf("unexpected event ID: %s", eventID)
		}
	})

	t.Run("thread reply", func(t *testing.T) {
		_, session := newTestSession(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			var body MessageContent
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatalf("failed to decode message: %v", err)
			}
			if body.RelatesTo == nil {
				t.Fatal("thread reply should have relates_to")
			}
			if body.RelatesTo.RelType != "m.thread" {
				t.Errorf("unexpected rel_type: %s", body.RelatesTo.RelType)
			}
			if body.RelatesTo.EventID != "$root1" {
				t.Errorf("unexpected thread root: %s", body.RelatesTo.EventID)
			}
			if body.RelatesTo.InReplyTo == nil {
				t.Fatal("thread reply should have in_reply_to")
			}
			if body.RelatesTo.InReplyTo.EventID != "$root1" {
				t.Errorf("unexpected in_reply_to: %s", body.RelatesTo.InReplyTo.EventID)
			}

			writeJSON(writer, SendEventResponse{EventID: "$event2"})
		}))

		eventID, err := session.SendMessage(context.Background(), "!room1:local", NewThreadReply("$root1", "thread response"))
		if err != nil {
			t.Fatalf("SendMessage (thread) failed: %v", err)
		}
		if eventID != "$event2" {
			t.Errorf("unexpected event ID: %s", eventID)
		}
	})
}

func TestSendStateEvent(t *testing.T) {
	_, session := newTestSession(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertAuth(t, request, "test-token")
		if request.Method != http.MethodPut {
			t.Errorf("expected PUT, got %s", request.Method)
		}
		// Path should include /state/m.space.child/{child_room_id}
		if !strings.Contains(request.URL.Path, "/state/m.space.child/") {
			t.Errorf("unexpected path: %s", request.URL.Path)
		}

		writeJSON(writer, SendEventResponse{EventID: "$state1"})
	}))

	eventID, err := session.SendStateEvent(context.Background(), "!space1:local", "m.space.child", "!room1:local",
		map[string]any{"via": []string{"local"}})
	if err != nil {
		t.Fatalf("SendStateEvent failed: %v", err)
	}
	if eventID != "$state1" {
		t.Errorf("unexpected event ID: %s", eventID)
	}
}

func TestGetStateEvent(t *testing.T) {
	t.Run("existing event", func(t *testing.T) {
		_, session := newTestSession(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			assertAuth(t, request, "test-token")
			if request.Method != http.MethodGet {
				t.Errorf("expected GET, got %s", request.Method)
			}
			// Path should be /rooms/{roomId}/state/{eventType}/{stateKey}
			expectedPath := "/_matrix/client/v3/rooms/%21room1%3Alocal/state/m.bureau.machine_key/machine%2Fworkstation"
			if request.URL.RawPath != expectedPath && request.URL.Path != "/_matrix/client/v3/rooms/!room1:local/state/m.bureau.machine_key/machine/workstation" {
				t.Errorf("unexpected path: raw=%s path=%s", request.URL.RawPath, request.URL.Path)
			}

			// Matrix GET /state/{type}/{key} returns just the content.
			writeJSON(writer, map[string]string{
				"algorithm":  "age-x25519",
				"public_key": "age1testkey",
			})
		}))

		content, err := session.GetStateEvent(context.Background(), "!room1:local", "m.bureau.machine_key", "machine/workstation")
		if err != nil {
			t.Fatalf("GetStateEvent failed: %v", err)
		}

		// Unmarshal the raw content into the expected type.
		var machineKey struct {
			Algorithm string `json:"algorithm"`
			PublicKey string `json:"public_key"`
		}
		if err := json.Unmarshal(content, &machineKey); err != nil {
			t.Fatalf("failed to unmarshal content: %v", err)
		}
		if machineKey.Algorithm != "age-x25519" {
			t.Errorf("algorithm = %q, want %q", machineKey.Algorithm, "age-x25519")
		}
		if machineKey.PublicKey != "age1testkey" {
			t.Errorf("public_key = %q, want %q", machineKey.PublicKey, "age1testkey")
		}
	})

	t.Run("not found", func(t *testing.T) {
		_, session := newTestSession(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusNotFound)
			json.NewEncoder(writer).Encode(MatrixError{Code: ErrCodeNotFound, Message: "State event not found"})
		}))

		_, err := session.GetStateEvent(context.Background(), "!room1:local", "m.bureau.machine_key", "nonexistent")
		if err == nil {
			t.Fatal("expected error for missing state event")
		}
		if !IsMatrixError(err, ErrCodeNotFound) {
			t.Errorf("expected M_NOT_FOUND, got: %v", err)
		}
	})
}

func TestGetRoomState(t *testing.T) {
	_, session := newTestSession(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertAuth(t, request, "test-token")
		if request.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", request.Method)
		}
		if !strings.HasSuffix(request.URL.Path, "/state") {
			t.Errorf("unexpected path: %s", request.URL.Path)
		}

		stateKey1 := "machine/workstation"
		stateKey2 := ""
		events := []Event{
			{
				EventID:  "$s1",
				Type:     "m.bureau.machine_key",
				Sender:   "@machine/workstation:local",
				StateKey: &stateKey1,
				Content:  map[string]any{"algorithm": "age-x25519", "public_key": "age1test"},
			},
			{
				EventID:  "$s2",
				Type:     "m.room.power_levels",
				Sender:   "@admin:local",
				StateKey: &stateKey2,
				Content:  map[string]any{"users_default": float64(0)},
			},
		}
		writeJSON(writer, events)
	}))

	events, err := session.GetRoomState(context.Background(), "!room1:local")
	if err != nil {
		t.Fatalf("GetRoomState failed: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 state events, got %d", len(events))
	}
	if events[0].Type != "m.bureau.machine_key" {
		t.Errorf("first event type = %q, want %q", events[0].Type, "m.bureau.machine_key")
	}
	if events[0].StateKey == nil || *events[0].StateKey != "machine/workstation" {
		t.Errorf("first event state_key unexpected")
	}
	if events[1].Type != "m.room.power_levels" {
		t.Errorf("second event type = %q, want %q", events[1].Type, "m.room.power_levels")
	}
}

func TestRoomMessages(t *testing.T) {
	_, session := newTestSession(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertAuth(t, request, "test-token")
		if !strings.Contains(request.URL.Path, "/messages") {
			t.Errorf("unexpected path: %s", request.URL.Path)
		}

		query := request.URL.Query()
		if query.Get("dir") != "b" {
			t.Errorf("unexpected direction: %s", query.Get("dir"))
		}
		if query.Get("limit") != "10" {
			t.Errorf("unexpected limit: %s", query.Get("limit"))
		}

		writeJSON(writer, RoomMessagesResponse{
			Start: "t1",
			End:   "t2",
			Chunk: []Event{
				{EventID: "$msg1", Type: "m.room.message", Sender: "@alice:local"},
				{EventID: "$msg2", Type: "m.room.message", Sender: "@bob:local"},
			},
		})
	}))

	response, err := session.RoomMessages(context.Background(), "!room1:local", RoomMessagesOptions{
		Direction: "b",
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("RoomMessages failed: %v", err)
	}
	if len(response.Chunk) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(response.Chunk))
	}
	if response.Chunk[0].EventID != "$msg1" {
		t.Errorf("unexpected first event ID: %s", response.Chunk[0].EventID)
	}
}

func TestThreadMessages(t *testing.T) {
	_, session := newTestSession(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertAuth(t, request, "test-token")
		if !strings.Contains(request.URL.Path, "/relations/") {
			t.Errorf("unexpected path: %s", request.URL.Path)
		}
		if !strings.HasSuffix(request.URL.Path, "/m.thread") {
			t.Errorf("expected path to end with /m.thread: %s", request.URL.Path)
		}

		writeJSON(writer, ThreadMessagesResponse{
			Chunk: []Event{
				{EventID: "$reply1", Type: "m.room.message", Sender: "@bob:local"},
			},
			NextBatch: "next-page-token",
		})
	}))

	response, err := session.ThreadMessages(context.Background(), "!room1:local", "$root1", ThreadMessagesOptions{
		Limit: 50,
	})
	if err != nil {
		t.Fatalf("ThreadMessages failed: %v", err)
	}
	if len(response.Chunk) != 1 {
		t.Fatalf("expected 1 thread message, got %d", len(response.Chunk))
	}
	if response.NextBatch != "next-page-token" {
		t.Errorf("unexpected next_batch: %s", response.NextBatch)
	}
}

func TestSync(t *testing.T) {
	_, session := newTestSession(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertAuth(t, request, "test-token")
		if request.URL.Path != "/_matrix/client/v3/sync" {
			t.Errorf("unexpected path: %s", request.URL.Path)
		}

		query := request.URL.Query()
		if query.Get("since") != "s123" {
			t.Errorf("unexpected since token: %s", query.Get("since"))
		}
		if query.Get("timeout") != "0" {
			t.Errorf("unexpected timeout: %s", query.Get("timeout"))
		}

		writeJSON(writer, SyncResponse{
			NextBatch: "s456",
			Rooms: RoomsSection{
				Join: map[string]JoinedRoom{
					"!room1:local": {
						Timeline: TimelineSection{
							Events: []Event{
								{EventID: "$evt1", Type: "m.room.message", Sender: "@alice:local"},
							},
						},
					},
				},
			},
		})
	}))

	response, err := session.Sync(context.Background(), SyncOptions{
		Since:      "s123",
		Timeout:    0,
		SetTimeout: true,
	})
	if err != nil {
		t.Fatalf("Sync failed: %v", err)
	}
	if response.NextBatch != "s456" {
		t.Errorf("unexpected next_batch: %s", response.NextBatch)
	}
	room, ok := response.Rooms.Join["!room1:local"]
	if !ok {
		t.Fatal("expected room !room1:local in sync response")
	}
	if len(room.Timeline.Events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(room.Timeline.Events))
	}
}

func TestResolveAlias(t *testing.T) {
	t.Run("alias exists", func(t *testing.T) {
		_, session := newTestSession(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			assertAuth(t, request, "test-token")
			if !strings.HasPrefix(request.URL.Path, "/_matrix/client/v3/directory/room/") {
				t.Errorf("unexpected path: %s", request.URL.Path)
			}
			writeJSON(writer, ResolveAliasResponse{
				RoomID:  "!room1:local",
				Servers: []string{"local"},
			})
		}))

		roomID, err := session.ResolveAlias(context.Background(), "#test:local")
		if err != nil {
			t.Fatalf("ResolveAlias failed: %v", err)
		}
		if roomID != "!room1:local" {
			t.Errorf("unexpected room ID: %s", roomID)
		}
	})

	t.Run("alias not found", func(t *testing.T) {
		_, session := newTestSession(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusNotFound)
			json.NewEncoder(writer).Encode(MatrixError{Code: ErrCodeNotFound, Message: "Room alias not found"})
		}))

		_, err := session.ResolveAlias(context.Background(), "#nonexistent:local")
		if err == nil {
			t.Fatal("expected error for missing alias")
		}
		if !IsMatrixError(err, ErrCodeNotFound) {
			t.Errorf("expected M_NOT_FOUND, got: %v", err)
		}
	})
}

func TestUploadMedia(t *testing.T) {
	_, session := newTestSession(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertAuth(t, request, "test-token")
		if request.URL.Path != "/_matrix/media/v3/upload" {
			t.Errorf("unexpected path: %s", request.URL.Path)
		}
		if request.Header.Get("Content-Type") != "image/png" {
			t.Errorf("unexpected content type: %s", request.Header.Get("Content-Type"))
		}

		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("failed to read body: %v", err)
		}
		if string(body) != "fake-png-data" {
			t.Errorf("unexpected body: %s", string(body))
		}

		writeJSON(writer, UploadResponse{ContentURI: "mxc://local/abc123"})
	}))

	mxcURI, err := session.UploadMedia(context.Background(), "image/png", bytes.NewReader([]byte("fake-png-data")))
	if err != nil {
		t.Fatalf("UploadMedia failed: %v", err)
	}
	if mxcURI != "mxc://local/abc123" {
		t.Errorf("unexpected MXC URI: %s", mxcURI)
	}
}

func TestJoinedRooms(t *testing.T) {
	_, session := newTestSession(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertAuth(t, request, "test-token")
		if request.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", request.Method)
		}
		if request.URL.Path != "/_matrix/client/v3/joined_rooms" {
			t.Errorf("unexpected path: %s", request.URL.Path)
		}
		writeJSON(writer, JoinedRoomsResponse{
			JoinedRooms: []string{"!room1:local", "!room2:local", "!space1:local"},
		})
	}))

	rooms, err := session.JoinedRooms(context.Background())
	if err != nil {
		t.Fatalf("JoinedRooms failed: %v", err)
	}
	if len(rooms) != 3 {
		t.Fatalf("expected 3 rooms, got %d", len(rooms))
	}
	if rooms[0] != "!room1:local" {
		t.Errorf("unexpected first room: %s", rooms[0])
	}
	if rooms[2] != "!space1:local" {
		t.Errorf("unexpected third room: %s", rooms[2])
	}
}

func TestLeaveRoom(t *testing.T) {
	_, session := newTestSession(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertAuth(t, request, "test-token")
		if request.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", request.Method)
		}
		if !strings.Contains(request.URL.Path, "/leave") {
			t.Errorf("unexpected path: %s", request.URL.Path)
		}
		writeJSON(writer, map[string]any{})
	}))

	err := session.LeaveRoom(context.Background(), "!room1:local")
	if err != nil {
		t.Fatalf("LeaveRoom failed: %v", err)
	}
}

func TestGetRoomMembers(t *testing.T) {
	_, session := newTestSession(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertAuth(t, request, "test-token")
		if request.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", request.Method)
		}
		if !strings.Contains(request.URL.Path, "/members") {
			t.Errorf("unexpected path: %s", request.URL.Path)
		}
		writeJSON(writer, RoomMembersResponse{
			Chunk: []RoomMemberEvent{
				{
					Type:     "m.room.member",
					StateKey: "@alice:local",
					Sender:   "@alice:local",
					Content: RoomMemberContent{
						Membership:  "join",
						DisplayName: "Alice",
					},
				},
				{
					Type:     "m.room.member",
					StateKey: "@bob:local",
					Sender:   "@alice:local",
					Content: RoomMemberContent{
						Membership:  "invite",
						DisplayName: "Bob",
					},
				},
			},
		})
	}))

	members, err := session.GetRoomMembers(context.Background(), "!room1:local")
	if err != nil {
		t.Fatalf("GetRoomMembers failed: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(members))
	}
	if members[0].UserID != "@alice:local" {
		t.Errorf("unexpected first member user ID: %s", members[0].UserID)
	}
	if members[0].DisplayName != "Alice" {
		t.Errorf("unexpected first member display name: %s", members[0].DisplayName)
	}
	if members[0].Membership != "join" {
		t.Errorf("unexpected first member membership: %s", members[0].Membership)
	}
	if members[1].UserID != "@bob:local" {
		t.Errorf("unexpected second member user ID: %s", members[1].UserID)
	}
	if members[1].Membership != "invite" {
		t.Errorf("unexpected second member membership: %s", members[1].Membership)
	}
}

func TestKickUser(t *testing.T) {
	t.Run("with reason", func(t *testing.T) {
		_, session := newTestSession(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			assertAuth(t, request, "test-token")
			if request.Method != http.MethodPost {
				t.Errorf("expected POST, got %s", request.Method)
			}
			if !strings.Contains(request.URL.Path, "/kick") {
				t.Errorf("unexpected path: %s", request.URL.Path)
			}

			var body KickRequest
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatalf("failed to decode kick request: %v", err)
			}
			if body.UserID != "@alice:local" {
				t.Errorf("unexpected kick target: %s", body.UserID)
			}
			if body.Reason != "misbehaving" {
				t.Errorf("unexpected reason: %s", body.Reason)
			}
			writeJSON(writer, map[string]any{})
		}))

		err := session.KickUser(context.Background(), "!room1:local", "@alice:local", "misbehaving")
		if err != nil {
			t.Fatalf("KickUser failed: %v", err)
		}
	})

	t.Run("without reason", func(t *testing.T) {
		_, session := newTestSession(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			var body KickRequest
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatalf("failed to decode kick request: %v", err)
			}
			if body.UserID != "@bob:local" {
				t.Errorf("unexpected kick target: %s", body.UserID)
			}
			if body.Reason != "" {
				t.Errorf("expected empty reason, got: %s", body.Reason)
			}
			writeJSON(writer, map[string]any{})
		}))

		err := session.KickUser(context.Background(), "!room1:local", "@bob:local", "")
		if err != nil {
			t.Fatalf("KickUser failed: %v", err)
		}
	})
}

func TestGetDisplayName(t *testing.T) {
	t.Run("has display name", func(t *testing.T) {
		_, session := newTestSession(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			assertAuth(t, request, "test-token")
			if request.Method != http.MethodGet {
				t.Errorf("expected GET, got %s", request.Method)
			}
			if !strings.Contains(request.URL.Path, "/profile/") || !strings.HasSuffix(request.URL.Path, "/displayname") {
				t.Errorf("unexpected path: %s", request.URL.Path)
			}
			writeJSON(writer, DisplayNameResponse{DisplayName: "Alice Wonderland"})
		}))

		displayName, err := session.GetDisplayName(context.Background(), "@alice:local")
		if err != nil {
			t.Fatalf("GetDisplayName failed: %v", err)
		}
		if displayName != "Alice Wonderland" {
			t.Errorf("unexpected display name: %s", displayName)
		}
	})

	t.Run("no display name set", func(t *testing.T) {
		_, session := newTestSession(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writeJSON(writer, DisplayNameResponse{})
		}))

		displayName, err := session.GetDisplayName(context.Background(), "@bob:local")
		if err != nil {
			t.Fatalf("GetDisplayName failed: %v", err)
		}
		if displayName != "" {
			t.Errorf("expected empty display name, got: %s", displayName)
		}
	})

	t.Run("user not found", func(t *testing.T) {
		_, session := newTestSession(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusNotFound)
			json.NewEncoder(writer).Encode(MatrixError{Code: ErrCodeNotFound, Message: "User not found"})
		}))

		_, err := session.GetDisplayName(context.Background(), "@nonexistent:local")
		if err == nil {
			t.Fatal("expected error for unknown user")
		}
		if !IsMatrixError(err, ErrCodeNotFound) {
			t.Errorf("expected M_NOT_FOUND, got: %v", err)
		}
	})
}

func TestTransactionIDUniqueness(t *testing.T) {
	// Verify that consecutive sends produce different transaction IDs.
	transactionIDs := make(map[string]bool)
	callCount := 0

	_, session := newTestSession(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// Extract txnID from the path (last segment).
		parts := strings.Split(request.URL.Path, "/")
		transactionID := parts[len(parts)-1]
		if transactionIDs[transactionID] {
			t.Errorf("duplicate transaction ID: %s", transactionID)
		}
		transactionIDs[transactionID] = true
		callCount++
		writeJSON(writer, SendEventResponse{EventID: "$evt"})
	}))

	for range 5 {
		_, err := session.SendMessage(context.Background(), "!room1:local", NewTextMessage("msg"))
		if err != nil {
			t.Fatalf("SendMessage failed: %v", err)
		}
	}

	if callCount != 5 {
		t.Errorf("expected 5 calls, got %d", callCount)
	}
	if len(transactionIDs) != 5 {
		t.Errorf("expected 5 unique transaction IDs, got %d", len(transactionIDs))
	}
}

func TestTURNCredentials(t *testing.T) {
	_, session := newTestSession(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertAuth(t, request, "test-token")
		if request.URL.Path != "/_matrix/client/v3/voip/turnServer" {
			t.Errorf("unexpected path: %s", request.URL.Path)
		}
		if request.Method != http.MethodGet {
			t.Errorf("unexpected method: %s", request.Method)
		}
		writeJSON(writer, map[string]any{
			"username": "1234567890:@user:test.local",
			"password": "hmac-secret",
			"uris":     []string{"turn:turn.test.local:3478?transport=udp", "turn:turn.test.local:3478?transport=tcp"},
			"ttl":      86400,
		})
	}))

	credentials, err := session.TURNCredentials(context.Background())
	if err != nil {
		t.Fatalf("TURNCredentials failed: %v", err)
	}
	if credentials.Username != "1234567890:@user:test.local" {
		t.Errorf("Username = %q, want %q", credentials.Username, "1234567890:@user:test.local")
	}
	if credentials.Password != "hmac-secret" {
		t.Errorf("Password = %q, want %q", credentials.Password, "hmac-secret")
	}
	if len(credentials.URIs) != 2 {
		t.Fatalf("URIs length = %d, want 2", len(credentials.URIs))
	}
	if credentials.URIs[0] != "turn:turn.test.local:3478?transport=udp" {
		t.Errorf("URIs[0] = %q, want TURN UDP URI", credentials.URIs[0])
	}
	if credentials.TTL != 86400 {
		t.Errorf("TTL = %d, want 86400", credentials.TTL)
	}
}

func TestTURNCredentials_NotConfigured(t *testing.T) {
	_, session := newTestSession(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/_matrix/client/v3/voip/turnServer" {
			writer.WriteHeader(http.StatusNotFound)
			writeJSON(writer, map[string]string{
				"errcode": "M_NOT_FOUND",
				"error":   "TURN is not configured",
			})
			return
		}
		http.Error(writer, "not found", http.StatusNotFound)
	}))

	_, err := session.TURNCredentials(context.Background())
	if err == nil {
		t.Fatal("expected error for unconfigured TURN, got nil")
	}
}

func TestChangePassword(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		_, session := newTestSession(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			assertAuth(t, request, "test-token")
			if request.Method != http.MethodPost {
				t.Errorf("expected POST, got %s", request.Method)
			}
			if request.URL.Path != "/_matrix/client/v3/account/password" {
				t.Errorf("unexpected path: %s", request.URL.Path)
			}

			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatalf("failed to decode request body: %v", err)
			}

			if body["new_password"] != "new-secret-password" {
				t.Errorf("new_password = %q, want %q", body["new_password"], "new-secret-password")
			}

			auth, ok := body["auth"].(map[string]any)
			if !ok {
				t.Fatal("missing auth block in request body")
			}
			if auth["type"] != "m.login.password" {
				t.Errorf("auth type = %q, want %q", auth["type"], "m.login.password")
			}
			if auth["user"] != "@test:local" {
				t.Errorf("auth user = %q, want %q", auth["user"], "@test:local")
			}
			if auth["password"] != "old-password" {
				t.Errorf("auth password = %q, want %q", auth["password"], "old-password")
			}

			writeJSON(writer, map[string]any{})
		}))

		err := session.ChangePassword(context.Background(), testBuffer(t, "old-password"), testBuffer(t, "new-secret-password"))
		if err != nil {
			t.Fatalf("ChangePassword failed: %v", err)
		}
	})

	t.Run("wrong current password", func(t *testing.T) {
		_, session := newTestSession(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(writer).Encode(MatrixError{Code: ErrCodeForbidden, Message: "Invalid password"})
		}))

		err := session.ChangePassword(context.Background(), testBuffer(t, "wrong-password"), testBuffer(t, "new-password"))
		if err == nil {
			t.Fatal("expected error for wrong password")
		}
		if !IsMatrixError(err, ErrCodeForbidden) {
			t.Errorf("expected M_FORBIDDEN, got: %v", err)
		}
	})

	t.Run("nil current password", func(t *testing.T) {
		_, session := newTestSession(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatal("server should not be called")
		}))

		err := session.ChangePassword(context.Background(), nil, testBuffer(t, "new-password"))
		if err == nil {
			t.Fatal("expected error for nil current password")
		}
	})

	t.Run("nil new password", func(t *testing.T) {
		_, session := newTestSession(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatal("server should not be called")
		}))

		err := session.ChangePassword(context.Background(), testBuffer(t, "old-password"), nil)
		if err == nil {
			t.Fatal("expected error for nil new password")
		}
	})
}

// Test helpers.

func assertAuth(t *testing.T, request *http.Request, expectedToken string) {
	t.Helper()
	auth := request.Header.Get("Authorization")
	expected := "Bearer " + expectedToken
	if auth != expected {
		t.Errorf("unexpected auth header: got %q, want %q", auth, expected)
	}
}

func writeJSON(writer http.ResponseWriter, value any) {
	writer.Header().Set("Content-Type", "application/json")
	json.NewEncoder(writer).Encode(value)
}
