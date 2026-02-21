// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// mockMatrixState holds configurable state for a mock Matrix homeserver.
// Thread-safe: state can be updated between reconcile calls from the test
// goroutine while the mock server handles requests.
type mockMatrixState struct {
	mu sync.Mutex

	// stateEvents stores individual state event content. Key format:
	// "roomID\x00eventType\x00stateKey" → JSON content bytes.
	stateEvents map[string]json.RawMessage

	// roomStates stores arrays of state events for GetRoomState responses.
	// Key: roomID.
	roomStates map[string][]mockRoomStateEvent

	// roomAliases maps room aliases to room IDs for ResolveAlias.
	// Key: full alias (e.g., "#iree/amdgpu/general:bureau.local").
	roomAliases map[string]string

	// roomMembers maps room IDs to member lists for GetRoomMembers.
	roomMembers map[string][]mockRoomMember

	// stateEventWritten is signaled (non-blocking) whenever a PUT state
	// event arrives. Tests can wait on this to detect when production
	// code publishes a state event. Nil means no notification.
	stateEventWritten chan string

	// joinedRooms tracks which rooms each user has joined via the JoinRoom
	// endpoint. Key: room ID, value: set of user IDs that called join.
	joinedRooms map[string]map[string]bool

	// syncBatch is a counter for generating unique next_batch tokens.
	syncBatch int

	// pendingSyncEvents holds events to include in the next incremental
	// /sync response. Keyed by room ID. Cleared after each sync response.
	pendingSyncEvents map[string][]mockRoomStateEvent

	// invites tracks rooms with pending invites. Included in the /sync
	// response's rooms.invite section. Cleared when handleJoinRoom is
	// called for the room (accepting the invite moves it to joined).
	invites map[string]bool
}

// mockRoomMember represents a member for the /members endpoint.
type mockRoomMember struct {
	UserID      string `json:"user_id"`
	Membership  string `json:"membership"`
	DisplayName string `json:"displayname,omitempty"`
}

// mockRoomStateEvent represents a single state event in a GetRoomState response.
// The Content field uses map[string]any because that's what the messaging
// library's Event type uses after JSON unmarshaling.
type mockRoomStateEvent struct {
	Type     string         `json:"type"`
	StateKey *string        `json:"state_key"`
	Content  map[string]any `json:"content"`
}

func newMockMatrixState() *mockMatrixState {
	return &mockMatrixState{
		stateEvents:       make(map[string]json.RawMessage),
		roomStates:        make(map[string][]mockRoomStateEvent),
		roomAliases:       make(map[string]string),
		roomMembers:       make(map[string][]mockRoomMember),
		joinedRooms:       make(map[string]map[string]bool),
		pendingSyncEvents: make(map[string][]mockRoomStateEvent),
		invites:           make(map[string]bool),
	}
}

// enqueueSyncEvent queues a state event to appear in the next incremental
// /sync response for the given room. Used by tests to simulate state changes
// arriving via /sync.
func (m *mockMatrixState) enqueueSyncEvent(roomID string, event mockRoomStateEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pendingSyncEvents[roomID] = append(m.pendingSyncEvents[roomID], event)
}

// addInvite marks a room as having a pending invite. The room will appear
// in the /sync response's rooms.invite section until the daemon calls
// JoinRoom (which clears the invite).
func (m *mockMatrixState) addInvite(roomID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.invites[roomID] = true
}

func (m *mockMatrixState) setStateEvent(roomID, eventType, stateKey string, content any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, _ := json.Marshal(content)
	key := roomID + "\x00" + eventType + "\x00" + stateKey
	m.stateEvents[key] = data
}

func (m *mockMatrixState) setRoomState(roomID string, events []mockRoomStateEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roomStates[roomID] = events
}

func (m *mockMatrixState) setRoomAlias(alias ref.RoomAlias, roomID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roomAliases[alias.String()] = roomID
}

func (m *mockMatrixState) setRoomMembers(roomID string, members []mockRoomMember) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roomMembers[roomID] = members
}

// handler returns an http.Handler that implements the subset of the Matrix
// client-server API used by the daemon: GetStateEvent, GetRoomState,
// SendStateEvent, ResolveAlias, GetRoomMembers, and Sync.
//
// URL path parsing handles percent-encoded room IDs and state keys (the
// messaging library uses url.PathEscape which encodes /, :, ! etc.). The
// handler splits on literal "/" in the raw path and decodes each segment
// individually, so an encoded state key like "test%2Fecho" is correctly
// decoded to "test/echo".
func (m *mockMatrixState) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Use RawPath to preserve percent-encoded slashes in state keys.
		rawPath := r.URL.RawPath
		if rawPath == "" {
			rawPath = r.URL.Path
		}

		// GET /_matrix/client/v3/sync — sync.
		if rawPath == "/_matrix/client/v3/sync" && r.Method == "GET" {
			since := r.URL.Query().Get("since")
			m.handleSync(w, since)
			return
		}

		// POST /_matrix/client/v3/join/{roomIdOrAlias} — join room.
		const joinPrefix = "/_matrix/client/v3/join/"
		if strings.HasPrefix(rawPath, joinPrefix) && r.Method == "POST" {
			encoded := rawPath[len(joinPrefix):]
			roomIDOrAlias, _ := url.PathUnescape(encoded)
			m.handleJoinRoom(w, r, roomIDOrAlias)
			return
		}

		// GET /_matrix/client/v3/directory/room/{alias} — resolve alias.
		const directoryPrefix = "/_matrix/client/v3/directory/room/"
		if strings.HasPrefix(rawPath, directoryPrefix) && r.Method == "GET" {
			encodedAlias := rawPath[len(directoryPrefix):]
			alias, _ := url.PathUnescape(encodedAlias)
			m.handleResolveAlias(w, alias)
			return
		}

		const roomsPrefix = "/_matrix/client/v3/rooms/"
		if !strings.HasPrefix(rawPath, roomsPrefix) {
			http.NotFound(w, r)
			return
		}

		// Split into room ID and the rest of the path.
		rest := rawPath[len(roomsPrefix):]
		segments := strings.SplitN(rest, "/", 2)
		if len(segments) < 2 {
			http.NotFound(w, r)
			return
		}

		roomID, _ := url.PathUnescape(segments[0])
		pathAfterRoom := segments[1]

		// GET /rooms/{roomId}/state — return all state events.
		if pathAfterRoom == "state" && r.Method == "GET" {
			m.handleGetRoomState(w, roomID)
			return
		}

		// GET or PUT /rooms/{roomId}/state/{eventType}/{stateKey}
		if strings.HasPrefix(pathAfterRoom, "state/") {
			stateRest := pathAfterRoom[len("state/"):]
			typeAndKey := strings.SplitN(stateRest, "/", 2)
			if len(typeAndKey) < 2 {
				http.NotFound(w, r)
				return
			}

			eventType, _ := url.PathUnescape(typeAndKey[0])
			stateKey, _ := url.PathUnescape(typeAndKey[1])

			switch r.Method {
			case "GET":
				m.handleGetStateEvent(w, roomID, eventType, stateKey)
			case "PUT":
				m.handlePutStateEvent(w, r, roomID, eventType, stateKey)
			default:
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}

		// POST /rooms/{roomId}/invite — invite user.
		if pathAfterRoom == "invite" && r.Method == "POST" {
			m.handleInvite(w, r, roomID)
			return
		}

		// PUT /rooms/{roomId}/send/{eventType}/{txnId} — send event.
		if strings.HasPrefix(pathAfterRoom, "send/") && r.Method == "PUT" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"event_id": "$mock-send-event-id",
			})
			return
		}

		// GET /rooms/{roomId}/members — return room members.
		if pathAfterRoom == "members" && r.Method == "GET" {
			m.handleGetRoomMembers(w, roomID)
			return
		}

		http.NotFound(w, r)
	})
}

func (m *mockMatrixState) handleJoinRoom(w http.ResponseWriter, r *http.Request, roomIDOrAlias string) {
	// Resolve alias to room ID if it looks like an alias.
	roomID := roomIDOrAlias
	if strings.HasPrefix(roomIDOrAlias, "#") {
		m.mu.Lock()
		resolved, ok := m.roomAliases[roomIDOrAlias]
		m.mu.Unlock()
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{
				"errcode": "M_NOT_FOUND",
				"error":   fmt.Sprintf("room alias %q not found", roomIDOrAlias),
			})
			return
		}
		roomID = resolved
	}

	m.mu.Lock()
	if m.joinedRooms[roomID] == nil {
		m.joinedRooms[roomID] = make(map[string]bool)
	}
	m.joinedRooms[roomID]["_joined"] = true
	delete(m.invites, roomID)
	m.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"room_id": roomID,
	})
}

// hasJoined returns true if any client called JoinRoom on the given room ID.
func (m *mockMatrixState) hasJoined(roomID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.joinedRooms[roomID]["_joined"]
}

func (m *mockMatrixState) handleGetStateEvent(w http.ResponseWriter, roomID, eventType, stateKey string) {
	m.mu.Lock()
	key := roomID + "\x00" + eventType + "\x00" + stateKey
	data, ok := m.stateEvents[key]
	m.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{
			"errcode": "M_NOT_FOUND",
			"error":   fmt.Sprintf("state event not found: %s/%s in %s", eventType, stateKey, roomID),
		})
		return
	}

	w.Write(data)
}

func (m *mockMatrixState) handleGetRoomState(w http.ResponseWriter, roomID string) {
	m.mu.Lock()
	events, ok := m.roomStates[roomID]
	m.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if !ok || len(events) == 0 {
		w.Write([]byte("[]"))
		return
	}

	json.NewEncoder(w).Encode(events)
}

func (m *mockMatrixState) handlePutStateEvent(w http.ResponseWriter, r *http.Request, roomID, eventType, stateKey string) {
	body, _ := io.ReadAll(r.Body)

	// Store the event (so it can be read back via GetStateEvent).
	m.mu.Lock()
	key := roomID + "\x00" + eventType + "\x00" + stateKey
	m.stateEvents[key] = json.RawMessage(body)
	writeNotify := m.stateEventWritten
	m.mu.Unlock()

	// Signal listeners that a state event was written (non-blocking).
	if writeNotify != nil {
		select {
		case writeNotify <- key:
		default:
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"event_id": "$mock-event-id",
	})
}

func (m *mockMatrixState) handleInvite(w http.ResponseWriter, r *http.Request, roomID string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{})
}

func (m *mockMatrixState) handleResolveAlias(w http.ResponseWriter, alias string) {
	m.mu.Lock()
	roomID, ok := m.roomAliases[alias]
	m.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{
			"errcode": "M_NOT_FOUND",
			"error":   fmt.Sprintf("room alias %q not found", alias),
		})
		return
	}

	json.NewEncoder(w).Encode(map[string]any{
		"room_id": roomID,
		"servers": []string{"bureau.local"},
	})
}

func (m *mockMatrixState) handleGetRoomMembers(w http.ResponseWriter, roomID string) {
	m.mu.Lock()
	members, ok := m.roomMembers[roomID]
	m.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if !ok {
		json.NewEncoder(w).Encode(map[string]any{"chunk": []any{}})
		return
	}

	// Build the member events in the format that GetRoomMembers expects.
	var chunk []map[string]any
	for _, member := range members {
		chunk = append(chunk, map[string]any{
			"type":      schema.MatrixEventTypeRoomMember,
			"state_key": member.UserID,
			"sender":    member.UserID,
			"content": map[string]any{
				"membership":  member.Membership,
				"displayname": member.DisplayName,
			},
		})
	}

	json.NewEncoder(w).Encode(map[string]any{"chunk": chunk})
}

// handleSync returns a /sync response. On initial sync (empty since), it
// returns all rooms that have roomStates configured as joined rooms with
// their state events. On incremental sync (non-empty since), it returns
// any pending events queued via enqueueSyncEvent.
func (m *mockMatrixState) handleSync(w http.ResponseWriter, since string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.syncBatch++
	nextBatch := fmt.Sprintf("batch_%d", m.syncBatch)

	joinedRooms := make(map[string]any)

	if since == "" {
		// Initial sync: return all configured room state.
		for roomID, events := range m.roomStates {
			joinedRooms[roomID] = map[string]any{
				"state": map[string]any{
					"events": events,
				},
				"timeline": map[string]any{
					"events":     []any{},
					"prev_batch": "",
					"limited":    false,
				},
			}
		}
	} else {
		// Incremental sync: return pending events and clear the queue.
		for roomID, events := range m.pendingSyncEvents {
			// State events in incremental sync appear as timeline events
			// with state_key set (matching real Matrix server behavior).
			timelineEvents := make([]any, 0, len(events))
			for _, event := range events {
				timelineEvents = append(timelineEvents, map[string]any{
					"type":      event.Type,
					"state_key": event.StateKey,
					"content":   event.Content,
					"event_id":  fmt.Sprintf("$sync_%d", m.syncBatch),
					"sender":    "@admin:bureau.local",
				})
			}
			joinedRooms[roomID] = map[string]any{
				"state": map[string]any{
					"events": []any{},
				},
				"timeline": map[string]any{
					"events":     timelineEvents,
					"prev_batch": since,
					"limited":    false,
				},
			}
		}
		m.pendingSyncEvents = make(map[string][]mockRoomStateEvent)
	}

	// Build invite section from pending invites.
	invitedRooms := make(map[string]any)
	for roomID := range m.invites {
		invitedRooms[roomID] = map[string]any{
			"invite_state": map[string]any{
				"events": []any{},
			},
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"next_batch": nextBatch,
		"rooms": map[string]any{
			"join":   joinedRooms,
			"invite": invitedRooms,
		},
	})
}
