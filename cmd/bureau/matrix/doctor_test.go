// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package matrix

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/content"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// testLogger returns a logger that discards all output.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// mockDoctorServer is a configurable Matrix homeserver mock for doctor tests.
// Default (nil) field values represent a fully healthy Bureau deployment.
// Override specific fields to simulate broken states.
type mockDoctorServer struct {
	adminUserID string

	// Room IDs.
	spaceID    string
	systemID   string
	templateID string
	pipelineID string
	artifactID string

	// Configurable state. Nil means healthy defaults.
	spaceChildren map[string]bool           // room IDs that are space children; nil = all standard rooms
	joinRules     map[string]string         // roomID -> join_rule; nil = "invite" for all
	powerLevels   map[string]map[string]any // roomID -> PL content; nil = correct defaults
	roomMembers   map[string][]string       // roomID -> joined member user IDs; nil = admin only

	// Mutation tracking.
	mu             sync.Mutex
	invitesSent    map[string][]string // roomID -> []userID
	stateEventsSet []stateEventRecord
}

type stateEventRecord struct {
	roomID    string
	eventType string
	stateKey  string
}

func newHealthyMock(adminUserID string) *mockDoctorServer {
	return &mockDoctorServer{
		adminUserID: adminUserID,
		spaceID:     "!space:local",
		systemID:    "!system:local",
		templateID:  "!template:local",
		pipelineID:  "!pipeline:local",
		artifactID:  "!artifact:local",
		invitesSent: make(map[string][]string),
	}
}

func (m *mockDoctorServer) httpServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(m.handle(t)))
}

func (m *mockDoctorServer) getInvites(roomID string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string{}, m.invitesSent[roomID]...)
}

func (m *mockDoctorServer) getStateEvents() []stateEventRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]stateEventRecord{}, m.stateEventsSet...)
}

func (m *mockDoctorServer) handle(t *testing.T) http.HandlerFunc {
	emptyStateKey := ""

	return func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")

		path := request.URL.Path
		method := request.Method
		rawPath := request.URL.RawPath
		if rawPath == "" {
			rawPath = path
		}

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
				"user_id": m.adminUserID,
			})
			return
		}

		// Alias resolution.
		const aliasPrefix = "/_matrix/client/v3/directory/room/"
		if strings.HasPrefix(path, aliasPrefix) {
			aliasMap := map[string]string{
				"#bureau:local":          m.spaceID,
				"#bureau/system:local":   m.systemID,
				"#bureau/template:local": m.templateID,
				"#bureau/pipeline:local": m.pipelineID,
				"#bureau/artifact:local": m.artifactID,
			}

			encodedAlias := strings.TrimPrefix(rawPath, aliasPrefix)
			decodedAlias, err := url.PathUnescape(encodedAlias)
			if err != nil {
				writer.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(writer).Encode(messaging.MatrixError{Code: "M_INVALID_PARAM", Message: err.Error()})
				return
			}
			if roomID, ok := aliasMap[decodedAlias]; ok {
				json.NewEncoder(writer).Encode(messaging.ResolveAliasResponse{RoomID: mustRoomID(roomID)})
				return
			}
			writer.WriteHeader(http.StatusNotFound)
			json.NewEncoder(writer).Encode(messaging.MatrixError{Code: "M_NOT_FOUND", Message: "Room alias not found"})
			return
		}

		// PUT state events — track mutations.
		if method == http.MethodPut && strings.Contains(rawPath, "/state/") {
			roomID := extractRoomIDFromStatePath(rawPath)
			idx := strings.Index(rawPath, "/state/")
			rest := rawPath[idx+len("/state/"):]
			eventType, stateKey, _ := strings.Cut(rest, "/")
			eventType, _ = url.PathUnescape(eventType)
			stateKey, _ = url.PathUnescape(stateKey)

			m.mu.Lock()
			m.stateEventsSet = append(m.stateEventsSet, stateEventRecord{
				roomID:    roomID,
				eventType: eventType,
				stateKey:  stateKey,
			})
			m.mu.Unlock()
			json.NewEncoder(writer).Encode(messaging.SendEventResponse{EventID: "$fix"})
			return
		}

		// POST invite — track mutations.
		if method == http.MethodPost && strings.Contains(path, "/invite") {
			var inviteRequest messaging.InviteRequest
			json.NewDecoder(request.Body).Decode(&inviteRequest)
			roomID := extractRoomIDFromPath(rawPath)

			m.mu.Lock()
			m.invitesSent[roomID] = append(m.invitesSent[roomID], inviteRequest.UserID)
			m.mu.Unlock()
			json.NewEncoder(writer).Encode(map[string]any{})
			return
		}

		// GET specific state event: join rules.
		if method == http.MethodGet && strings.Contains(rawPath, "/state/m.room.join_rules") {
			roomID := extractRoomIDFromStatePath(rawPath)
			joinRule := "invite"
			if m.joinRules != nil {
				if rule, ok := m.joinRules[roomID]; ok {
					joinRule = rule
				}
			}
			json.NewEncoder(writer).Encode(map[string]any{"join_rule": joinRule})
			return
		}

		// GET specific state event: power levels.
		if method == http.MethodGet && strings.Contains(rawPath, "/state/m.room.power_levels") {
			roomID := extractRoomIDFromStatePath(rawPath)
			if m.powerLevels != nil {
				if content, ok := m.powerLevels[roomID]; ok {
					json.NewEncoder(writer).Encode(content)
					return
				}
			}
			json.NewEncoder(writer).Encode(powerLevelsForRoom(m.adminUserID, roomID, m.systemID, m.pipelineID, m.artifactID))
			return
		}

		// GET specific state event: m.bureau.template (template room).
		if method == http.MethodGet && strings.Contains(rawPath, "/state/m.bureau.template") {
			roomID := extractRoomIDFromStatePath(rawPath)
			if roomID == m.templateID {
				// Extract the state key (template name) from the path.
				idx := strings.Index(rawPath, "/state/m.bureau.template/")
				if idx >= 0 {
					stateKey := rawPath[idx+len("/state/m.bureau.template/"):]
					stateKey, _ = url.PathUnescape(stateKey)
					for _, template := range baseTemplates() {
						if template.name == stateKey {
							json.NewEncoder(writer).Encode(template.content)
							return
						}
					}
				}
			}
			writer.WriteHeader(http.StatusNotFound)
			json.NewEncoder(writer).Encode(messaging.MatrixError{Code: "M_NOT_FOUND", Message: "State event not found"})
			return
		}

		// GET specific state event: m.bureau.pipeline (pipeline room).
		if method == http.MethodGet && strings.Contains(rawPath, "/state/m.bureau.pipeline") {
			roomID := extractRoomIDFromStatePath(rawPath)
			if roomID == m.pipelineID {
				idx := strings.Index(rawPath, "/state/m.bureau.pipeline/")
				if idx >= 0 {
					stateKey := rawPath[idx+len("/state/m.bureau.pipeline/"):]
					stateKey, _ = url.PathUnescape(stateKey)
					pipelines, _ := content.Pipelines()
					for _, pipeline := range pipelines {
						if pipeline.Name == stateKey {
							json.NewEncoder(writer).Encode(pipeline.Content)
							return
						}
					}
				}
			}
			writer.WriteHeader(http.StatusNotFound)
			json.NewEncoder(writer).Encode(messaging.MatrixError{Code: "M_NOT_FOUND", Message: "State event not found"})
			return
		}

		// GET room members.
		if method == http.MethodGet && strings.Contains(path, "/members") && !strings.Contains(path, "/state") {
			roomID := extractRoomIDFromPath(rawPath)
			var memberUserIDs []string
			if m.roomMembers != nil {
				memberUserIDs = m.roomMembers[roomID]
			} else {
				memberUserIDs = []string{m.adminUserID}
			}
			var chunk []messaging.RoomMemberEvent
			for _, userID := range memberUserIDs {
				chunk = append(chunk, messaging.RoomMemberEvent{
					Type:     schema.MatrixEventTypeRoomMember,
					StateKey: userID,
					Content:  messaging.RoomMemberContent{Membership: "join"},
				})
			}
			json.NewEncoder(writer).Encode(messaging.RoomMembersResponse{Chunk: chunk})
			return
		}

		// GET full room state.
		if method == http.MethodGet && strings.HasSuffix(path, "/state") && !strings.Contains(path, "/state/") {
			roomID := extractRoomIDFromPath(rawPath)

			if roomID == m.spaceID {
				var events []messaging.Event
				events = append(events, messaging.Event{
					Type: "m.room.create", StateKey: &emptyStateKey,
					Content: map[string]any{"type": "m.space"},
				})

				childIDs := m.spaceChildren
				if childIDs == nil {
					childIDs = map[string]bool{
						m.systemID:   true,
						m.templateID: true,
						m.pipelineID: true,
						m.artifactID: true,
					}
				}
				for childID := range childIDs {
					id := childID
					events = append(events, messaging.Event{
						Type: schema.MatrixEventTypeSpaceChild, StateKey: &id,
						Content: map[string]any{"via": []string{"local"}},
					})
				}
				json.NewEncoder(writer).Encode(events)
				return
			}

			json.NewEncoder(writer).Encode([]messaging.Event{})
			return
		}

		t.Logf("unhandled request: %s %s (raw: %s)", method, path, rawPath)
		writer.WriteHeader(http.StatusNotFound)
		json.NewEncoder(writer).Encode(messaging.MatrixError{Code: "M_NOT_FOUND", Message: "Not found"})
	}
}

// powerLevelsForRoom returns the expected power levels for a Bureau room.
func powerLevelsForRoom(adminUserID, roomID, systemID, pipelineID, artifactID string) map[string]any {
	if roomID == pipelineID {
		return schema.PipelineRoomPowerLevels(adminUserID)
	}
	if roomID == artifactID {
		return schema.ArtifactRoomPowerLevels(adminUserID)
	}

	events := map[string]any{
		schema.MatrixEventTypeRoomName:    100,
		schema.MatrixEventTypeRoomTopic:   100,
		schema.MatrixEventTypePowerLevels: 100,
		schema.MatrixEventTypeSpaceChild:  100,
	}

	if roomID == systemID {
		events[schema.EventTypeTokenSigningKey] = 0
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
// this checks a few known patterns. Also extracts config room IDs
// of the form !config-<suffix>:local.
func extractRoomIDFromStatePath(path string) string {
	knownRooms := map[string]string{
		"system":   "!system:local",
		"space":    "!space:local",
		"template": "!template:local",
		"pipeline": "!pipeline:local",
		"artifact": "!artifact:local",
	}
	for name, roomID := range knownRooms {
		encoded := "%21" + name + "%3Alocal"
		if strings.Contains(path, encoded) || strings.Contains(path, roomID) {
			return roomID
		}
	}
	// Handle config room IDs of the form !config-<suffix>:local.
	// URL-encoded form: %21config-<suffix>%3Alocal
	const configPrefix = "%21config-"
	if idx := strings.Index(path, configPrefix); idx >= 0 {
		rest := path[idx+len(configPrefix):]
		if end := strings.Index(rest, "%3Alocal"); end >= 0 {
			return "!config-" + rest[:end] + ":local"
		}
	}
	// Also handle unencoded form.
	const rawConfigPrefix = "!config-"
	if idx := strings.Index(path, rawConfigPrefix); idx >= 0 {
		rest := path[idx+len(rawConfigPrefix):]
		if end := strings.Index(rest, ":local"); end >= 0 {
			suffix := rest[:end]
			// Stop at path separator.
			if slashIdx := strings.Index(suffix, "/"); slashIdx >= 0 {
				suffix = suffix[:slashIdx]
			}
			return "!config-" + suffix + ":local"
		}
	}
	return ""
}

// extractRoomIDFromPath extracts the room ID from paths like /rooms/{roomId}/state.
func extractRoomIDFromPath(path string) string {
	return extractRoomIDFromStatePath(path)
}

// mockBureauServer creates a fully healthy mock for backward compatibility.
func mockBureauServer(t *testing.T, adminUserID string) *httptest.Server {
	t.Helper()
	return newHealthyMock(adminUserID).httpServer(t)
}

// --- Integration tests ---

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

	results := runDoctor(t.Context(), client, session, "local", nil, "", testLogger())

	for _, result := range results {
		if result.Status == statusFail {
			t.Errorf("[FAIL] %s: %s", result.Name, result.Message)
		}
		if result.Status == statusWarn {
			t.Logf("[WARN] %s: %s", result.Name, result.Message)
		}
	}

	names := make(map[string]bool)
	for _, result := range results {
		names[result.Name] = true
	}

	expectedChecks := []string{
		"homeserver",
		"authentication",
		"bureau space",
		"system room",
		"template room",
		"system room in space",
		"template room in space",
		"bureau space admin power",
		"bureau space state_default",
		"system room admin power",
		"system room state_default",
		"system room m.bureau.token_signing_key",
		"template room admin power",
		"template room state_default",
		"bureau space join rules",
		"system room join rules",
		"template room join rules",
		"pipeline room",
		"pipeline room in space",
		"pipeline room admin power",
		"pipeline room state_default",
		"pipeline room join rules",
		"artifact room",
		"artifact room in space",
		"artifact room admin power",
		"artifact room state_default",
		"artifact room join rules",
		`template "base"`,
		`template "base-networked"`,
	}
	// Add expected pipeline checks dynamically from embedded content.
	pipelines, err := content.Pipelines()
	if err != nil {
		t.Fatalf("content.Pipelines(): %v", err)
	}
	for _, pipeline := range pipelines {
		expectedChecks = append(expectedChecks, fmt.Sprintf("pipeline %q", pipeline.Name))
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

	credentials := map[string]string{
		"MATRIX_SPACE_ROOM":    "!space:local",
		"MATRIX_SYSTEM_ROOM":   "!system:local",
		"MATRIX_TEMPLATE_ROOM": "!template:local",
		"MATRIX_PIPELINE_ROOM": "!pipeline:local",
		"MATRIX_ARTIFACT_ROOM": "!artifact:local",
	}

	results := runDoctor(t.Context(), client, session, "local", credentials, "", testLogger())

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

	credentials := map[string]string{
		"MATRIX_SPACE_ROOM":    "!space:local",
		"MATRIX_SYSTEM_ROOM":   "!wrong:local",
		"MATRIX_TEMPLATE_ROOM": "!template:local",
		"MATRIX_PIPELINE_ROOM": "!pipeline:local",
		"MATRIX_ARTIFACT_ROOM": "!artifact:local",
	}

	results := runDoctor(t.Context(), client, session, "local", credentials, "", testLogger())

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
	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session, err := client.SessionFromToken("@admin:local", "test-token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	defer session.Close()

	results := runDoctor(t.Context(), client, session, "local", nil, "", testLogger())

	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}

	if results[0].Name != "homeserver" || results[0].Status != statusFail {
		t.Errorf("expected homeserver FAIL, got %s %s", results[0].Name, results[0].Status)
	}

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

	results := runDoctor(t.Context(), client, session, "local", nil, "", testLogger())

	if results[0].Status != statusPass {
		t.Errorf("expected homeserver PASS, got %s: %s", results[0].Status, results[0].Message)
	}

	if results[1].Name != "authentication" || results[1].Status != statusFail {
		t.Errorf("expected authentication FAIL, got %s %s: %s", results[1].Name, results[1].Status, results[1].Message)
	}
	if !strings.Contains(results[1].Message, "invalid or expired") {
		t.Errorf("expected 'invalid or expired' in message, got: %s", results[1].Message)
	}

	for _, result := range results[2:] {
		if result.Status != statusSkip {
			t.Errorf("expected %s to be skipped after auth failure, got %s", result.Name, result.Status)
		}
	}
}

// --- Fix tests ---

func TestRunDoctor_FixMissingSpaceChild(t *testing.T) {
	adminUserID := "@bureau-admin:local"
	mock := newHealthyMock(adminUserID)
	mock.spaceChildren = map[string]bool{} // no children
	server := mock.httpServer(t)
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

	results := runDoctor(t.Context(), client, session, "local", nil, "", testLogger())

	// Space child checks should fail with fix hints.
	for _, result := range results {
		if strings.HasSuffix(result.Name, " in space") {
			if result.Status != statusFail {
				t.Errorf("expected %s to FAIL, got %s", result.Name, result.Status)
			}
			if result.FixHint == "" {
				t.Errorf("expected %s to have a fix hint", result.Name)
			}
			if result.fix == nil {
				t.Errorf("expected %s to have a fix function", result.Name)
			}
		}
	}

	executeFixes(t.Context(), session, results, false)

	// Verify m.space.child state events were sent.
	stateEvents := mock.getStateEvents()
	spaceChildCount := 0
	for _, event := range stateEvents {
		if event.eventType == schema.MatrixEventTypeSpaceChild {
			spaceChildCount++
		}
	}
	if spaceChildCount != len(standardRooms) {
		t.Errorf("expected %d m.space.child state events, got %d", len(standardRooms), spaceChildCount)
	}

	// Verify results updated to fixed.
	for _, result := range results {
		if strings.HasSuffix(result.Name, " in space") {
			if result.Status != statusFixed {
				t.Errorf("expected %s to be FIXED, got %s", result.Name, result.Status)
			}
		}
	}
}

func TestRunDoctor_FixJoinRules(t *testing.T) {
	adminUserID := "@bureau-admin:local"
	mock := newHealthyMock(adminUserID)
	mock.joinRules = map[string]string{
		"!system:local": "public",
	}
	server := mock.httpServer(t)
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

	results := runDoctor(t.Context(), client, session, "local", nil, "", testLogger())

	// System room join rules should fail.
	var systemJoinRulesIndex int
	found := false
	for i, result := range results {
		if result.Name == "system room join rules" {
			systemJoinRulesIndex = i
			found = true
			if result.Status != statusFail {
				t.Errorf("expected FAIL, got %s: %s", result.Status, result.Message)
			}
			if result.fix == nil {
				t.Fatal("expected fix function")
			}
			break
		}
	}
	if !found {
		t.Fatal("system room join rules check not found")
	}

	// Other rooms should still pass.
	for _, result := range results {
		if result.Name == "bureau space join rules" || result.Name == "template room join rules" {
			if result.Status != statusPass {
				t.Errorf("expected %s to PASS, got %s", result.Name, result.Status)
			}
		}
	}

	executeFixes(t.Context(), session, results, false)

	// Verify join rules state event was sent.
	stateEvents := mock.getStateEvents()
	joinRulesFound := false
	for _, event := range stateEvents {
		if event.eventType == schema.MatrixEventTypeJoinRules && event.roomID == "!system:local" {
			joinRulesFound = true
		}
	}
	if !joinRulesFound {
		t.Error("expected m.room.join_rules state event for system room")
	}
	if results[systemJoinRulesIndex].Status != statusFixed {
		t.Errorf("expected FIXED, got %s", results[systemJoinRulesIndex].Status)
	}
}

func TestRunDoctor_FixPowerLevels(t *testing.T) {
	adminUserID := "@bureau-admin:local"
	mock := newHealthyMock(adminUserID)
	mock.powerLevels = map[string]map[string]any{
		"!system:local": {
			"users":          map[string]any{adminUserID: float64(100)},
			"users_default":  float64(0),
			"state_default":  float64(50), // wrong
			"events_default": float64(0),
			"events":         map[string]any{},
		},
	}
	server := mock.httpServer(t)
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

	results := runDoctor(t.Context(), client, session, "local", nil, "", testLogger())

	// Find the system room state_default check.
	found := false
	for _, result := range results {
		if result.Name == "system room state_default" {
			found = true
			if result.Status != statusFail {
				t.Errorf("expected FAIL, got %s: %s", result.Status, result.Message)
			}
			if result.FixHint == "" {
				t.Error("expected fix hint")
			}
			break
		}
	}
	if !found {
		t.Fatal("system room state_default check not found")
	}

	executeFixes(t.Context(), session, results, false)

	// Verify power levels state event was sent.
	stateEvents := mock.getStateEvents()
	powerLevelsFound := false
	for _, event := range stateEvents {
		if event.eventType == schema.MatrixEventTypePowerLevels && event.roomID == "!system:local" {
			powerLevelsFound = true
		}
	}
	if !powerLevelsFound {
		t.Error("expected m.room.power_levels state event for system room")
	}
}

func TestRunDoctor_FixCredentialFile(t *testing.T) {
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

	// Write a credential file with stale room IDs and missing keys.
	credentialFile := t.TempDir() + "/bureau-creds"
	credentialContent := "# Bureau Matrix credentials\n" +
		"MATRIX_HOMESERVER_URL=http://localhost:6167\n" +
		"MATRIX_ADMIN_USER=" + adminUserID + "\n" +
		"MATRIX_ADMIN_TOKEN=test-token\n" +
		"MATRIX_SPACE_ROOM=!space:local\n" +
		"MATRIX_SYSTEM_ROOM=!wrong:local\n"
	if err := os.WriteFile(credentialFile, []byte(credentialContent), 0600); err != nil {
		t.Fatalf("write credential file: %v", err)
	}

	storedCredentials, err := cli.ReadCredentialFile(credentialFile)
	if err != nil {
		t.Fatalf("ReadCredentialFile: %v", err)
	}

	results := runDoctor(t.Context(), client, session, "local", storedCredentials, credentialFile, testLogger())

	// System room credential should be FAIL (was !wrong:local, should be !system:local).
	// Missing keys (machine, service, template, pipeline) should also be FAIL.
	credentialFailCount := 0
	var firstCredentialFail *checkResult
	for i, result := range results {
		if strings.HasSuffix(result.Name, " credential") && result.Status == statusFail {
			credentialFailCount++
			if firstCredentialFail == nil {
				firstCredentialFail = &results[i]
			}
		}
	}
	if credentialFailCount == 0 {
		t.Fatal("expected at least one credential FAIL")
	}
	if firstCredentialFail.fix == nil {
		t.Fatal("expected first credential failure to have a fix function")
	}
	if firstCredentialFail.FixHint != "update credential file" {
		t.Errorf("expected FixHint 'update credential file', got %q", firstCredentialFail.FixHint)
	}

	// Execute the fix.
	executeFixes(t.Context(), session, results, false)

	// Verify the credential file was updated.
	updatedCredentials, err := cli.ReadCredentialFile(credentialFile)
	if err != nil {
		t.Fatalf("read updated credential file: %v", err)
	}

	expectedRoomIDs := map[string]string{
		"MATRIX_SPACE_ROOM":    "!space:local",
		"MATRIX_SYSTEM_ROOM":   "!system:local",
		"MATRIX_TEMPLATE_ROOM": "!template:local",
		"MATRIX_PIPELINE_ROOM": "!pipeline:local",
		"MATRIX_ARTIFACT_ROOM": "!artifact:local",
	}
	for key, expectedValue := range expectedRoomIDs {
		if updatedCredentials[key] != expectedValue {
			t.Errorf("credential %s: expected %q, got %q", key, expectedValue, updatedCredentials[key])
		}
	}

	// Verify non-room fields were preserved.
	if updatedCredentials["MATRIX_ADMIN_TOKEN"] != "test-token" {
		t.Errorf("expected admin token preserved, got %q", updatedCredentials["MATRIX_ADMIN_TOKEN"])
	}
}

func TestRunDoctor_CredentialWarnWithoutFilePath(t *testing.T) {
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

	// Credentials with missing keys but no file path — should remain WARN.
	credentials := map[string]string{
		"MATRIX_SPACE_ROOM":  "!space:local",
		"MATRIX_SYSTEM_ROOM": "!system:local",
	}

	results := runDoctor(t.Context(), client, session, "local", credentials, "", testLogger())

	for _, result := range results {
		if strings.HasSuffix(result.Name, " credential") && result.Status == statusWarn {
			if result.fix != nil {
				t.Errorf("credential %q has fix function but no file path was provided", result.Name)
			}
		}
	}
}

func TestRunDoctor_DryRunNoMutations(t *testing.T) {
	adminUserID := "@bureau-admin:local"
	mock := newHealthyMock(adminUserID)
	mock.spaceChildren = map[string]bool{} // broken
	server := mock.httpServer(t)
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

	results := runDoctor(t.Context(), client, session, "local", nil, "", testLogger())

	executeFixes(t.Context(), session, results, true)

	if events := mock.getStateEvents(); len(events) != 0 {
		t.Errorf("expected no state events in dry-run mode, got %d", len(events))
	}

	for _, result := range results {
		if result.Status == statusFixed {
			t.Errorf("unexpected FIXED status in dry-run: %s", result.Name)
		}
	}
}

func TestRunDoctor_FixHintsPresent(t *testing.T) {
	adminUserID := "@bureau-admin:local"
	mock := newHealthyMock(adminUserID)
	mock.spaceChildren = map[string]bool{}
	mock.joinRules = map[string]string{"!space:local": "public"}
	server := mock.httpServer(t)
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

	results := runDoctor(t.Context(), client, session, "local", nil, "", testLogger())

	for _, result := range results {
		if result.Status == statusFail && result.fix != nil && result.FixHint == "" {
			t.Errorf("result %q has a fix function but no FixHint", result.Name)
		}
	}
}

// --- Unit tests ---

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
		{"@nobody:local", 0},
	}

	for _, test := range tests {
		level := getUserPowerLevel(powerLevels, test.userID)
		if level != test.expected {
			t.Errorf("getUserPowerLevel(%q) = %v, want %v", test.userID, level, test.expected)
		}
	}
}

func TestGetStateEventPowerLevel(t *testing.T) {
	powerLevels := map[string]any{
		"events":         map[string]any{schema.MatrixEventTypeRoomName: float64(100), schema.EventTypeMachineKey: float64(0)},
		"events_default": float64(50),
		"state_default":  float64(75),
	}

	tests := []struct {
		eventType string
		expected  float64
	}{
		{schema.MatrixEventTypeRoomName, 100},
		{schema.EventTypeMachineKey, 0},
		// Unlisted state events fall back to state_default (75), not
		// events_default (50). The Matrix spec requires this: state events
		// not in the events map use state_default.
		{"m.unknown.type", 75},
	}

	for _, test := range tests {
		level := getStateEventPowerLevel(powerLevels, test.eventType)
		if level != test.expected {
			t.Errorf("getStateEventPowerLevel(%q) = %v, want %v", test.eventType, level, test.expected)
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

func TestDoctorCommand_DryRunRequiresFix(t *testing.T) {
	command := DoctorCommand()
	err := command.Execute([]string{
		"--homeserver", "http://localhost:6167",
		"--token", "tok",
		"--user-id", "@a:b",
		"--dry-run",
	})
	if err == nil {
		t.Fatal("expected error for --dry-run without --fix")
	}
	if !strings.Contains(err.Error(), "--dry-run requires --fix") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestResolveHomeserverURL(t *testing.T) {
	t.Run("from flag", func(t *testing.T) {
		config := cli.SessionConfig{HomeserverURL: "http://localhost:6167"}
		resolvedURL, err := config.ResolveHomeserverURL()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resolvedURL != "http://localhost:6167" {
			t.Errorf("expected http://localhost:6167, got %s", resolvedURL)
		}
	})

	t.Run("missing both", func(t *testing.T) {
		config := cli.SessionConfig{}
		_, err := config.ResolveHomeserverURL()
		if err == nil {
			t.Fatal("expected error for missing homeserver and credential file")
		}
	})
}

func TestExitError(t *testing.T) {
	err := &cli.ExitError{Code: 42}
	if err.Error() != "exit code 42" {
		t.Errorf("unexpected error string: %s", err.Error())
	}
	if err.ExitCode() != 42 {
		t.Errorf("expected exit code 42, got %d", err.ExitCode())
	}
}

func TestExecuteFixes(t *testing.T) {
	called := false
	results := []checkResult{
		pass("check1", "ok"),
		{
			Name: "check2", Status: statusFail, Message: "broken",
			FixHint: "fix it",
			fix: func(ctx context.Context, session messaging.Session) error {
				called = true
				return nil
			},
		},
		fail("check3", "also broken"),
	}

	fixCount := executeFixes(t.Context(), nil, results, false)

	if !called {
		t.Error("fix function was not called")
	}
	if fixCount != 1 {
		t.Errorf("expected fixCount=1, got %d", fixCount)
	}
	if results[0].Status != statusPass {
		t.Errorf("check1 should still be PASS, got %s", results[0].Status)
	}
	if results[1].Status != statusFixed {
		t.Errorf("check2 should be FIXED, got %s", results[1].Status)
	}
	if results[2].Status != statusFail {
		t.Errorf("check3 should still be FAIL (no fix function), got %s", results[2].Status)
	}
}

func TestExecuteFixes_ReturnsZeroOnDryRun(t *testing.T) {
	results := []checkResult{
		failWithFix("check1", "broken", "fix it", func(ctx context.Context, session messaging.Session) error {
			t.Error("fix should not be called in dry-run mode")
			return nil
		}),
	}

	fixCount := executeFixes(t.Context(), nil, results, true)
	if fixCount != 0 {
		t.Errorf("expected fixCount=0 in dry-run, got %d", fixCount)
	}
}

func TestFailWithFix(t *testing.T) {
	result := failWithFix("test", "broken", "repair it", func(ctx context.Context, session messaging.Session) error {
		return nil
	})
	if result.Status != statusFail {
		t.Errorf("expected FAIL, got %s", result.Status)
	}
	if result.FixHint != "repair it" {
		t.Errorf("expected FixHint 'repair it', got %q", result.FixHint)
	}
	if result.fix == nil {
		t.Error("expected non-nil fix function")
	}
}
