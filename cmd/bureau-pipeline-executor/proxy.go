// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/bureau-foundation/bureau/lib/netutil"
)

// proxyClient communicates with the Bureau proxy's structured /v1/matrix/*
// API over a Unix socket. All Matrix operations the executor needs go
// through this client — it never constructs Matrix API URLs directly.
type proxyClient struct {
	httpClient *http.Client
	serverName string // set by whoami()
}

func newProxyClient(socketPath string) *proxyClient {
	return &proxyClient{
		httpClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

// whoami calls GET /v1/matrix/whoami and extracts the server name from the
// user ID. This is called once at startup — the server name is needed for
// constructing room aliases when resolving pipeline refs.
func (p *proxyClient) whoami(ctx context.Context) error {
	response, err := p.get(ctx, "/v1/matrix/whoami")
	if err != nil {
		return fmt.Errorf("whoami: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("whoami: HTTP %d: %s", response.StatusCode, netutil.ErrorBody(response.Body))
	}

	var result struct {
		UserID string `json:"user_id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return fmt.Errorf("whoami: parsing response: %w", err)
	}
	if result.UserID == "" {
		return fmt.Errorf("whoami: empty user_id in response")
	}

	// Extract server name from @localpart:server.
	parts := strings.SplitN(result.UserID, ":", 2)
	if len(parts) != 2 {
		return fmt.Errorf("whoami: user_id %q has no server component", result.UserID)
	}
	p.serverName = parts[1]
	return nil
}

// resolveAlias calls GET /v1/matrix/resolve to resolve a room alias to a
// room ID.
func (p *proxyClient) resolveAlias(ctx context.Context, alias string) (string, error) {
	response, err := p.get(ctx, "/v1/matrix/resolve?alias="+url.QueryEscape(alias))
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
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("resolve alias %q: parsing response: %w", alias, err)
	}
	if result.RoomID == "" {
		return "", fmt.Errorf("resolve alias %q: empty room_id in response", alias)
	}
	return result.RoomID, nil
}

// getState calls GET /v1/matrix/state to read a state event from a room.
// Returns the event content as raw JSON.
func (p *proxyClient) getState(ctx context.Context, room, eventType, stateKey string) (json.RawMessage, error) {
	query := url.Values{
		"room": {room},
		"type": {eventType},
		"key":  {stateKey},
	}
	response, err := p.get(ctx, "/v1/matrix/state?"+query.Encode())
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

// putState calls POST /v1/matrix/state to publish a state event.
// Returns the event ID of the created event.
func (p *proxyClient) putState(ctx context.Context, room, eventType, stateKey string, content any) (string, error) {
	request := struct {
		Room      string `json:"room"`
		EventType string `json:"event_type"`
		StateKey  string `json:"state_key"`
		Content   any    `json:"content"`
	}{
		Room:      room,
		EventType: eventType,
		StateKey:  stateKey,
		Content:   content,
	}

	response, err := p.post(ctx, "/v1/matrix/state", request)
	if err != nil {
		return "", fmt.Errorf("put state %s/%s in %s: %w", eventType, stateKey, room, err)
	}
	defer response.Body.Close()

	body, _ := netutil.ReadResponse(response.Body)
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("put state %s/%s in %s: HTTP %d: %s", eventType, stateKey, room, response.StatusCode, body)
	}

	var result struct {
		EventID string `json:"event_id"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("put state: parsing response: %w", err)
	}
	return result.EventID, nil
}

// sendMessage calls POST /v1/matrix/message to send a message.
// Returns the event ID of the sent message.
func (p *proxyClient) sendMessage(ctx context.Context, room string, content any) (string, error) {
	request := struct {
		Room    string `json:"room"`
		Content any    `json:"content"`
	}{
		Room:    room,
		Content: content,
	}

	response, err := p.post(ctx, "/v1/matrix/message", request)
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

// get makes a GET request to the proxy.
func (p *proxyClient) get(ctx context.Context, path string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost"+path, nil)
	if err != nil {
		return nil, err
	}
	return p.httpClient.Do(request)
}

// post makes a POST request to the proxy with a JSON body.
func (p *proxyClient) post(ctx context.Context, path string, body any) (*http.Response, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encoding request body: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost"+path, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	return p.httpClient.Do(request)
}
