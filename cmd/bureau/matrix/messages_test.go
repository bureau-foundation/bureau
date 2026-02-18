// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package matrix

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMessagesCommand_MissingRoom(t *testing.T) {
	command := MessagesCommand()
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

func TestMessagesCommand_TooManyArgs(t *testing.T) {
	command := MessagesCommand()
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

func TestMessagesCommand_InvalidDirection(t *testing.T) {
	command := MessagesCommand()
	err := command.Execute([]string{
		"--homeserver", "http://localhost:6167", "--token", "tok", "--user-id", "@a:b",
		"--direction", "sideways",
		"!room:local",
	})
	if err == nil {
		t.Fatal("expected error for invalid direction")
	}
	if !strings.Contains(err.Error(), "invalid direction") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestParseDirection(t *testing.T) {
	tests := []struct {
		input    string
		expected string
		wantErr  bool
	}{
		{"backward", "b", false},
		{"b", "b", false},
		{"forward", "f", false},
		{"f", "f", false},
		{"sideways", "", true},
		{"", "", true},
	}

	for _, test := range tests {
		result, err := parseDirection(test.input)
		if test.wantErr {
			if err == nil {
				t.Errorf("parseDirection(%q): expected error", test.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseDirection(%q): unexpected error: %v", test.input, err)
			continue
		}
		if result != test.expected {
			t.Errorf("parseDirection(%q): got %q, want %q", test.input, result, test.expected)
		}
	}
}

func TestMessagesCommand_RoomMessages(t *testing.T) {
	var capturedDirection string
	var capturedLimit string

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")

		if strings.Contains(request.URL.Path, "/messages") {
			capturedDirection = request.URL.Query().Get("dir")
			capturedLimit = request.URL.Query().Get("limit")

			json.NewEncoder(writer).Encode(map[string]any{
				"start": "s123",
				"end":   "s456",
				"chunk": []map[string]any{
					{
						"event_id":         "$ev1",
						"type":             "m.room.message",
						"sender":           "@user:local",
						"origin_server_ts": 1771200000000,
						"content":          map[string]any{"msgtype": "m.text", "body": "hello"},
					},
				},
			})
			return
		}
		http.NotFound(writer, request)
	}))
	defer server.Close()

	command := MessagesCommand()
	err := command.Execute([]string{
		"--homeserver", server.URL, "--token", "tok", "--user-id", "@a:b",
		"--limit", "10",
		"!room:local",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedDirection != "b" {
		t.Errorf("expected direction 'b', got %q", capturedDirection)
	}
	if capturedLimit != "10" {
		t.Errorf("expected limit '10', got %q", capturedLimit)
	}
}

func TestMessagesCommand_ThreadMessages(t *testing.T) {
	var gotRelationsPath bool

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")

		if strings.Contains(request.URL.Path, "/relations/") {
			gotRelationsPath = true
			json.NewEncoder(writer).Encode(map[string]any{
				"chunk": []map[string]any{
					{
						"event_id":         "$reply1",
						"type":             "m.room.message",
						"sender":           "@user:local",
						"origin_server_ts": 1771200000000,
						"content":          map[string]any{"msgtype": "m.text", "body": "thread reply"},
					},
				},
				"next_batch": "nb_123",
			})
			return
		}
		http.NotFound(writer, request)
	}))
	defer server.Close()

	command := MessagesCommand()
	err := command.Execute([]string{
		"--homeserver", server.URL, "--token", "tok", "--user-id", "@a:b",
		"--thread", "$root_event",
		"!room:local",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !gotRelationsPath {
		t.Error("expected request to /relations/ endpoint for thread messages")
	}
}
