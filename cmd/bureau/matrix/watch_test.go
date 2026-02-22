// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package matrix

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/messaging"
)

func TestWatchCommand_MissingRoom(t *testing.T) {
	command := WatchCommand()
	err := command.Execute([]string{
		"--homeserver", "http://localhost:6167", "--token", "tok", "--user-id", "@a:b",
	})
	if err == nil {
		t.Fatal("expected error for missing room")
	}
	if !strings.Contains(err.Error(), "room is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestWatchCommand_TooManyArgs(t *testing.T) {
	command := WatchCommand()
	err := command.Execute([]string{
		"--homeserver", "http://localhost:6167", "--token", "tok", "--user-id", "@a:b",
		"!room:local", "extra",
	})
	if err == nil {
		t.Fatal("expected error for extra arg")
	}
	if !strings.Contains(err.Error(), "unexpected argument") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRunWatchLoop_ReceivesEvents(t *testing.T) {
	var syncCount atomic.Int32
	roomID := mustRoomID("!testroom:local")

	// cancel is called by the mock server after it delivers events,
	// signaling the watch loop to exit deterministically.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")

		if !strings.Contains(request.URL.Path, "/sync") {
			http.NotFound(writer, request)
			return
		}

		count := syncCount.Add(1)
		switch {
		case count == 1:
			// Initial sync: return next_batch token.
			json.NewEncoder(writer).Encode(messaging.SyncResponse{
				NextBatch: "batch_1",
			})
		case count == 2:
			// Incremental sync: return one event, then cancel context
			// so the loop exits after processing this batch.
			json.NewEncoder(writer).Encode(messaging.SyncResponse{
				NextBatch: "batch_2",
				Rooms: messaging.RoomsSection{
					Join: map[ref.RoomID]messaging.JoinedRoom{
						roomID: {
							Timeline: messaging.TimelineSection{
								Events: []messaging.Event{
									{
										EventID:        ref.MustParseEventID("$ev1"),
										Type:           "m.room.message",
										Sender:         ref.MustParseUserID("@user:local"),
										OriginServerTS: 1771200000000,
										Content:        map[string]any{"msgtype": "m.text", "body": "hello"},
									},
								},
							},
						},
					},
				},
			})
			cancel()
		default:
			// Block until the client cancels the request.
			<-request.Context().Done()
		}
	}))
	defer server.Close()

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: server.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session, err := client.SessionFromToken(mustParseUserID(t, "@test:local"), "tok")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}

	params := &watchParams{
		ShowState: false,
	}

	if err = runWatchLoop(ctx, session, roomID, params); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if syncCount.Load() < 2 {
		t.Errorf("expected at least 2 sync calls, got %d", syncCount.Load())
	}
}

func TestRunWatchLoop_TypeFiltering(t *testing.T) {
	var syncCount atomic.Int32
	roomID := mustRoomID("!testroom:local")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")

		if !strings.Contains(request.URL.Path, "/sync") {
			http.NotFound(writer, request)
			return
		}

		count := syncCount.Add(1)
		switch {
		case count == 1:
			json.NewEncoder(writer).Encode(messaging.SyncResponse{
				NextBatch: "batch_1",
			})
		case count == 2:
			json.NewEncoder(writer).Encode(messaging.SyncResponse{
				NextBatch: "batch_2",
				Rooms: messaging.RoomsSection{
					Join: map[ref.RoomID]messaging.JoinedRoom{
						roomID: {
							Timeline: messaging.TimelineSection{
								Events: []messaging.Event{
									{
										EventID:        ref.MustParseEventID("$ev1"),
										Type:           "m.room.message",
										Sender:         ref.MustParseUserID("@user:local"),
										OriginServerTS: 1771200000000,
										Content:        map[string]any{"msgtype": "m.text", "body": "should be filtered out"},
									},
									{
										EventID:        ref.MustParseEventID("$ev2"),
										Type:           "m.bureau.machine_status",
										Sender:         ref.MustParseUserID("@machine:local"),
										OriginServerTS: 1771200001000,
										Content:        map[string]any{"cpu_percent": 42.5},
									},
								},
							},
						},
					},
				},
			})
			cancel()
		default:
			<-request.Context().Done()
		}
	}))
	defer server.Close()

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: server.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session, err := client.SessionFromToken(mustParseUserID(t, "@test:local"), "tok")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}

	params := &watchParams{
		EventTypes: []string{"m.bureau.*"},
	}

	if err = runWatchLoop(ctx, session, roomID, params); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Filtering correctness is validated by matchesTypeFilter unit tests
	// in format_test.go. This test verifies the filtering code path in
	// the sync loop doesn't panic or produce errors.
}

func TestRunWatchLoop_HistoryFetch(t *testing.T) {
	var gotMessagesRequest atomic.Bool
	var syncCount atomic.Int32
	roomID := mustRoomID("!testroom:local")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")

		switch {
		case strings.Contains(request.URL.Path, "/messages"):
			gotMessagesRequest.Store(true)
			json.NewEncoder(writer).Encode(map[string]any{
				"start": "s1",
				"end":   "s2",
				"chunk": []map[string]any{
					{
						"event_id":         "$hist1",
						"type":             "m.room.message",
						"sender":           "@user:local",
						"origin_server_ts": 1771200000000,
						"content":          map[string]any{"msgtype": "m.text", "body": "history msg"},
					},
				},
			})
		case strings.Contains(request.URL.Path, "/sync"):
			count := syncCount.Add(1)
			if count == 1 {
				json.NewEncoder(writer).Encode(messaging.SyncResponse{NextBatch: "batch_1"})
				cancel()
			} else {
				<-request.Context().Done()
			}
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: server.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session, err := client.SessionFromToken(mustParseUserID(t, "@test:local"), "tok")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}

	params := &watchParams{
		History: 5,
	}

	if err = runWatchLoop(ctx, session, roomID, params); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !gotMessagesRequest.Load() {
		t.Error("expected /messages request for history fetch")
	}
}
