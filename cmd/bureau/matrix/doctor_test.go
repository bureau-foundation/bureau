// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package matrix

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/messaging"
)

// mockBureauServer creates an httptest.Server that simulates a healthy Bureau
// Matrix homeserver: versions, whoami, aliases, room state, and power levels.
// Returns the server and the room IDs used in responses.
func mockBureauServer(t *testing.T, adminUserID string) *httptest.Server {
	t.Helper()

	emptyStateKey := ""
	systemID := "!system:local"
	machinesID := "!machines:local"
	servicesID := "!services:local"
	spaceID := "!space:local"

	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")

		path := request.URL.Path

		// Unauthenticated: versions.
		if path == "/_matrix/client/versions" {
			json.NewEncoder(writer).Encode(map[string]any{
				"versions": []string{"v1.1", "v1.2", "v1.3"},
			})
			return
		}

		// WhoAmI.
		if path == "/_matrix/client/v3/account/whoami" {
			json.NewEncoder(writer).Encode(map[string]string{
				"user_id": adminUserID,
			})
			return
		}

		// Alias resolution. The alias is URL-encoded in the path.
		// Extract it by stripping the prefix and URL-decoding.
		const aliasPrefix = "/_matrix/client/v3/directory/room/"
		if strings.HasPrefix(path, aliasPrefix) {
			aliasMap := map[string]string{
				"#bureau:local":          spaceID,
				"#bureau/system:local":   systemID,
				"#bureau/machines:local": machinesID,
				"#bureau/services:local": servicesID,
			}

			// Use RawPath if available (preserves %2F encoding), then decode.
			rawPath := request.URL.RawPath
			if rawPath == "" {
				rawPath = path
			}
			encodedAlias := strings.TrimPrefix(rawPath, aliasPrefix)
			decodedAlias, err := url.PathUnescape(encodedAlias)
			if err != nil {
				writer.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(writer).Encode(messaging.MatrixError{Code: "M_INVALID_PARAM", Message: err.Error()})
				return
			}
			if roomID, ok := aliasMap[decodedAlias]; ok {
				json.NewEncoder(writer).Encode(messaging.ResolveAliasResponse{RoomID: roomID})
				return
			}
			writer.WriteHeader(http.StatusNotFound)
			json.NewEncoder(writer).Encode(messaging.MatrixError{Code: "M_NOT_FOUND", Message: "Room alias not found"})
			return
		}

		// Room state — used for space children and power levels.
		if strings.Contains(path, "/state/m.room.power_levels/") {
			// Specific state event: power levels.
			roomID := extractRoomIDFromStatePath(path)
			json.NewEncoder(writer).Encode(powerLevelsForRoom(adminUserID, roomID, machinesID, servicesID))
			return
		}
		if strings.HasSuffix(path, "/state") && !strings.Contains(path, "/state/") {
			// Full room state. Only the space room needs m.space.child events.
			roomID := extractRoomIDFromPath(path)
			if roomID == spaceID || strings.Contains(request.URL.RawPath, "%21space%3Alocal") {
				events := []messaging.Event{
					{Type: "m.room.create", StateKey: &emptyStateKey, Content: map[string]any{"type": "m.space"}},
					{Type: "m.space.child", StateKey: &systemID, Content: map[string]any{"via": []string{"local"}}},
					{Type: "m.space.child", StateKey: &machinesID, Content: map[string]any{"via": []string{"local"}}},
					{Type: "m.space.child", StateKey: &servicesID, Content: map[string]any{"via": []string{"local"}}},
				}
				json.NewEncoder(writer).Encode(events)
				return
			}
			json.NewEncoder(writer).Encode([]messaging.Event{})
			return
		}

		t.Logf("unhandled request: %s %s (raw: %s)", request.Method, path, request.URL.RawPath)
		writer.WriteHeader(http.StatusNotFound)
		json.NewEncoder(writer).Encode(messaging.MatrixError{Code: "M_NOT_FOUND", Message: "Not found"})
	}))
}

// powerLevelsForRoom returns the expected power levels for a Bureau room.
func powerLevelsForRoom(adminUserID, roomID, machinesID, servicesID string) map[string]any {
	events := map[string]any{
		"m.room.name":        100,
		"m.room.topic":       100,
		"m.room.power_levels": 100,
		"m.space.child":      100,
	}

	switch roomID {
	case machinesID:
		events["m.bureau.machine_key"] = 0
		events["m.bureau.machine_status"] = 0
	case servicesID:
		events["m.bureau.service"] = 0
	}

	return map[string]any{
		"users":          map[string]any{adminUserID: 100},
		"users_default":  0,
		"state_default":  100,
		"events_default": 0,
		"events":         events,
	}
}

// extractRoomIDFromStatePath extracts the room ID from paths like
// /rooms/{roomId}/state/{type}/{key}. Since room IDs are URL-encoded,
// this checks a few known patterns.
func extractRoomIDFromStatePath(path string) string {
	if strings.Contains(path, "%21machines%3Alocal") || strings.Contains(path, "!machines:local") {
		return "!machines:local"
	}
	if strings.Contains(path, "%21services%3Alocal") || strings.Contains(path, "!services:local") {
		return "!services:local"
	}
	if strings.Contains(path, "%21system%3Alocal") || strings.Contains(path, "!system:local") {
		return "!system:local"
	}
	if strings.Contains(path, "%21space%3Alocal") || strings.Contains(path, "!space:local") {
		return "!space:local"
	}
	return ""
}

// extractRoomIDFromPath extracts the room ID from paths like /rooms/{roomId}/state.
func extractRoomIDFromPath(path string) string {
	return extractRoomIDFromStatePath(path)
}

func TestRunDoctor_AllHealthy(t *testing.T) {
	adminUserID := "@bureau-admin:local"
	server := mockBureauServer(t, adminUserID)
	defer server.Close()

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: server.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session, err := client.SessionFromToken(adminUserID, "test-token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	defer session.Close()

	results := runDoctor(t.Context(), client, session, "local", nil)

	for _, result := range results {
		if result.Status == statusFail {
			t.Errorf("[FAIL] %s: %s", result.Name, result.Message)
		}
		if result.Status == statusWarn {
			t.Logf("[WARN] %s: %s", result.Name, result.Message)
		}
	}

	// Verify we got checks for all expected sections.
	names := make(map[string]bool)
	for _, result := range results {
		names[result.Name] = true
	}

	expectedChecks := []string{
		"homeserver",
		"authentication",
		"bureau space",
		"system room",
		"machines room",
		"services room",
		"system room in space",
		"machines room in space",
		"services room in space",
		"bureau space admin power",
		"bureau space state_default",
		"machines room admin power",
		"machines room state_default",
		"machines room m.bureau.machine_key",
		"machines room m.bureau.machine_status",
		"services room admin power",
		"services room state_default",
		"services room m.bureau.service",
	}
	for _, expected := range expectedChecks {
		if !names[expected] {
			t.Errorf("missing check: %q", expected)
		}
	}
}

func TestRunDoctor_WithCredentials(t *testing.T) {
	adminUserID := "@bureau-admin:local"
	server := mockBureauServer(t, adminUserID)
	defer server.Close()

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: server.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session, err := client.SessionFromToken(adminUserID, "test-token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	defer session.Close()

	// Provide matching credentials.
	credentials := map[string]string{
		"MATRIX_SPACE_ROOM":    "!space:local",
		"MATRIX_SYSTEM_ROOM":   "!system:local",
		"MATRIX_MACHINES_ROOM": "!machines:local",
		"MATRIX_SERVICES_ROOM": "!services:local",
	}

	results := runDoctor(t.Context(), client, session, "local", credentials)

	// All credential checks should pass.
	for _, result := range results {
		if strings.HasSuffix(result.Name, " credential") && result.Status != statusPass {
			t.Errorf("[%s] %s: %s", result.Status, result.Name, result.Message)
		}
	}
}

func TestRunDoctor_StaleCredentials(t *testing.T) {
	adminUserID := "@bureau-admin:local"
	server := mockBureauServer(t, adminUserID)
	defer server.Close()

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: server.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session, err := client.SessionFromToken(adminUserID, "test-token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	defer session.Close()

	// Provide mismatched credentials.
	credentials := map[string]string{
		"MATRIX_SPACE_ROOM":    "!space:local",
		"MATRIX_SYSTEM_ROOM":   "!wrong:local", // mismatch
		"MATRIX_MACHINES_ROOM": "!machines:local",
		"MATRIX_SERVICES_ROOM": "!services:local",
	}

	results := runDoctor(t.Context(), client, session, "local", credentials)

	// The system room credential check should fail.
	found := false
	for _, result := range results {
		if result.Name == "system room credential" {
			found = true
			if result.Status != statusFail {
				t.Errorf("expected system room credential to FAIL, got %s: %s", result.Status, result.Message)
			}
			if !strings.Contains(result.Message, "stale") {
				t.Errorf("expected 'stale' in message, got: %s", result.Message)
			}
		}
	}
	if !found {
		t.Error("system room credential check not found")
	}
}

func TestRunDoctor_HomeserverUnreachable(t *testing.T) {
	// Point at a server that immediately closes connections.
	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session, err := client.SessionFromToken("@admin:local", "test-token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	defer session.Close()

	results := runDoctor(t.Context(), client, session, "local", nil)

	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}

	// First check should fail.
	if results[0].Name != "homeserver" || results[0].Status != statusFail {
		t.Errorf("expected homeserver FAIL, got %s %s", results[0].Name, results[0].Status)
	}

	// All subsequent checks should be skipped.
	for _, result := range results[1:] {
		if result.Status != statusSkip {
			t.Errorf("expected %s to be skipped after homeserver failure, got %s", result.Name, result.Status)
		}
	}
}

func TestRunDoctor_AuthFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")

		if request.URL.Path == "/_matrix/client/versions" {
			json.NewEncoder(writer).Encode(map[string]any{
				"versions": []string{"v1.1"},
			})
			return
		}
		if request.URL.Path == "/_matrix/client/v3/account/whoami" {
			writer.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(writer).Encode(messaging.MatrixError{Code: "M_UNKNOWN_TOKEN", Message: "Invalid token"})
			return
		}
	}))
	defer server.Close()

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: server.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session, err := client.SessionFromToken("@admin:local", "bad-token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	defer session.Close()

	results := runDoctor(t.Context(), client, session, "local", nil)

	// Homeserver should pass.
	if results[0].Status != statusPass {
		t.Errorf("expected homeserver PASS, got %s: %s", results[0].Status, results[0].Message)
	}

	// Auth should fail.
	if results[1].Name != "authentication" || results[1].Status != statusFail {
		t.Errorf("expected authentication FAIL, got %s %s: %s", results[1].Name, results[1].Status, results[1].Message)
	}
	if !strings.Contains(results[1].Message, "invalid or expired") {
		t.Errorf("expected 'invalid or expired' in message, got: %s", results[1].Message)
	}

	// All subsequent checks should be skipped.
	for _, result := range results[2:] {
		if result.Status != statusSkip {
			t.Errorf("expected %s to be skipped after auth failure, got %s", result.Name, result.Status)
		}
	}
}

func TestCheckServerVersions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]any{
			"versions": []string{"v1.1", "v1.5"},
		})
	}))
	defer server.Close()

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: server.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	result := checkServerVersions(t.Context(), client)
	if result.Status != statusPass {
		t.Errorf("expected PASS, got %s: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "v1.1") || !strings.Contains(result.Message, "v1.5") {
		t.Errorf("expected version info in message, got: %s", result.Message)
	}
}

func TestCheckAuth_Mismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]string{"user_id": "@different:local"})
	}))
	defer server.Close()

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: server.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session, err := client.SessionFromToken("@expected:local", "token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	defer session.Close()

	result := checkAuth(t.Context(), session)
	if result.Status != statusWarn {
		t.Errorf("expected WARN for user ID mismatch, got %s: %s", result.Status, result.Message)
	}
}

func TestGetUserPowerLevel(t *testing.T) {
	powerLevels := map[string]any{
		"users":         map[string]any{"@admin:local": float64(100), "@mod:local": float64(50)},
		"users_default": float64(0),
	}

	tests := []struct {
		userID   string
		expected float64
	}{
		{"@admin:local", 100},
		{"@mod:local", 50},
		{"@nobody:local", 0}, // falls back to users_default
	}

	for _, test := range tests {
		level := getUserPowerLevel(powerLevels, test.userID)
		if level != test.expected {
			t.Errorf("getUserPowerLevel(%q) = %v, want %v", test.userID, level, test.expected)
		}
	}
}

func TestGetEventPowerLevel(t *testing.T) {
	powerLevels := map[string]any{
		"events":         map[string]any{"m.room.name": float64(100), "m.bureau.machine_key": float64(0)},
		"events_default": float64(50),
	}

	tests := []struct {
		eventType string
		expected  float64
	}{
		{"m.room.name", 100},
		{"m.bureau.machine_key", 0},
		{"m.unknown.type", 50}, // falls back to events_default
	}

	for _, test := range tests {
		level := getEventPowerLevel(powerLevels, test.eventType)
		if level != test.expected {
			t.Errorf("getEventPowerLevel(%q) = %v, want %v", test.eventType, level, test.expected)
		}
	}
}

func TestCheckSpaceChild(t *testing.T) {
	children := map[string]bool{
		"!a:local": true,
		"!b:local": true,
	}

	result := checkSpaceChild("room A", "!a:local", children)
	if result.Status != statusPass {
		t.Errorf("expected PASS for known child, got %s", result.Status)
	}

	result = checkSpaceChild("room C", "!c:local", children)
	if result.Status != statusFail {
		t.Errorf("expected FAIL for missing child, got %s", result.Status)
	}
}

func TestCheckCredentialMatch(t *testing.T) {
	creds := map[string]string{
		"MATRIX_SYSTEM_ROOM": "!system:local",
	}

	result := checkCredentialMatch("system room", "MATRIX_SYSTEM_ROOM", "!system:local", creds)
	if result.Status != statusPass {
		t.Errorf("expected PASS for matching credential, got %s: %s", result.Status, result.Message)
	}

	result = checkCredentialMatch("system room", "MATRIX_SYSTEM_ROOM", "!different:local", creds)
	if result.Status != statusFail {
		t.Errorf("expected FAIL for mismatched credential, got %s: %s", result.Status, result.Message)
	}

	result = checkCredentialMatch("system room", "MATRIX_MISSING_KEY", "!system:local", creds)
	if result.Status != statusWarn {
		t.Errorf("expected WARN for missing key, got %s: %s", result.Status, result.Message)
	}
}

func TestDoctorCommand_UnexpectedArg(t *testing.T) {
	command := DoctorCommand()
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

func TestResolveHomeserverURL(t *testing.T) {
	t.Run("from flag", func(t *testing.T) {
		config := SessionConfig{HomeserverURL: "http://localhost:6167"}
		url, err := config.resolveHomeserverURL()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if url != "http://localhost:6167" {
			t.Errorf("expected http://localhost:6167, got %s", url)
		}
	})

	t.Run("missing both", func(t *testing.T) {
		config := SessionConfig{}
		_, err := config.resolveHomeserverURL()
		if err == nil {
			t.Fatal("expected error for missing homeserver and credential file")
		}
	})
}

func TestExitError(t *testing.T) {
	err := &exitError{code: 42}
	if err.Error() != "exit code 42" {
		t.Errorf("unexpected error string: %s", err.Error())
	}
	if err.ExitCode() != 42 {
		t.Errorf("expected exit code 42, got %d", err.ExitCode())
	}
}
