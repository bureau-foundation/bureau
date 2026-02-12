// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/bureau-foundation/bureau/lib/httpx"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
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
// levels are enforced on every forwarded request. The proxy adds no new
// policy logic beyond what checkMatrixPolicy already does for /http/matrix/.

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

// getMatrixService looks up the "matrix" HTTPService and returns it. If the
// service is not registered, writes a 503 error response and returns nil.
func (h *Handler) getMatrixService(w http.ResponseWriter) *HTTPService {
	h.mu.RLock()
	service := h.httpServices[matrixServiceName]
	h.mu.RUnlock()

	if service == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(matrixErrorResponse{
			Error: "matrix service not configured",
		})
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
		return "", fmt.Errorf("resolving alias %q: HTTP %d: %s", alias, response.StatusCode, httpx.ErrorBody(response.Body))
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

// HandleMatrixWhoami handles GET /v1/matrix/whoami. Returns the agent's
// Matrix user ID by forwarding to /_matrix/client/v3/account/whoami.
func (h *Handler) HandleMatrixWhoami(w http.ResponseWriter, r *http.Request) {
	service := h.getMatrixService(w)
	if service == nil {
		return
	}

	response, err := service.ForwardRequest(r.Context(), http.MethodGet, "/_matrix/client/v3/account/whoami", nil)
	if err != nil {
		h.logger.Error("matrix whoami failed", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(matrixErrorResponse{Error: err.Error()})
		return
	}
	defer response.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	io.Copy(w, response.Body)
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
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(matrixErrorResponse{Error: "alias query parameter is required"})
		return
	}

	path := "/_matrix/client/v3/directory/room/" + url.PathEscape(alias)
	response, err := service.ForwardRequest(r.Context(), http.MethodGet, path, nil)
	if err != nil {
		h.logger.Error("matrix resolve failed", "alias", alias, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(matrixErrorResponse{Error: err.Error()})
		return
	}
	defer response.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	io.Copy(w, response.Body)
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
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(matrixErrorResponse{
			Error: "room and type query parameters are required",
		})
		return
	}

	// Resolve alias if needed.
	roomID, err := h.resolveRoom(r, service, room)
	if err != nil {
		h.logger.Error("matrix get state: room resolution failed", "room", room, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(matrixErrorResponse{Error: err.Error()})
		return
	}

	path := "/_matrix/client/v3/rooms/" + url.PathEscape(roomID) + "/state/" + url.PathEscape(eventType) + "/" + url.PathEscape(stateKey)
	response, err := service.ForwardRequest(r.Context(), http.MethodGet, path, nil)
	if err != nil {
		h.logger.Error("matrix get state failed", "room", roomID, "type", eventType, "key", stateKey, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(matrixErrorResponse{Error: err.Error()})
		return
	}
	defer response.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	io.Copy(w, response.Body)
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
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(matrixErrorResponse{Error: fmt.Sprintf("invalid request body: %v", err)})
		return
	}

	if request.Room == "" || request.EventType == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(matrixErrorResponse{Error: "room and event_type are required"})
		return
	}

	// Resolve alias if needed.
	roomID, err := h.resolveRoom(r, service, request.Room)
	if err != nil {
		h.logger.Error("matrix put state: room resolution failed", "room", request.Room, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(matrixErrorResponse{Error: err.Error()})
		return
	}

	// Marshal just the content for the upstream PUT body.
	contentBody, err := json.Marshal(request.Content)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(matrixErrorResponse{Error: fmt.Sprintf("invalid content: %v", err)})
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
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(matrixErrorResponse{Error: err.Error()})
		return
	}
	defer response.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	io.Copy(w, response.Body)
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
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(matrixErrorResponse{Error: fmt.Sprintf("invalid request body: %v", err)})
		return
	}

	if request.Room == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(matrixErrorResponse{Error: "room is required"})
		return
	}

	// Resolve alias if needed.
	roomID, err := h.resolveRoom(r, service, request.Room)
	if err != nil {
		h.logger.Error("matrix send message: room resolution failed", "room", request.Room, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(matrixErrorResponse{Error: err.Error()})
		return
	}

	// Marshal the content for the upstream PUT body.
	contentBody, err := json.Marshal(request.Content)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(matrixErrorResponse{Error: fmt.Sprintf("invalid content: %v", err)})
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
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(matrixErrorResponse{Error: err.Error()})
		return
	}
	defer response.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	io.Copy(w, response.Body)
}
