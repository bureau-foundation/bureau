// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package proxyclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// testServer creates a test HTTP server that mimics the proxy API and
// returns a Client connected to it. The server is cleaned up when the
// test completes.
func testServer(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return &Client{
		httpClient: &http.Client{
			Transport: &testServerTransport{
				server:    server,
				transport: http.DefaultTransport,
			},
		},
		serverName: ref.MustParseServerName("test.local"),
	}
}

// testServerTransport rewrites requests to target the test server.
type testServerTransport struct {
	server    *httptest.Server
	transport http.RoundTripper
}

func (transport *testServerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request.URL.Scheme = "http"
	request.URL.Host = transport.server.Listener.Addr().String()
	return transport.transport.RoundTrip(request)
}

func TestIdentity(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/identity", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(IdentityResponse{
			UserID:     "@agent/test:test.local",
			ServerName: "test.local",
		})
	})

	client := testServer(t, mux)
	identity, err := client.Identity(context.Background())
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if identity.UserID != "@agent/test:test.local" {
		t.Errorf("UserID = %q, want @agent/test:test.local", identity.UserID)
	}
	if identity.ServerName != "test.local" {
		t.Errorf("ServerName = %q, want test.local", identity.ServerName)
	}
}

func TestIdentityError(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/identity", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(writer).Encode(map[string]string{"error": "identity not configured"})
	})

	client := testServer(t, mux)
	_, err := client.Identity(context.Background())
	if err == nil {
		t.Fatal("expected error for 503 response")
	}
	if got := err.Error(); got == "" {
		t.Error("error message should not be empty")
	}
}

func TestGrants(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/grants", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode([]schema.Grant{
			{Actions: []string{"matrix/send"}, Targets: []string{"bureau/fleet/*/machine/*"}},
		})
	})

	client := testServer(t, mux)
	grants, err := client.Grants(context.Background())
	if err != nil {
		t.Fatalf("Grants: %v", err)
	}
	if len(grants) != 1 {
		t.Fatalf("got %d grants, want 1", len(grants))
	}
	if len(grants[0].Actions) != 1 || grants[0].Actions[0] != "matrix/send" {
		t.Errorf("Actions = %v, want [matrix/send]", grants[0].Actions)
	}
}

func TestServices(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/services", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode([]ServiceEntry{
			{
				Localpart:   "service/stt/whisper",
				Principal:   "@service/stt/whisper:test.local",
				Machine:     "@machine/gpu:test.local",
				Protocol:    "http",
				Description: "Speech to text",
			},
		})
	})

	client := testServer(t, mux)
	services, err := client.Services(context.Background())
	if err != nil {
		t.Fatalf("Services: %v", err)
	}
	if len(services) != 1 {
		t.Fatalf("got %d services, want 1", len(services))
	}
	if services[0].Localpart != "service/stt/whisper" {
		t.Errorf("Localpart = %q, want service/stt/whisper", services[0].Localpart)
	}
	if services[0].Protocol != "http" {
		t.Errorf("Protocol = %q, want http", services[0].Protocol)
	}
}

func TestWhoami(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/matrix/whoami", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]string{"user_id": "@agent/test:test.local"})
	})

	client := testServer(t, mux)
	userID, err := client.Whoami(context.Background())
	if err != nil {
		t.Fatalf("Whoami: %v", err)
	}
	expected := ref.MustParseUserID("@agent/test:test.local")
	if userID != expected {
		t.Errorf("UserID = %s, want %s", userID, expected)
	}
}

func TestWhoamiServerName(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/matrix/whoami", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]string{"user_id": "@agent/test:bureau.example.com"})
	})

	client := testServer(t, mux)
	serverName, err := client.WhoamiServerName(context.Background())
	if err != nil {
		t.Fatalf("WhoamiServerName: %v", err)
	}
	if serverName.String() != "bureau.example.com" {
		t.Errorf("ServerName = %q, want bureau.example.com", serverName)
	}
}

func TestDiscoverServerName(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/matrix/whoami", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]string{"user_id": "@pipeline/test:discovered.local"})
	})

	client := testServer(t, mux)
	// Before discovery, server name is the constructor value.
	if client.ServerName().String() != "test.local" {
		t.Errorf("ServerName before discover = %q, want test.local", client.ServerName())
	}

	serverName, err := client.DiscoverServerName(context.Background())
	if err != nil {
		t.Fatalf("DiscoverServerName: %v", err)
	}
	if serverName.String() != "discovered.local" {
		t.Errorf("returned ServerName = %q, want discovered.local", serverName)
	}
	if client.ServerName().String() != "discovered.local" {
		t.Errorf("ServerName after discover = %q, want discovered.local", client.ServerName())
	}
}

func TestResolveAlias(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/matrix/resolve", func(writer http.ResponseWriter, request *http.Request) {
		alias := request.URL.Query().Get("alias")
		if alias != "#bureau/fleet/prod/machine/ws:test.local" {
			writer.WriteHeader(http.StatusNotFound)
			json.NewEncoder(writer).Encode(map[string]string{"error": "not found"})
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]string{"room_id": "!config:test"})
	})

	client := testServer(t, mux)

	roomID, err := client.ResolveAlias(context.Background(), ref.MustParseRoomAlias("#bureau/fleet/prod/machine/ws:test.local"))
	if err != nil {
		t.Fatalf("ResolveAlias: %v", err)
	}
	expectedRoomID := ref.MustParseRoomID("!config:test")
	if roomID != expectedRoomID {
		t.Errorf("RoomID = %s, want %s", roomID, expectedRoomID)
	}

	_, err = client.ResolveAlias(context.Background(), ref.MustParseRoomAlias("#nonexistent:test.local"))
	if err == nil {
		t.Fatal("expected error for nonexistent alias")
	}
}

func TestGetState(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/matrix/state", func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		if query.Get("room") != "!room:test" || query.Get("type") != "m.bureau.template" || query.Get("key") != "base" {
			writer.WriteHeader(http.StatusNotFound)
			json.NewEncoder(writer).Encode(map[string]string{"error": "not found"})
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprint(writer, `{"description":"Base template"}`)
	})

	client := testServer(t, mux)
	content, err := client.GetState(context.Background(), ref.MustParseRoomID("!room:test"), "m.bureau.template", "base")
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	var parsed map[string]string
	if err := json.Unmarshal(content, &parsed); err != nil {
		t.Fatalf("unmarshal state content: %v", err)
	}
	if parsed["description"] != "Base template" {
		t.Errorf("description = %q, want Base template", parsed["description"])
	}
}

func TestPutState(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/matrix/state", func(writer http.ResponseWriter, request *http.Request) {
		var body PutStateRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if body.Room.String() != "!room:test" || body.EventType != "m.bureau.test" {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]string{"event_id": "$event123"})
	})

	client := testServer(t, mux)
	eventID, err := client.PutState(context.Background(), PutStateRequest{
		Room:      ref.MustParseRoomID("!room:test"),
		EventType: "m.bureau.test",
		StateKey:  "key",
		Content:   map[string]string{"value": "test"},
	})
	if err != nil {
		t.Fatalf("PutState: %v", err)
	}
	if eventID != "$event123" {
		t.Errorf("EventID = %q, want $event123", eventID)
	}
}

func TestSendMessage(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/matrix/message", func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if body["room"] != "!room:test" {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]string{"event_id": "$msg456"})
	})

	client := testServer(t, mux)
	eventID, err := client.SendTextMessage(context.Background(), ref.MustParseRoomID("!room:test"), "hello")
	if err != nil {
		t.Fatalf("SendTextMessage: %v", err)
	}
	if eventID != "$msg456" {
		t.Errorf("EventID = %q, want $msg456", eventID)
	}
}

func TestSync(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()

	// Mock the structured /v1/matrix/sync endpoint.
	mux.HandleFunc("GET /v1/matrix/sync", func(writer http.ResponseWriter, request *http.Request) {
		since := request.URL.Query().Get("since")
		timeout := request.URL.Query().Get("timeout")

		writer.Header().Set("Content-Type", "application/json")
		if since == "" && timeout == "0" {
			// Initial sync: return a batch token and no events.
			json.NewEncoder(writer).Encode(map[string]any{
				"next_batch": "s_initial",
				"rooms":      map[string]any{"join": map[string]any{}},
			})
			return
		}

		// Incremental sync: return a message event in the room.
		json.NewEncoder(writer).Encode(map[string]any{
			"next_batch": "s_after_message",
			"rooms": map[string]any{
				"join": map[string]any{
					"!config:test": map[string]any{
						"timeline": map[string]any{
							"events": []map[string]any{
								{
									"type":   "m.room.message",
									"sender": "@admin:test.local",
									"content": map[string]any{
										"msgtype": "m.text",
										"body":    "hello agent",
									},
								},
							},
						},
					},
				},
			},
		})
	})

	client := testServer(t, mux)

	// Initial sync with timeout=0.
	response, err := client.Sync(context.Background(), messaging.SyncOptions{Timeout: 0, SetTimeout: true})
	if err != nil {
		t.Fatalf("initial Sync: %v", err)
	}
	if response.NextBatch != "s_initial" {
		t.Errorf("initial NextBatch = %q, want s_initial", response.NextBatch)
	}
	if length := len(response.Rooms.Join); length != 0 {
		t.Errorf("initial sync should have 0 joined rooms, got %d", length)
	}

	// Incremental sync with the since token.
	response, err = client.Sync(context.Background(), messaging.SyncOptions{
		Since:      "s_initial",
		Timeout:    30000,
		SetTimeout: true,
	})
	if err != nil {
		t.Fatalf("incremental Sync: %v", err)
	}
	if response.NextBatch != "s_after_message" {
		t.Errorf("incremental NextBatch = %q, want s_after_message", response.NextBatch)
	}

	configRoomID, err := ref.ParseRoomID("!config:test")
	if err != nil {
		t.Fatalf("parse room ID: %v", err)
	}
	joined, ok := response.Rooms.Join[configRoomID]
	if !ok {
		t.Fatal("room !config:test not in sync response")
	}
	if length := len(joined.Timeline.Events); length != 1 {
		t.Fatalf("expected 1 timeline event, got %d", length)
	}

	event := joined.Timeline.Events[0]
	if event.Type != "m.room.message" {
		t.Errorf("event type = %q, want m.room.message", event.Type)
	}
	if event.Sender != "@admin:test.local" {
		t.Errorf("event sender = %q, want @admin:test.local", event.Sender)
	}
	body, _ := event.Content["body"].(string)
	if body != "hello agent" {
		t.Errorf("event body = %q, want hello agent", body)
	}
}

func TestSyncError(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/matrix/sync", func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(writer, `{"errcode":"M_UNKNOWN_TOKEN","error":"Invalid access token"}`)
	})

	client := testServer(t, mux)
	_, err := client.Sync(context.Background(), messaging.SyncOptions{Timeout: 0, SetTimeout: true})
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
	if got := err.Error(); got == "" {
		t.Error("error message should not be empty")
	}
}

func TestHTTPClient(t *testing.T) {
	t.Parallel()

	client := New("/tmp/nonexistent.sock", ref.MustParseServerName("test.local"))
	httpClient := client.HTTPClient()
	if httpClient == nil {
		t.Fatal("HTTPClient returned nil")
	}
}

func TestServerName(t *testing.T) {
	t.Parallel()

	client := New("/tmp/nonexistent.sock", ref.MustParseServerName("bureau.local"))
	if client.ServerName().String() != "bureau.local" {
		t.Errorf("ServerName = %q, want bureau.local", client.ServerName())
	}
}

func TestJoinedRooms(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/matrix/joined-rooms", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string][]string{
			"joined_rooms": {"!room1:test.local", "!room2:test.local"},
		})
	})

	client := testServer(t, mux)
	rooms, err := client.JoinedRooms(context.Background())
	if err != nil {
		t.Fatalf("JoinedRooms: %v", err)
	}
	if len(rooms) != 2 {
		t.Fatalf("got %d rooms, want 2", len(rooms))
	}
	expectedRoom := ref.MustParseRoomID("!room1:test.local")
	if rooms[0] != expectedRoom {
		t.Errorf("rooms[0] = %s, want %s", rooms[0], expectedRoom)
	}
}

func TestGetRoomState(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/matrix/room-state", func(writer http.ResponseWriter, request *http.Request) {
		room := request.URL.Query().Get("room")
		if room != "!room1:test.local" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprint(writer, `[{"type":"m.room.topic","state_key":"","sender":"@admin:test.local","content":{"topic":"test topic"}}]`)
	})

	client := testServer(t, mux)
	events, err := client.GetRoomState(context.Background(), ref.MustParseRoomID("!room1:test.local"))
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

func TestGetRoomMembers(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/matrix/room-members", func(writer http.ResponseWriter, request *http.Request) {
		room := request.URL.Query().Get("room")
		if room != "!room1:test.local" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprint(writer, `{"chunk":[{"type":"m.room.member","state_key":"@agent/test:test.local","sender":"@admin:test.local","content":{"membership":"join","displayname":"Test Agent"}}]}`)
	})

	client := testServer(t, mux)
	members, err := client.GetRoomMembers(context.Background(), ref.MustParseRoomID("!room1:test.local"))
	if err != nil {
		t.Fatalf("GetRoomMembers: %v", err)
	}
	if len(members) != 1 {
		t.Fatalf("got %d members, want 1", len(members))
	}
	if members[0].UserID != "@agent/test:test.local" {
		t.Errorf("UserID = %q, want @agent/test:test.local", members[0].UserID)
	}
	if members[0].DisplayName != "Test Agent" {
		t.Errorf("DisplayName = %q, want Test Agent", members[0].DisplayName)
	}
	if members[0].Membership != "join" {
		t.Errorf("Membership = %q, want join", members[0].Membership)
	}
}

func TestRoomMessages(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/matrix/messages", func(writer http.ResponseWriter, request *http.Request) {
		room := request.URL.Query().Get("room")
		if room != "!room1:test.local" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprint(writer, `{"start":"s1","end":"s2","chunk":[{"type":"m.room.message","sender":"@admin:test.local","content":{"msgtype":"m.text","body":"hello"}}]}`)
	})

	client := testServer(t, mux)
	response, err := client.RoomMessages(context.Background(), ref.MustParseRoomID("!room1:test.local"), messaging.RoomMessagesOptions{
		Direction: "b",
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("RoomMessages: %v", err)
	}
	if response.Start != "s1" {
		t.Errorf("Start = %q, want s1", response.Start)
	}
	if response.End != "s2" {
		t.Errorf("End = %q, want s2", response.End)
	}
	if len(response.Chunk) != 1 {
		t.Fatalf("got %d chunks, want 1", len(response.Chunk))
	}
}

func TestThreadMessages(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/matrix/thread-messages", func(writer http.ResponseWriter, request *http.Request) {
		room := request.URL.Query().Get("room")
		thread := request.URL.Query().Get("thread")
		if room != "!room1:test.local" || thread != "$root" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprint(writer, `{"chunk":[{"type":"m.room.message","sender":"@agent:test.local","content":{"msgtype":"m.text","body":"reply"}}],"next_batch":"nb1"}`)
	})

	client := testServer(t, mux)
	response, err := client.ThreadMessages(context.Background(), ref.MustParseRoomID("!room1:test.local"), "$root", messaging.ThreadMessagesOptions{
		Limit: 25,
	})
	if err != nil {
		t.Fatalf("ThreadMessages: %v", err)
	}
	if len(response.Chunk) != 1 {
		t.Fatalf("got %d chunks, want 1", len(response.Chunk))
	}
	if response.NextBatch != "nb1" {
		t.Errorf("NextBatch = %q, want nb1", response.NextBatch)
	}
}

func TestGetDisplayName(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/matrix/display-name", func(writer http.ResponseWriter, request *http.Request) {
		userID := request.URL.Query().Get("user")
		if userID != "@agent/test:test.local" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprint(writer, `{"displayname":"Test Agent"}`)
	})

	client := testServer(t, mux)
	displayName, err := client.GetDisplayName(context.Background(), ref.MustParseUserID("@agent/test:test.local"))
	if err != nil {
		t.Fatalf("GetDisplayName: %v", err)
	}
	if displayName != "Test Agent" {
		t.Errorf("DisplayName = %q, want Test Agent", displayName)
	}
}

func TestGetDisplayNameNotFound(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/matrix/display-name", func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusNotFound)
		json.NewEncoder(writer).Encode(map[string]string{"error": "not found"})
	})

	client := testServer(t, mux)
	_, err := client.GetDisplayName(context.Background(), ref.MustParseUserID("@nonexistent:test.local"))
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}

func TestCreateRoom(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/matrix/room", func(writer http.ResponseWriter, request *http.Request) {
		var body messaging.CreateRoomRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]string{"room_id": "!created:test.local"})
	})

	client := testServer(t, mux)
	response, err := client.CreateRoom(context.Background(), messaging.CreateRoomRequest{
		Name: "Test Room",
	})
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	if response.RoomID.String() != "!created:test.local" {
		t.Errorf("RoomID = %q, want !created:test.local", response.RoomID)
	}
}

func TestJoinRoom(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/matrix/join", func(writer http.ResponseWriter, request *http.Request) {
		var body struct {
			Room string `json:"room"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]string{"room_id": body.Room})
	})

	client := testServer(t, mux)
	expectedJoinID := ref.MustParseRoomID("!target:test.local")
	roomID, err := client.JoinRoom(context.Background(), expectedJoinID)
	if err != nil {
		t.Fatalf("JoinRoom: %v", err)
	}
	if roomID != expectedJoinID {
		t.Errorf("RoomID = %s, want %s", roomID, expectedJoinID)
	}
}

func TestInviteUser(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/matrix/invite", func(writer http.ResponseWriter, request *http.Request) {
		var body struct {
			Room   string `json:"room"`
			UserID string `json:"user_id"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if body.Room == "" || body.UserID == "" {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.Write([]byte("{}"))
	})

	client := testServer(t, mux)
	err := client.InviteUser(context.Background(), ref.MustParseRoomID("!room1:test.local"), ref.MustParseUserID("@other:test.local"))
	if err != nil {
		t.Fatalf("InviteUser: %v", err)
	}
}

func TestSendEvent(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/matrix/event", func(writer http.ResponseWriter, request *http.Request) {
		var body struct {
			Room      string `json:"room"`
			EventType string `json:"event_type"`
			Content   any    `json:"content"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if body.Room == "" || body.EventType == "" {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]string{"event_id": "$evt789"})
	})

	client := testServer(t, mux)
	eventID, err := client.SendEvent(context.Background(), ref.MustParseRoomID("!room1:test.local"), "m.bureau.test", map[string]string{"key": "value"})
	if err != nil {
		t.Fatalf("SendEvent: %v", err)
	}
	if eventID != "$evt789" {
		t.Errorf("EventID = %q, want $evt789", eventID)
	}
}
