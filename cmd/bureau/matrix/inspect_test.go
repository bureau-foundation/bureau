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

func TestInspectCommand_MissingRoom(t *testing.T) {
	command := InspectCommand()
	err := command.Execute([]string{
		"--homeserver", "http://localhost:6167", "--token", "tok", "--user-id", "@a:b",
	})
	if err == nil {
		t.Fatal("expected error for missing room arg")
	}
	if !strings.Contains(err.Error(), "room is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestInspectCommand_TooManyArgs(t *testing.T) {
	command := InspectCommand()
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

func TestInspectCommand_Success(t *testing.T) {
	stateEvents := []map[string]any{
		{
			"type":      "m.room.create",
			"sender":    "@admin:local",
			"state_key": "",
			"content":   map[string]any{"room_version": "11", "creator": "@admin:local"},
		},
		{
			"type":      "m.room.name",
			"sender":    "@admin:local",
			"state_key": "",
			"content":   map[string]any{"name": "Test Room"},
		},
		{
			"type":      "m.room.canonical_alias",
			"sender":    "@admin:local",
			"state_key": "",
			"content":   map[string]any{"alias": "#test:local"},
		},
		{
			"type":      "m.room.topic",
			"sender":    "@admin:local",
			"state_key": "",
			"content":   map[string]any{"topic": "A test room"},
		},
		{
			"type":      "m.room.power_levels",
			"sender":    "@admin:local",
			"state_key": "",
			"content": map[string]any{
				"events_default": float64(0),
				"state_default":  float64(100),
				"ban":            float64(100),
				"kick":           float64(100),
				"invite":         float64(50),
				"redact":         float64(50),
				"users": map[string]any{
					"@admin:local": float64(100),
				},
				"events": map[string]any{
					"m.bureau.machine_key": float64(0),
				},
			},
		},
		{
			"type":      "m.bureau.machine_key",
			"sender":    "@machine/work:local",
			"state_key": "machine/work",
			"content":   map[string]any{"public_key": "age1abc"},
		},
		{
			"type":      "m.room.join_rules",
			"sender":    "@admin:local",
			"state_key": "",
			"content":   map[string]any{"join_rule": "invite"},
		},
	}

	membersResponse := map[string]any{
		"chunk": []map[string]any{
			{
				"type":      "m.room.member",
				"state_key": "@admin:local",
				"sender":    "@admin:local",
				"content":   map[string]any{"membership": "join", "displayname": "Admin"},
			},
			{
				"type":      "m.room.member",
				"state_key": "@machine/work:local",
				"sender":    "@machine/work:local",
				"content":   map[string]any{"membership": "join", "displayname": "workstation"},
			},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")

		switch {
		case strings.Contains(request.URL.Path, "/state"):
			json.NewEncoder(writer).Encode(stateEvents)
		case strings.Contains(request.URL.Path, "/members"):
			json.NewEncoder(writer).Encode(membersResponse)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	command := InspectCommand()
	err := command.Execute([]string{
		"--homeserver", server.URL, "--token", "tok", "--user-id", "@admin:local",
		"!room:local",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParsePowerLevels(t *testing.T) {
	raw := map[string]any{
		"events_default": float64(0),
		"state_default":  float64(100),
		"ban":            float64(100),
		"kick":           float64(50),
		"invite":         float64(50),
		"redact":         float64(50),
		"users": map[string]any{
			"@admin:local": float64(100),
			"@user:local":  float64(50),
		},
		"events": map[string]any{
			"m.bureau.machine_key": float64(0),
			"m.room.topic":         float64(50),
		},
	}

	pl := parsePowerLevels(raw)

	if pl.EventsDefault != 0 {
		t.Errorf("events_default: got %d, want 0", pl.EventsDefault)
	}
	if pl.StateDefault != 100 {
		t.Errorf("state_default: got %d, want 100", pl.StateDefault)
	}
	if pl.Ban != 100 {
		t.Errorf("ban: got %d, want 100", pl.Ban)
	}
	if pl.Kick != 50 {
		t.Errorf("kick: got %d, want 50", pl.Kick)
	}
	if len(pl.Users) != 2 {
		t.Errorf("users count: got %d, want 2", len(pl.Users))
	}
	if pl.Users["@admin:local"] != 100 {
		t.Errorf("admin power level: got %d, want 100", pl.Users["@admin:local"])
	}
	if len(pl.Events) != 2 {
		t.Errorf("events count: got %d, want 2", len(pl.Events))
	}
	if pl.Events["m.bureau.machine_key"] != 0 {
		t.Errorf("machine_key event level: got %d, want 0", pl.Events["m.bureau.machine_key"])
	}
}

func TestBuildStateGroups_Filtering(t *testing.T) {
	groups := map[string][]inspectStateEntry{
		"m.bureau.machine_key": {
			{StateKey: "machine/a", Sender: "@a:local", Content: map[string]any{"key": "val"}},
		},
		"m.bureau.service": {
			{StateKey: "svc/a", Sender: "@a:local", Content: map[string]any{"port": float64(8080)}},
		},
	}

	// No filter: both groups returned.
	result := buildStateGroups(groups, nil)
	if len(result) != 2 {
		t.Errorf("no filter: got %d groups, want 2", len(result))
	}

	// Exact filter.
	result = buildStateGroups(groups, []string{"m.bureau.service"})
	if len(result) != 1 {
		t.Fatalf("exact filter: got %d groups, want 1", len(result))
	}
	if result[0].EventType != "m.bureau.service" {
		t.Errorf("exact filter: got type %s, want m.bureau.service", result[0].EventType)
	}

	// Prefix filter.
	result = buildStateGroups(groups, []string{"m.bureau.*"})
	if len(result) != 2 {
		t.Errorf("prefix filter: got %d groups, want 2", len(result))
	}

	// Non-matching filter.
	result = buildStateGroups(groups, []string{"m.room.topic"})
	if len(result) != 0 {
		t.Errorf("non-matching filter: got %d groups, want 0", len(result))
	}
}

func TestBuildStateGroups_Sorting(t *testing.T) {
	groups := map[string][]inspectStateEntry{
		"m.bureau.service": {
			{StateKey: "svc/b", Sender: "@b:local"},
			{StateKey: "svc/a", Sender: "@a:local"},
		},
		"m.bureau.machine_key": {
			{StateKey: "machine/z", Sender: "@z:local"},
			{StateKey: "machine/a", Sender: "@a:local"},
		},
	}

	result := buildStateGroups(groups, nil)

	// Groups should be sorted by event type.
	if result[0].EventType != "m.bureau.machine_key" {
		t.Errorf("first group should be machine_key, got %s", result[0].EventType)
	}
	if result[1].EventType != "m.bureau.service" {
		t.Errorf("second group should be service, got %s", result[1].EventType)
	}

	// Entries within groups should be sorted by state key.
	if result[0].Events[0].StateKey != "machine/a" {
		t.Errorf("first entry should be machine/a, got %s", result[0].Events[0].StateKey)
	}
	if result[1].Events[0].StateKey != "svc/a" {
		t.Errorf("first entry should be svc/a, got %s", result[1].Events[0].StateKey)
	}
}

func TestPowerLevelLabel(t *testing.T) {
	tests := []struct {
		level    int
		expected string
	}{
		{0, "any member"},
		{-1, "any member"},
		{25, "operator"},
		{50, "operator"},
		{51, "admin"},
		{100, "admin"},
	}

	for _, test := range tests {
		result := powerLevelLabel(test.level)
		if result != test.expected {
			t.Errorf("powerLevelLabel(%d): got %q, want %q", test.level, result, test.expected)
		}
	}
}
