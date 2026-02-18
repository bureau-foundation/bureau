// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package proxyclient provides a typed HTTP client for the Bureau proxy
// Unix socket API. Any code running inside a Bureau sandbox — agents,
// pipeline executors, service binaries — uses this client to communicate
// with the proxy's /v1/* endpoints.
//
// The client mirrors the proxy's wire format using its own response types,
// avoiding an import dependency from sandbox code back into the proxy
// implementation.
package proxyclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/bureau-foundation/bureau/lib/netutil"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// Client is a typed HTTP client for the Bureau proxy Unix socket API.
type Client struct {
	httpClient *http.Client
	serverName string
}

// New creates a Client that communicates with the proxy over the given
// Unix socket path. The serverName is used for constructing Matrix room
// aliases (e.g., "#bureau/config/machine/ws:serverName").
func New(socketPath string, serverName string) *Client {
	return &Client{
		httpClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
				},
			},
		},
		serverName: serverName,
	}
}

// NewForTesting creates a Client with a custom transport. This is used by
// tests that need to redirect requests to a httptest.Server instead of a
// Unix socket.
func NewForTesting(transport http.RoundTripper, serverName string) *Client {
	return &Client{
		httpClient: &http.Client{Transport: transport},
		serverName: serverName,
	}
}

// ServerName returns the Matrix server name this client was configured with.
func (client *Client) ServerName() string {
	return client.serverName
}

// HTTPClient returns the underlying HTTP client configured to dial the
// proxy Unix socket. Use this for /http/* routes (LLM API calls,
// service-to-service requests, raw Matrix API access).
func (client *Client) HTTPClient() *http.Client {
	return client.httpClient
}

// IdentityResponse is the wire format for GET /v1/identity.
type IdentityResponse struct {
	UserID        string `json:"user_id"`
	ServerName    string `json:"server_name,omitempty"`
	ObserveSocket string `json:"observe_socket,omitempty"`
}

// Identity returns the agent's identity as configured by the proxy.
func (client *Client) Identity(ctx context.Context) (*IdentityResponse, error) {
	response, err := client.get(ctx, "/v1/identity")
	if err != nil {
		return nil, fmt.Errorf("identity: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("identity: HTTP %d: %s", response.StatusCode, netutil.ErrorBody(response.Body))
	}

	var result IdentityResponse
	if err := netutil.DecodeResponse(response.Body, &result); err != nil {
		return nil, fmt.Errorf("identity: %w", err)
	}
	return &result, nil
}

// Grants returns the principal's pre-resolved authorization grants.
func (client *Client) Grants(ctx context.Context) ([]schema.Grant, error) {
	response, err := client.get(ctx, "/v1/grants")
	if err != nil {
		return nil, fmt.Errorf("grants: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("grants: HTTP %d: %s", response.StatusCode, netutil.ErrorBody(response.Body))
	}

	var result []schema.Grant
	if err := netutil.DecodeResponse(response.Body, &result); err != nil {
		return nil, fmt.Errorf("grants: %w", err)
	}
	return result, nil
}

// ServiceEntry is a single entry in the service directory, as returned
// by GET /v1/services.
type ServiceEntry struct {
	Localpart    string   `json:"localpart"`
	Principal    string   `json:"principal"`
	Machine      string   `json:"machine"`
	Protocol     string   `json:"protocol"`
	Description  string   `json:"description,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// Services returns the service directory visible to this principal.
func (client *Client) Services(ctx context.Context) ([]ServiceEntry, error) {
	response, err := client.get(ctx, "/v1/services")
	if err != nil {
		return nil, fmt.Errorf("services: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("services: HTTP %d: %s", response.StatusCode, netutil.ErrorBody(response.Body))
	}

	var result []ServiceEntry
	if err := netutil.DecodeResponse(response.Body, &result); err != nil {
		return nil, fmt.Errorf("services: %w", err)
	}
	return result, nil
}

// Whoami returns the principal's full Matrix user ID.
func (client *Client) Whoami(ctx context.Context) (string, error) {
	response, err := client.get(ctx, "/v1/matrix/whoami")
	if err != nil {
		return "", fmt.Errorf("whoami: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("whoami: HTTP %d: %s", response.StatusCode, netutil.ErrorBody(response.Body))
	}

	var result struct {
		UserID string `json:"user_id"`
	}
	if err := netutil.DecodeResponse(response.Body, &result); err != nil {
		return "", fmt.Errorf("whoami: %w", err)
	}
	if result.UserID == "" {
		return "", fmt.Errorf("whoami: empty user_id in response")
	}
	return result.UserID, nil
}

// WhoamiServerName calls Whoami and extracts the server name from the
// Matrix user ID. Convenience method for callers that need the server
// name but don't have it from configuration.
func (client *Client) WhoamiServerName(ctx context.Context) (string, error) {
	userID, err := client.Whoami(ctx)
	if err != nil {
		return "", err
	}
	parts := strings.SplitN(userID, ":", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("whoami: user_id %q has no server component", userID)
	}
	return parts[1], nil
}

// DiscoverServerName calls WhoamiServerName and stores the result so
// that subsequent calls to ServerName() return the discovered name.
// Use this when the server name is not known at construction time (e.g.,
// the pipeline executor discovers it via the proxy at startup).
func (client *Client) DiscoverServerName(ctx context.Context) (string, error) {
	serverName, err := client.WhoamiServerName(ctx)
	if err != nil {
		return "", err
	}
	client.serverName = serverName
	return serverName, nil
}

// ResolveAlias resolves a Matrix room alias to a room ID.
func (client *Client) ResolveAlias(ctx context.Context, alias string) (string, error) {
	response, err := client.get(ctx, "/v1/matrix/resolve?alias="+url.QueryEscape(alias))
	if err != nil {
		return "", fmt.Errorf("resolve alias %q: %w", alias, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("resolve alias %q: HTTP %d: %s", alias, response.StatusCode, netutil.ErrorBody(response.Body))
	}

	var result struct {
		RoomID string `json:"room_id"`
	}
	if err := netutil.DecodeResponse(response.Body, &result); err != nil {
		return "", fmt.Errorf("resolve alias %q: %w", alias, err)
	}
	if result.RoomID == "" {
		return "", fmt.Errorf("resolve alias %q: empty room_id in response", alias)
	}
	return result.RoomID, nil
}

// GetState retrieves a state event from a Matrix room. Returns the
// event content as raw JSON.
func (client *Client) GetState(ctx context.Context, room, eventType, stateKey string) (json.RawMessage, error) {
	query := url.Values{
		"room": {room},
		"type": {eventType},
		"key":  {stateKey},
	}
	response, err := client.get(ctx, "/v1/matrix/state?"+query.Encode())
	if err != nil {
		return nil, fmt.Errorf("get state %s/%s in %s: %w", eventType, stateKey, room, err)
	}
	defer response.Body.Close()

	body, err := netutil.ReadResponse(response.Body)
	if err != nil {
		return nil, fmt.Errorf("get state: reading response: %w", err)
	}

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get state %s/%s in %s: HTTP %d: %s", eventType, stateKey, room, response.StatusCode, body)
	}

	return json.RawMessage(body), nil
}

// PutStateRequest is the JSON body for POST /v1/matrix/state.
type PutStateRequest struct {
	Room      string `json:"room"`
	EventType string `json:"event_type"`
	StateKey  string `json:"state_key"`
	Content   any    `json:"content"`
}

// PutState publishes a state event to a Matrix room. Returns the event ID.
func (client *Client) PutState(ctx context.Context, request PutStateRequest) (string, error) {
	response, err := client.post(ctx, "/v1/matrix/state", request)
	if err != nil {
		return "", fmt.Errorf("put state %s/%s in %s: %w", request.EventType, request.StateKey, request.Room, err)
	}
	defer response.Body.Close()

	body, _ := netutil.ReadResponse(response.Body)
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("put state %s/%s in %s: HTTP %d: %s", request.EventType, request.StateKey, request.Room, response.StatusCode, body)
	}

	var result struct {
		EventID string `json:"event_id"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("put state: parsing response: %w", err)
	}
	return result.EventID, nil
}

// SendMessage sends a message to a Matrix room. Returns the event ID.
func (client *Client) SendMessage(ctx context.Context, room string, content any) (string, error) {
	request := struct {
		Room    string `json:"room"`
		Content any    `json:"content"`
	}{
		Room:    room,
		Content: content,
	}

	response, err := client.post(ctx, "/v1/matrix/message", request)
	if err != nil {
		return "", fmt.Errorf("send message to %s: %w", room, err)
	}
	defer response.Body.Close()

	body, _ := netutil.ReadResponse(response.Body)
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("send message to %s: HTTP %d: %s", room, response.StatusCode, body)
	}

	var result struct {
		EventID string `json:"event_id"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("send message: parsing response: %w", err)
	}
	return result.EventID, nil
}

// SendTextMessage is a convenience wrapper for plain text messages.
func (client *Client) SendTextMessage(ctx context.Context, room, text string) (string, error) {
	return client.SendMessage(ctx, room, map[string]string{
		"msgtype": "m.text",
		"body":    text,
	})
}

// JoinedRooms returns the list of room IDs the principal has joined.
func (client *Client) JoinedRooms(ctx context.Context) ([]string, error) {
	response, err := client.get(ctx, "/v1/matrix/joined-rooms")
	if err != nil {
		return nil, fmt.Errorf("joined rooms: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("joined rooms: HTTP %d: %s", response.StatusCode, netutil.ErrorBody(response.Body))
	}

	var result messaging.JoinedRoomsResponse
	if err := netutil.DecodeResponse(response.Body, &result); err != nil {
		return nil, fmt.Errorf("joined rooms: %w", err)
	}
	return result.JoinedRooms, nil
}

// GetRoomState returns all current state events from a room.
func (client *Client) GetRoomState(ctx context.Context, room string) ([]messaging.Event, error) {
	response, err := client.get(ctx, "/v1/matrix/room-state?room="+url.QueryEscape(room))
	if err != nil {
		return nil, fmt.Errorf("get room state for %s: %w", room, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get room state for %s: HTTP %d: %s", room, response.StatusCode, netutil.ErrorBody(response.Body))
	}

	var events []messaging.Event
	if err := netutil.DecodeResponse(response.Body, &events); err != nil {
		return nil, fmt.Errorf("get room state for %s: %w", room, err)
	}
	return events, nil
}

// GetRoomMembers returns the members of a room.
func (client *Client) GetRoomMembers(ctx context.Context, room string) ([]messaging.RoomMember, error) {
	response, err := client.get(ctx, "/v1/matrix/room-members?room="+url.QueryEscape(room))
	if err != nil {
		return nil, fmt.Errorf("get room members for %s: %w", room, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get room members for %s: HTTP %d: %s", room, response.StatusCode, netutil.ErrorBody(response.Body))
	}

	// The proxy forwards the homeserver's /members response, which contains
	// member state events. Parse them into RoomMember structs matching what
	// messaging.Session.GetRoomMembers returns.
	var membersResponse messaging.RoomMembersResponse
	if err := netutil.DecodeResponse(response.Body, &membersResponse); err != nil {
		return nil, fmt.Errorf("get room members for %s: %w", room, err)
	}

	members := make([]messaging.RoomMember, len(membersResponse.Chunk))
	for index, event := range membersResponse.Chunk {
		members[index] = messaging.RoomMember{
			UserID:      event.StateKey,
			DisplayName: event.Content.DisplayName,
			Membership:  event.Content.Membership,
			AvatarURL:   event.Content.AvatarURL,
		}
	}
	return members, nil
}

// RoomMessages fetches paginated messages from a room.
func (client *Client) RoomMessages(ctx context.Context, room string, options messaging.RoomMessagesOptions) (*messaging.RoomMessagesResponse, error) {
	query := url.Values{"room": {room}}
	if options.From != "" {
		query.Set("from", options.From)
	}
	direction := options.Direction
	if direction == "" {
		direction = "b"
	}
	query.Set("dir", direction)
	if options.Limit > 0 {
		query.Set("limit", strconv.Itoa(options.Limit))
	}

	response, err := client.get(ctx, "/v1/matrix/messages?"+query.Encode())
	if err != nil {
		return nil, fmt.Errorf("room messages for %s: %w", room, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("room messages for %s: HTTP %d: %s", room, response.StatusCode, netutil.ErrorBody(response.Body))
	}

	var result messaging.RoomMessagesResponse
	if err := netutil.DecodeResponse(response.Body, &result); err != nil {
		return nil, fmt.Errorf("room messages for %s: %w", room, err)
	}
	return &result, nil
}

// ThreadMessages fetches messages in a thread.
func (client *Client) ThreadMessages(ctx context.Context, room, threadRootID string, options messaging.ThreadMessagesOptions) (*messaging.ThreadMessagesResponse, error) {
	query := url.Values{
		"room":   {room},
		"thread": {threadRootID},
	}
	if options.From != "" {
		query.Set("from", options.From)
	}
	if options.Limit > 0 {
		query.Set("limit", strconv.Itoa(options.Limit))
	}

	response, err := client.get(ctx, "/v1/matrix/thread-messages?"+query.Encode())
	if err != nil {
		return nil, fmt.Errorf("thread messages for %s in %s: %w", threadRootID, room, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("thread messages for %s in %s: HTTP %d: %s", threadRootID, room, response.StatusCode, netutil.ErrorBody(response.Body))
	}

	var result messaging.ThreadMessagesResponse
	if err := netutil.DecodeResponse(response.Body, &result); err != nil {
		return nil, fmt.Errorf("thread messages for %s in %s: %w", threadRootID, room, err)
	}
	return &result, nil
}

// GetDisplayName fetches a user's display name.
func (client *Client) GetDisplayName(ctx context.Context, userID string) (string, error) {
	response, err := client.get(ctx, "/v1/matrix/display-name?user="+url.QueryEscape(userID))
	if err != nil {
		return "", fmt.Errorf("get display name for %s: %w", userID, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("get display name for %s: HTTP %d: %s", userID, response.StatusCode, netutil.ErrorBody(response.Body))
	}

	var result messaging.DisplayNameResponse
	if err := netutil.DecodeResponse(response.Body, &result); err != nil {
		return "", fmt.Errorf("get display name for %s: %w", userID, err)
	}
	return result.DisplayName, nil
}

// CreateRoom creates a new Matrix room. Returns the room ID.
func (client *Client) CreateRoom(ctx context.Context, request messaging.CreateRoomRequest) (*messaging.CreateRoomResponse, error) {
	response, err := client.post(ctx, "/v1/matrix/room", request)
	if err != nil {
		return nil, fmt.Errorf("create room: %w", err)
	}
	defer response.Body.Close()

	body, _ := netutil.ReadResponse(response.Body)
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("create room: HTTP %d: %s", response.StatusCode, body)
	}

	var result messaging.CreateRoomResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("create room: parsing response: %w", err)
	}
	return &result, nil
}

// JoinRoom joins a room by ID or alias. Returns the room ID.
func (client *Client) JoinRoom(ctx context.Context, roomIDOrAlias string) (string, error) {
	response, err := client.post(ctx, "/v1/matrix/join", struct {
		Room string `json:"room"`
	}{Room: roomIDOrAlias})
	if err != nil {
		return "", fmt.Errorf("join room %q: %w", roomIDOrAlias, err)
	}
	defer response.Body.Close()

	body, _ := netutil.ReadResponse(response.Body)
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("join room %q: HTTP %d: %s", roomIDOrAlias, response.StatusCode, body)
	}

	var result struct {
		RoomID string `json:"room_id"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("join room: parsing response: %w", err)
	}
	return result.RoomID, nil
}

// InviteUser invites a user to a room.
func (client *Client) InviteUser(ctx context.Context, roomID, userID string) error {
	response, err := client.post(ctx, "/v1/matrix/invite", struct {
		Room   string `json:"room"`
		UserID string `json:"user_id"`
	}{Room: roomID, UserID: userID})
	if err != nil {
		return fmt.Errorf("invite %q to %q: %w", userID, roomID, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("invite %q to %q: HTTP %d: %s", userID, roomID, response.StatusCode, netutil.ErrorBody(response.Body))
	}
	return nil
}

// SendEvent sends an event of any type to a room. Returns the event ID.
func (client *Client) SendEvent(ctx context.Context, room, eventType string, content any) (string, error) {
	response, err := client.post(ctx, "/v1/matrix/event", struct {
		Room      string `json:"room"`
		EventType string `json:"event_type"`
		Content   any    `json:"content"`
	}{Room: room, EventType: eventType, Content: content})
	if err != nil {
		return "", fmt.Errorf("send event %s to %s: %w", eventType, room, err)
	}
	defer response.Body.Close()

	body, _ := netutil.ReadResponse(response.Body)
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("send event %s to %s: HTTP %d: %s", eventType, room, response.StatusCode, body)
	}

	var result struct {
		EventID string `json:"event_id"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("send event: parsing response: %w", err)
	}
	return result.EventID, nil
}

// Sync performs a Matrix /sync through the proxy's HTTP passthrough.
// The proxy injects the principal's access token and forwards the request
// to the homeserver. For initial sync, leave options.Since empty and set
// Timeout to 0. For long-polling, set Timeout to 30000 (30 seconds).
func (client *Client) Sync(ctx context.Context, options messaging.SyncOptions) (*messaging.SyncResponse, error) {
	query := url.Values{}
	if options.Since != "" {
		query.Set("since", options.Since)
	}
	if options.SetTimeout || options.Timeout > 0 {
		query.Set("timeout", strconv.Itoa(options.Timeout))
	}
	if options.Filter != "" {
		query.Set("filter", options.Filter)
	}

	requestURL := "http://proxy/http/matrix/_matrix/client/v3/sync?" + query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("sync: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sync: HTTP %d: %s", response.StatusCode, netutil.ErrorBody(response.Body))
	}

	var result messaging.SyncResponse
	if err := netutil.DecodeResponse(response.Body, &result); err != nil {
		return nil, fmt.Errorf("sync: %w", err)
	}
	return &result, nil
}

// get makes a GET request to the proxy.
func (client *Client) get(ctx context.Context, path string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://proxy"+path, nil)
	if err != nil {
		return nil, err
	}
	return client.httpClient.Do(request)
}

// post makes a POST request to the proxy with a JSON body.
func (client *Client) post(ctx context.Context, path string, body any) (*http.Response, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encoding request body: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://proxy"+path, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	return client.httpClient.Do(request)
}
