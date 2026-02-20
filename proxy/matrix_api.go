// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bureau-foundation/bureau/lib/netutil"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/messaging"
)

// Structured Matrix API endpoints for sandboxed processes.
//
// These endpoints sit at /v1/matrix/* on the agent socket. They provide a
// stable, structured JSON interface for the five Matrix operations that
// pipelines and other sandboxed code need: whoami, resolve alias, get state,
// put state, and send message.
//
// The proxy translates each structured request into the appropriate Matrix
// Client-Server API call, injecting credentials and handling alias
// resolution. This decouples sandbox code from Matrix URL conventions — if
// we change room naming, URL patterns, or homeserver API versions, only the
// proxy changes. It also makes testing clean: mock five JSON endpoints
// instead of Matrix HTTP paths.
//
// Authorization is handled by the homeserver: room membership and power
// levels are enforced on every forwarded request. Write operations that
// create new room-level state (join, invite, create-room) require explicit
// grants checked by requireGrant before forwarding.

// matrixServiceName is the HTTPService name used for Matrix homeserver access.
// The daemon registers this service on the proxy when configuring credentials.
const matrixServiceName = "matrix"

// matrixTransactionCounter generates unique transaction IDs for Matrix
// send requests. Monotonically increasing within a process lifetime.
var matrixTransactionCounter atomic.Int64

// MatrixStateRequest is the JSON body for POST /v1/matrix/state.
type MatrixStateRequest struct {
	// Room is the target room ID (e.g., "!abc:bureau.local") or alias
	// (e.g., "#bureau/pipeline:bureau.local"). When an alias is provided,
	// the proxy resolves it to a room ID before forwarding.
	Room string `json:"room"`

	// EventType is the Matrix state event type (e.g., "m.bureau.pipeline").
	EventType string `json:"event_type"`

	// StateKey is the state key for the event (e.g., "dev-workspace-init").
	StateKey string `json:"state_key"`

	// Content is the event content, forwarded as-is to the homeserver.
	Content any `json:"content"`
}

// MatrixMessageRequest is the JSON body for POST /v1/matrix/message.
type MatrixMessageRequest struct {
	// Room is the target room ID or alias. When an alias is provided,
	// the proxy resolves it to a room ID before forwarding.
	Room string `json:"room"`

	// Content is the full message content including msgtype, body, and
	// optional fields like m.relates_to for thread replies.
	Content any `json:"content"`
}

// MatrixEventResponse is returned by POST /v1/matrix/state and
// POST /v1/matrix/message.
type MatrixEventResponse struct {
	EventID string `json:"event_id"`
}

// matrixErrorResponse is the JSON body returned when a Matrix API call fails.
type matrixErrorResponse struct {
	Error   string `json:"error"`
	Code    string `json:"errcode,omitempty"`
	Details string `json:"details,omitempty"`
}

// respondError writes a JSON error response with the given HTTP status code
// and message. Used by Matrix API handlers for both client errors (400) and
// gateway errors (502).
func respondError(w http.ResponseWriter, statusCode int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(matrixErrorResponse{Error: message})
}

// respondErrorf is like respondError but accepts a format string.
func respondErrorf(w http.ResponseWriter, statusCode int, format string, args ...any) {
	respondError(w, statusCode, fmt.Sprintf(format, args...))
}

// getMatrixService looks up the "matrix" HTTPService and returns it. If the
// service is not registered, writes a 503 error response and returns nil.
func (h *Handler) getMatrixService(w http.ResponseWriter) *HTTPService {
	h.mu.RLock()
	service := h.httpServices[matrixServiceName]
	h.mu.RUnlock()

	if service == nil {
		respondError(w, http.StatusServiceUnavailable, "matrix service not configured")
		return nil
	}
	return service
}

// resolveRoomAlias resolves a room alias to a room ID using the Matrix
// directory API. Returns the room ID on success.
func (h *Handler) resolveRoomAlias(r *http.Request, service *HTTPService, alias string) (string, error) {
	path := "/_matrix/client/v3/directory/room/" + url.PathEscape(alias)
	response, err := service.ForwardRequest(r.Context(), http.MethodGet, path, nil)
	if err != nil {
		return "", fmt.Errorf("resolving alias %q: %w", alias, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("resolving alias %q: HTTP %d: %s", alias, response.StatusCode, netutil.ErrorBody(response.Body))
	}

	var result struct {
		RoomID string `json:"room_id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("parsing resolve response for %q: %w", alias, err)
	}
	if result.RoomID == "" {
		return "", fmt.Errorf("resolving alias %q: empty room_id in response", alias)
	}
	return result.RoomID, nil
}

// resolveRoom resolves a room identifier to a room ID. If the identifier
// starts with "#", it is treated as an alias and resolved via the Matrix
// directory API. Otherwise it is returned as-is (assumed to be a room ID).
func (h *Handler) resolveRoom(r *http.Request, service *HTTPService, room string) (string, error) {
	if strings.HasPrefix(room, "#") {
		return h.resolveRoomAlias(r, service, room)
	}
	return room, nil
}

// copyResponseBody copies an HTTP response body to the response writer.
// Headers must already be written. Logs a warning if the copy fails for a
// reason other than normal client disconnect (EOF, connection reset, etc.).
func (h *Handler) copyResponseBody(w http.ResponseWriter, body io.Reader, endpoint string) {
	if _, err := io.Copy(w, body); err != nil && !netutil.IsExpectedCloseError(err) {
		h.logger.Warn("response body copy failed",
			"endpoint", endpoint,
			"error", err,
		)
	}
}

// forwardSimpleGet forwards a parameterless GET request to the Matrix
// homeserver at the given path and streams the response to the client.
// Used for endpoints that need no query parameters (whoami, joined-rooms).
func (h *Handler) forwardSimpleGet(w http.ResponseWriter, r *http.Request, matrixPath, endpoint string) {
	service := h.getMatrixService(w)
	if service == nil {
		return
	}

	response, err := service.ForwardRequest(r.Context(), http.MethodGet, matrixPath, nil)
	if err != nil {
		h.logger.Error("matrix "+endpoint+" failed", "error", err)
		respondError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer response.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	h.copyResponseBody(w, response.Body, endpoint)
}

// forwardRoomGet resolves a room from the "room" query parameter and forwards
// a GET request to a room-scoped Matrix endpoint. The pathSuffix is appended
// after the room ID (e.g., "/state", "/members").
func (h *Handler) forwardRoomGet(w http.ResponseWriter, r *http.Request, pathSuffix, endpoint string) {
	service := h.getMatrixService(w)
	if service == nil {
		return
	}

	room := r.URL.Query().Get("room")
	if room == "" {
		respondError(w, http.StatusBadRequest, "room query parameter is required")
		return
	}

	roomID, err := h.resolveRoom(r, service, room)
	if err != nil {
		h.logger.Error("matrix "+endpoint+": room resolution failed", "room", room, "error", err)
		respondError(w, http.StatusBadGateway, err.Error())
		return
	}

	path := "/_matrix/client/v3/rooms/" + url.PathEscape(roomID) + pathSuffix
	response, err := service.ForwardRequest(r.Context(), http.MethodGet, path, nil)
	if err != nil {
		h.logger.Error("matrix "+endpoint+" failed", "room", roomID, "error", err)
		respondError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer response.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	h.copyResponseBody(w, response.Body, endpoint)
}

// HandleMatrixWhoami handles GET /v1/matrix/whoami. Returns the agent's
// Matrix user ID by forwarding to /_matrix/client/v3/account/whoami.
func (h *Handler) HandleMatrixWhoami(w http.ResponseWriter, r *http.Request) {
	h.forwardSimpleGet(w, r, "/_matrix/client/v3/account/whoami", "whoami")
}

// HandleMatrixResolve handles GET /v1/matrix/resolve?alias=<alias>. Resolves
// a room alias to a room ID via /_matrix/client/v3/directory/room/{alias}.
func (h *Handler) HandleMatrixResolve(w http.ResponseWriter, r *http.Request) {
	service := h.getMatrixService(w)
	if service == nil {
		return
	}

	alias := r.URL.Query().Get("alias")
	if alias == "" {
		respondError(w, http.StatusBadRequest, "alias query parameter is required")
		return
	}

	path := "/_matrix/client/v3/directory/room/" + url.PathEscape(alias)
	response, err := service.ForwardRequest(r.Context(), http.MethodGet, path, nil)
	if err != nil {
		h.logger.Error("matrix resolve failed", "alias", alias, "error", err)
		respondError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer response.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	h.copyResponseBody(w, response.Body, "resolve")
}

// HandleMatrixGetState handles GET /v1/matrix/state?room=<id>&type=<type>&key=<key>.
// Reads a state event from the specified room via
// /_matrix/client/v3/rooms/{roomId}/state/{type}/{key}.
func (h *Handler) HandleMatrixGetState(w http.ResponseWriter, r *http.Request) {
	service := h.getMatrixService(w)
	if service == nil {
		return
	}

	room := r.URL.Query().Get("room")
	eventType := r.URL.Query().Get("type")
	stateKey := r.URL.Query().Get("key")

	if room == "" || eventType == "" {
		respondError(w, http.StatusBadRequest, "room and type query parameters are required")
		return
	}

	// Resolve alias if needed.
	roomID, err := h.resolveRoom(r, service, room)
	if err != nil {
		h.logger.Error("matrix get state: room resolution failed", "room", room, "error", err)
		respondError(w, http.StatusBadGateway, err.Error())
		return
	}

	path := "/_matrix/client/v3/rooms/" + url.PathEscape(roomID) + "/state/" + url.PathEscape(eventType) + "/" + url.PathEscape(stateKey)
	response, err := service.ForwardRequest(r.Context(), http.MethodGet, path, nil)
	if err != nil {
		h.logger.Error("matrix get state failed", "room", roomID, "type", eventType, "key", stateKey, "error", err)
		respondError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer response.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	h.copyResponseBody(w, response.Body, "get_state")
}

// HandleMatrixPutState handles POST /v1/matrix/state. Publishes a state event
// to the specified room via PUT /_matrix/client/v3/rooms/{roomId}/state/{type}/{key}.
//
// When the room field is an alias (starts with "#"), the proxy resolves it to
// a room ID before forwarding.
func (h *Handler) HandleMatrixPutState(w http.ResponseWriter, r *http.Request) {
	service := h.getMatrixService(w)
	if service == nil {
		return
	}

	var request MatrixStateRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		respondErrorf(w, http.StatusBadRequest, "invalid request body: %v", err)
		return
	}

	if request.Room == "" || request.EventType == "" {
		respondError(w, http.StatusBadRequest, "room and event_type are required")
		return
	}

	// Resolve alias if needed.
	roomID, err := h.resolveRoom(r, service, request.Room)
	if err != nil {
		h.logger.Error("matrix put state: room resolution failed", "room", request.Room, "error", err)
		respondError(w, http.StatusBadGateway, err.Error())
		return
	}

	// Marshal just the content for the upstream PUT body.
	contentBody, err := json.Marshal(request.Content)
	if err != nil {
		respondErrorf(w, http.StatusBadRequest, "invalid content: %v", err)
		return
	}

	path := "/_matrix/client/v3/rooms/" + url.PathEscape(roomID) + "/state/" + url.PathEscape(request.EventType) + "/" + url.PathEscape(request.StateKey)

	h.logger.Info("matrix put state",
		"room", roomID,
		"event_type", request.EventType,
		"state_key", request.StateKey,
	)

	response, err := service.ForwardRequest(r.Context(), http.MethodPut, path, bytes.NewReader(contentBody))
	if err != nil {
		h.logger.Error("matrix put state failed", "room", roomID, "type", request.EventType, "error", err)
		respondError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer response.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	h.copyResponseBody(w, response.Body, "put_state")
}

// HandleMatrixSendMessage handles POST /v1/matrix/message. Sends a message
// to the specified room via PUT /_matrix/client/v3/rooms/{roomId}/send/m.room.message/{txnId}.
//
// When the room field is an alias (starts with "#"), the proxy resolves it to
// a room ID before forwarding. Transaction IDs are generated automatically.
func (h *Handler) HandleMatrixSendMessage(w http.ResponseWriter, r *http.Request) {
	service := h.getMatrixService(w)
	if service == nil {
		return
	}

	var request MatrixMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		respondErrorf(w, http.StatusBadRequest, "invalid request body: %v", err)
		return
	}

	if request.Room == "" {
		respondError(w, http.StatusBadRequest, "room is required")
		return
	}

	// Resolve alias if needed.
	roomID, err := h.resolveRoom(r, service, request.Room)
	if err != nil {
		h.logger.Error("matrix send message: room resolution failed", "room", request.Room, "error", err)
		respondError(w, http.StatusBadGateway, err.Error())
		return
	}

	// Marshal the content for the upstream PUT body.
	contentBody, err := json.Marshal(request.Content)
	if err != nil {
		respondErrorf(w, http.StatusBadRequest, "invalid content: %v", err)
		return
	}

	// Generate a unique transaction ID.
	transactionID := fmt.Sprintf("bureau_%d_%d", time.Now().UnixMilli(), matrixTransactionCounter.Add(1))
	path := "/_matrix/client/v3/rooms/" + url.PathEscape(roomID) + "/send/m.room.message/" + url.PathEscape(transactionID)

	h.logger.Info("matrix send message",
		"room", roomID,
		"txn_id", transactionID,
	)

	response, err := service.ForwardRequest(r.Context(), http.MethodPut, path, bytes.NewReader(contentBody))
	if err != nil {
		h.logger.Error("matrix send message failed", "room", roomID, "error", err)
		respondError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer response.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	h.copyResponseBody(w, response.Body, "send_message")
}

// HandleMatrixJoinedRooms handles GET /v1/matrix/joined-rooms. Returns the
// list of room IDs the agent has joined via GET /_matrix/client/v3/joined_rooms.
func (h *Handler) HandleMatrixJoinedRooms(w http.ResponseWriter, r *http.Request) {
	h.forwardSimpleGet(w, r, "/_matrix/client/v3/joined_rooms", "joined_rooms")
}

// HandleMatrixGetRoomState handles GET /v1/matrix/room-state?room=<id>.
// Returns all current state events from the room via
// GET /_matrix/client/v3/rooms/{roomId}/state.
func (h *Handler) HandleMatrixGetRoomState(w http.ResponseWriter, r *http.Request) {
	h.forwardRoomGet(w, r, "/state", "get_room_state")
}

// HandleMatrixGetRoomMembers handles GET /v1/matrix/room-members?room=<id>.
// Returns room membership via GET /_matrix/client/v3/rooms/{roomId}/members.
func (h *Handler) HandleMatrixGetRoomMembers(w http.ResponseWriter, r *http.Request) {
	h.forwardRoomGet(w, r, "/members", "get_room_members")
}

// HandleMatrixMessages handles GET /v1/matrix/messages?room=<id>&from=...&dir=...&limit=...
// Fetches paginated room messages via GET /_matrix/client/v3/rooms/{roomId}/messages.
func (h *Handler) HandleMatrixMessages(w http.ResponseWriter, r *http.Request) {
	service := h.getMatrixService(w)
	if service == nil {
		return
	}

	room := r.URL.Query().Get("room")
	if room == "" {
		respondError(w, http.StatusBadRequest, "room query parameter is required")
		return
	}

	roomID, err := h.resolveRoom(r, service, room)
	if err != nil {
		h.logger.Error("matrix messages: room resolution failed", "room", room, "error", err)
		respondError(w, http.StatusBadGateway, err.Error())
		return
	}

	// Build upstream query parameters from the client request.
	upstreamQuery := url.Values{}
	if from := r.URL.Query().Get("from"); from != "" {
		upstreamQuery.Set("from", from)
	}
	direction := r.URL.Query().Get("dir")
	if direction == "" {
		direction = "b"
	}
	upstreamQuery.Set("dir", direction)
	if limit := r.URL.Query().Get("limit"); limit != "" {
		upstreamQuery.Set("limit", limit)
	}

	path := "/_matrix/client/v3/rooms/" + url.PathEscape(roomID) + "/messages?" + upstreamQuery.Encode()
	response, err := service.ForwardRequest(r.Context(), http.MethodGet, path, nil)
	if err != nil {
		h.logger.Error("matrix messages failed", "room", roomID, "error", err)
		respondError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer response.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	h.copyResponseBody(w, response.Body, "messages")
}

// HandleMatrixThreadMessages handles GET /v1/matrix/thread-messages?room=<id>&thread=<eventId>&from=...&limit=...
// Fetches thread messages via GET /_matrix/client/v3/rooms/{roomId}/relations/{eventId}/m.thread.
func (h *Handler) HandleMatrixThreadMessages(w http.ResponseWriter, r *http.Request) {
	service := h.getMatrixService(w)
	if service == nil {
		return
	}

	room := r.URL.Query().Get("room")
	thread := r.URL.Query().Get("thread")
	if room == "" || thread == "" {
		respondError(w, http.StatusBadRequest, "room and thread query parameters are required")
		return
	}

	roomID, err := h.resolveRoom(r, service, room)
	if err != nil {
		h.logger.Error("matrix thread messages: room resolution failed", "room", room, "error", err)
		respondError(w, http.StatusBadGateway, err.Error())
		return
	}

	upstreamQuery := url.Values{}
	if from := r.URL.Query().Get("from"); from != "" {
		upstreamQuery.Set("from", from)
	}
	if limit := r.URL.Query().Get("limit"); limit != "" {
		upstreamQuery.Set("limit", limit)
	}

	path := "/_matrix/client/v3/rooms/" + url.PathEscape(roomID) + "/relations/" + url.PathEscape(thread) + "/m.thread"
	if len(upstreamQuery) > 0 {
		path += "?" + upstreamQuery.Encode()
	}

	response, err := service.ForwardRequest(r.Context(), http.MethodGet, path, nil)
	if err != nil {
		h.logger.Error("matrix thread messages failed", "room", roomID, "thread", thread, "error", err)
		respondError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer response.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	h.copyResponseBody(w, response.Body, "thread_messages")
}

// HandleMatrixGetDisplayName handles GET /v1/matrix/display-name?user=<userId>.
// Fetches a user's display name via GET /_matrix/client/v3/profile/{userId}/displayname.
func (h *Handler) HandleMatrixGetDisplayName(w http.ResponseWriter, r *http.Request) {
	service := h.getMatrixService(w)
	if service == nil {
		return
	}

	userID := r.URL.Query().Get("user")
	if userID == "" {
		respondError(w, http.StatusBadRequest, "user query parameter is required")
		return
	}

	path := "/_matrix/client/v3/profile/" + url.PathEscape(userID) + "/displayname"
	response, err := service.ForwardRequest(r.Context(), http.MethodGet, path, nil)
	if err != nil {
		h.logger.Error("matrix get display name failed", "user", userID, "error", err)
		respondError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer response.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	h.copyResponseBody(w, response.Body, "get_display_name")
}

// MatrixCreateRoomRequest is the JSON body for POST /v1/matrix/room.
// Reuses the messaging.CreateRoomRequest type directly — the proxy forwards
// the entire request body to the homeserver's POST /createRoom endpoint.
type MatrixCreateRoomRequest = messaging.CreateRoomRequest

// HandleMatrixCreateRoom handles POST /v1/matrix/room. Creates a room via
// POST /_matrix/client/v3/createRoom. Requires the matrix/create-room grant.
func (h *Handler) HandleMatrixCreateRoom(w http.ResponseWriter, r *http.Request) {
	if !h.requireGrant(w, "matrix/create-room") {
		return
	}

	service := h.getMatrixService(w)
	if service == nil {
		return
	}

	// Read the request body and forward it directly to the homeserver.
	// The request body is the Matrix createRoom JSON (room name, alias, etc.).
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodySize))
	if err != nil {
		respondErrorf(w, http.StatusBadRequest, "reading request body: %v", err)
		return
	}

	h.logger.Info("matrix create room")

	response, err := service.ForwardRequest(r.Context(), http.MethodPost, "/_matrix/client/v3/createRoom", bytes.NewReader(body))
	if err != nil {
		h.logger.Error("matrix create room failed", "error", err)
		respondError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer response.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	h.copyResponseBody(w, response.Body, "create_room")
}

// MatrixJoinRequest is the JSON body for POST /v1/matrix/join.
type MatrixJoinRequest struct {
	// Room is the room ID or alias to join.
	Room string `json:"room"`
}

// HandleMatrixJoinRoom handles POST /v1/matrix/join. Joins a room via
// POST /_matrix/client/v3/join/{roomIdOrAlias}. Requires the matrix/join grant.
func (h *Handler) HandleMatrixJoinRoom(w http.ResponseWriter, r *http.Request) {
	if !h.requireGrant(w, "matrix/join") {
		return
	}

	service := h.getMatrixService(w)
	if service == nil {
		return
	}

	var request MatrixJoinRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		respondErrorf(w, http.StatusBadRequest, "invalid request body: %v", err)
		return
	}

	if request.Room == "" {
		respondError(w, http.StatusBadRequest, "room is required")
		return
	}

	path := "/_matrix/client/v3/join/" + url.PathEscape(request.Room)

	h.logger.Info("matrix join room", "room", request.Room)

	// The join endpoint requires a POST with an empty JSON body.
	response, err := service.ForwardRequest(r.Context(), http.MethodPost, path, bytes.NewReader([]byte("{}")))
	if err != nil {
		h.logger.Error("matrix join room failed", "room", request.Room, "error", err)
		respondError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer response.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	h.copyResponseBody(w, response.Body, "join_room")
}

// MatrixInviteRequest is the JSON body for POST /v1/matrix/invite.
type MatrixInviteRequest struct {
	// Room is the room ID to invite the user to. Aliases are resolved.
	Room string `json:"room"`
	// UserID is the Matrix user ID to invite.
	UserID string `json:"user_id"`
}

// HandleMatrixInviteUser handles POST /v1/matrix/invite. Invites a user to a
// room via POST /_matrix/client/v3/rooms/{roomId}/invite. Requires the
// matrix/invite grant.
func (h *Handler) HandleMatrixInviteUser(w http.ResponseWriter, r *http.Request) {
	if !h.requireGrant(w, "matrix/invite") {
		return
	}

	service := h.getMatrixService(w)
	if service == nil {
		return
	}

	var request MatrixInviteRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		respondErrorf(w, http.StatusBadRequest, "invalid request body: %v", err)
		return
	}

	if request.Room == "" || request.UserID == "" {
		respondError(w, http.StatusBadRequest, "room and user_id are required")
		return
	}

	roomID, err := h.resolveRoom(r, service, request.Room)
	if err != nil {
		h.logger.Error("matrix invite: room resolution failed", "room", request.Room, "error", err)
		respondError(w, http.StatusBadGateway, err.Error())
		return
	}

	// Parse and validate the user ID at the proxy boundary.
	inviteUserID, err := ref.ParseUserID(request.UserID)
	if err != nil {
		respondErrorf(w, http.StatusBadRequest, "invalid user_id: %v", err)
		return
	}

	// Build the invite request body for the homeserver.
	inviteBody, _ := json.Marshal(messaging.InviteRequest{UserID: inviteUserID})

	path := "/_matrix/client/v3/rooms/" + url.PathEscape(roomID) + "/invite"

	h.logger.Info("matrix invite user", "room", roomID, "user", request.UserID)

	response, err := service.ForwardRequest(r.Context(), http.MethodPost, path, bytes.NewReader(inviteBody))
	if err != nil {
		h.logger.Error("matrix invite failed", "room", roomID, "user", request.UserID, "error", err)
		respondError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer response.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	h.copyResponseBody(w, response.Body, "invite_user")
}

// HandleMatrixSync handles GET /v1/matrix/sync?since=...&timeout=...&filter=...
// Forwards to GET /_matrix/client/v3/sync. This is a long-poll endpoint: the
// homeserver holds the connection for up to timeout ms before returning empty.
// The proxy streams the response without buffering.
func (h *Handler) HandleMatrixSync(w http.ResponseWriter, r *http.Request) {
	service := h.getMatrixService(w)
	if service == nil {
		return
	}

	query := url.Values{}
	if since := r.URL.Query().Get("since"); since != "" {
		query.Set("since", since)
	}
	if timeout := r.URL.Query().Get("timeout"); timeout != "" {
		query.Set("timeout", timeout)
	}
	if filter := r.URL.Query().Get("filter"); filter != "" {
		query.Set("filter", filter)
	}

	path := "/_matrix/client/v3/sync"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}

	response, err := service.ForwardRequest(r.Context(), http.MethodGet, path, nil)
	if err != nil {
		h.logger.Error("matrix sync failed", "error", err)
		respondError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer response.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	h.copyResponseBody(w, response.Body, "sync")
}

// MatrixSendEventRequest is the JSON body for POST /v1/matrix/event.
type MatrixSendEventRequest struct {
	// Room is the room ID or alias.
	Room string `json:"room"`
	// EventType is the Matrix event type (e.g., "m.room.message",
	// "m.bureau.pipeline_request").
	EventType string `json:"event_type"`
	// Content is the event content, forwarded as-is to the homeserver.
	Content any `json:"content"`
}

// HandleMatrixSendEvent handles POST /v1/matrix/event. Sends an event of any
// type to a room via PUT /_matrix/client/v3/rooms/{roomId}/send/{eventType}/{txnId}.
// Transaction IDs are generated automatically.
func (h *Handler) HandleMatrixSendEvent(w http.ResponseWriter, r *http.Request) {
	service := h.getMatrixService(w)
	if service == nil {
		return
	}

	var request MatrixSendEventRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		respondErrorf(w, http.StatusBadRequest, "invalid request body: %v", err)
		return
	}

	if request.Room == "" || request.EventType == "" {
		respondError(w, http.StatusBadRequest, "room and event_type are required")
		return
	}

	roomID, err := h.resolveRoom(r, service, request.Room)
	if err != nil {
		h.logger.Error("matrix send event: room resolution failed", "room", request.Room, "error", err)
		respondError(w, http.StatusBadGateway, err.Error())
		return
	}

	contentBody, err := json.Marshal(request.Content)
	if err != nil {
		respondErrorf(w, http.StatusBadRequest, "invalid content: %v", err)
		return
	}

	transactionID := fmt.Sprintf("bureau_%d_%d", time.Now().UnixMilli(), matrixTransactionCounter.Add(1))
	path := "/_matrix/client/v3/rooms/" + url.PathEscape(roomID) + "/send/" + url.PathEscape(request.EventType) + "/" + url.PathEscape(transactionID)

	h.logger.Info("matrix send event",
		"room", roomID,
		"event_type", request.EventType,
		"txn_id", transactionID,
	)

	response, err := service.ForwardRequest(r.Context(), http.MethodPut, path, bytes.NewReader(contentBody))
	if err != nil {
		h.logger.Error("matrix send event failed", "room", roomID, "event_type", request.EventType, "error", err)
		respondError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer response.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	h.copyResponseBody(w, response.Body, "send_event")
}
