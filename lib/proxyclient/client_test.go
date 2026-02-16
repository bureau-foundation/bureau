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

	"github.com/bureau-foundation/bureau/lib/schema"
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
		serverName: "test.local",
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
			{Actions: []string{"matrix/send"}, Targets: []string{"bureau/config/*"}},
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
	if userID != "@agent/test:test.local" {
		t.Errorf("UserID = %q, want @agent/test:test.local", userID)
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
	if serverName != "bureau.example.com" {
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
	if client.ServerName() != "test.local" {
		t.Errorf("ServerName before discover = %q, want test.local", client.ServerName())
	}

	serverName, err := client.DiscoverServerName(context.Background())
	if err != nil {
		t.Fatalf("DiscoverServerName: %v", err)
	}
	if serverName != "discovered.local" {
		t.Errorf("returned ServerName = %q, want discovered.local", serverName)
	}
	if client.ServerName() != "discovered.local" {
		t.Errorf("ServerName after discover = %q, want discovered.local", client.ServerName())
	}
}

func TestResolveAlias(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/matrix/resolve", func(writer http.ResponseWriter, request *http.Request) {
		alias := request.URL.Query().Get("alias")
		if alias != "#bureau/config/machine/ws:test.local" {
			writer.WriteHeader(http.StatusNotFound)
			json.NewEncoder(writer).Encode(map[string]string{"error": "not found"})
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]string{"room_id": "!config:test"})
	})

	client := testServer(t, mux)

	roomID, err := client.ResolveAlias(context.Background(), "#bureau/config/machine/ws:test.local")
	if err != nil {
		t.Fatalf("ResolveAlias: %v", err)
	}
	if roomID != "!config:test" {
		t.Errorf("RoomID = %q, want !config:test", roomID)
	}

	_, err = client.ResolveAlias(context.Background(), "#nonexistent:test.local")
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
	content, err := client.GetState(context.Background(), "!room:test", "m.bureau.template", "base")
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
		if body.Room != "!room:test" || body.EventType != "m.bureau.test" {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]string{"event_id": "$event123"})
	})

	client := testServer(t, mux)
	eventID, err := client.PutState(context.Background(), PutStateRequest{
		Room:      "!room:test",
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
	eventID, err := client.SendTextMessage(context.Background(), "!room:test", "hello")
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

	// Mock the Matrix /sync endpoint via the proxy passthrough.
	mux.HandleFunc("GET /http/matrix/_matrix/client/v3/sync", func(writer http.ResponseWriter, request *http.Request) {
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
	response, err := client.Sync(context.Background(), SyncOptions{Timeout: 0})
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
	response, err = client.Sync(context.Background(), SyncOptions{
		Since:   "s_initial",
		Timeout: 30000,
	})
	if err != nil {
		t.Fatalf("incremental Sync: %v", err)
	}
	if response.NextBatch != "s_after_message" {
		t.Errorf("incremental NextBatch = %q, want s_after_message", response.NextBatch)
	}

	joined, ok := response.Rooms.Join["!config:test"]
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
	mux.HandleFunc("GET /http/matrix/_matrix/client/v3/sync", func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(writer, `{"errcode":"M_UNKNOWN_TOKEN","error":"Invalid access token"}`)
	})

	client := testServer(t, mux)
	_, err := client.Sync(context.Background(), SyncOptions{Timeout: 0})
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
	if got := err.Error(); got == "" {
		t.Error("error message should not be empty")
	}
}

func TestHTTPClient(t *testing.T) {
	t.Parallel()

	client := New("/tmp/nonexistent.sock", "test.local")
	httpClient := client.HTTPClient()
	if httpClient == nil {
		t.Fatal("HTTPClient returned nil")
	}
}

func TestServerName(t *testing.T) {
	t.Parallel()

	client := New("/tmp/nonexistent.sock", "bureau.local")
	if client.ServerName() != "bureau.local" {
		t.Errorf("ServerName = %q, want bureau.local", client.ServerName())
	}
}
