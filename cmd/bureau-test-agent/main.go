// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// bureau-test-agent exercises the full Bureau stack from inside a sandbox.
// It validates proxy identity, Matrix authentication, room discovery, and
// bidirectional messaging — the minimal set of operations that prove a
// sandbox is fully functional.
//
// Environment variables (set via template variable expansion):
//
//	BUREAU_PROXY_SOCKET  — Unix socket path to the proxy
//	BUREAU_MACHINE_NAME  — machine localpart (e.g., "machine/workstation")
//	BUREAU_SERVER_NAME   — Matrix server name (e.g., "bureau.local")
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "TEST_AGENT: FATAL: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Read required environment variables.
	proxySocket := os.Getenv("BUREAU_PROXY_SOCKET")
	if proxySocket == "" {
		return fmt.Errorf("BUREAU_PROXY_SOCKET not set")
	}
	machineName := os.Getenv("BUREAU_MACHINE_NAME")
	if machineName == "" {
		return fmt.Errorf("BUREAU_MACHINE_NAME not set")
	}
	serverName := os.Getenv("BUREAU_SERVER_NAME")
	if serverName == "" {
		return fmt.Errorf("BUREAU_SERVER_NAME not set")
	}

	client := proxyHTTPClient(proxySocket)

	// Step 1: Verify proxy identity.
	log("checking proxy identity...")
	identity, err := getIdentity(ctx, client)
	if err != nil {
		return fmt.Errorf("identity check: %w", err)
	}
	if identity.UserID == "" {
		return fmt.Errorf("identity returned empty user_id")
	}
	log("identity: user_id=%s", identity.UserID)

	// Step 2: Verify Matrix authentication.
	log("checking Matrix whoami...")
	whoamiUserID, err := matrixWhoami(ctx, client)
	if err != nil {
		return fmt.Errorf("whoami: %w", err)
	}
	log("whoami: user_id=%s", whoamiUserID)

	// Step 3: Resolve the config room.
	configAlias := fmt.Sprintf("#bureau/config/%s:%s", machineName, serverName)
	log("resolving config room %s...", configAlias)
	configRoomID, err := matrixResolve(ctx, client, configAlias)
	if err != nil {
		return fmt.Errorf("resolve config room %s: %w", configAlias, err)
	}
	log("config room: %s", configRoomID)

	// Step 4: Send "quickstart-test-ready" to the config room.
	log("sending ready signal...")
	if err := matrixSendMessage(ctx, client, configRoomID, "quickstart-test-ready"); err != nil {
		return fmt.Errorf("send ready message: %w", err)
	}
	log("ready signal sent")

	// Step 5: Poll for an incoming message from another user.
	log("waiting for incoming message...")
	message, err := waitForMessage(ctx, client, configRoomID, whoamiUserID)
	if err != nil {
		return fmt.Errorf("wait for message: %w", err)
	}
	log("received: %q", message)

	// Step 6: Acknowledge the received message.
	ackBody := fmt.Sprintf("quickstart-test-ok: received '%s'", message)
	log("sending acknowledgment...")
	if err := matrixSendMessage(ctx, client, configRoomID, ackBody); err != nil {
		return fmt.Errorf("send ack message: %w", err)
	}
	log("acknowledgment sent")

	log("all checks passed")
	return nil
}

func log(format string, args ...any) {
	fmt.Fprintf(os.Stdout, "TEST_AGENT: "+format+"\n", args...)
}

// --- Proxy HTTP client ---

func proxyHTTPClient(socketPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
		Timeout: 30 * time.Second,
	}
}

// --- Proxy API calls ---

type identityResponse struct {
	UserID     string `json:"user_id"`
	ServerName string `json:"server_name"`
}

func getIdentity(ctx context.Context, client *http.Client) (*identityResponse, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://proxy/v1/identity", nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		return nil, fmt.Errorf("HTTP %d: %s", response.StatusCode, body)
	}
	var result identityResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode identity: %w", err)
	}
	return &result, nil
}

func matrixWhoami(ctx context.Context, client *http.Client) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://proxy/v1/matrix/whoami", nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		return "", fmt.Errorf("HTTP %d: %s", response.StatusCode, body)
	}
	var result struct {
		UserID string `json:"user_id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode whoami: %w", err)
	}
	return result.UserID, nil
}

func matrixResolve(ctx context.Context, client *http.Client, alias string) (string, error) {
	requestURL := "http://proxy/v1/matrix/resolve?alias=" + url.QueryEscape(alias)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		return "", fmt.Errorf("HTTP %d: %s", response.StatusCode, body)
	}
	var result struct {
		RoomID string `json:"room_id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode resolve: %w", err)
	}
	if result.RoomID == "" {
		return "", fmt.Errorf("resolve returned empty room_id")
	}
	return result.RoomID, nil
}

func matrixSendMessage(ctx context.Context, client *http.Client, roomID, body string) error {
	payload := map[string]any{
		"room": roomID,
		"content": map[string]string{
			"msgtype": "m.text",
			"body":    body,
		},
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://proxy/v1/matrix/message", strings.NewReader(string(payloadJSON)))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(response.Body)
		return fmt.Errorf("HTTP %d: %s", response.StatusCode, responseBody)
	}
	return nil
}

// waitForMessage polls the room's timeline via the proxy's raw Matrix HTTP
// endpoint, looking for a message from someone other than ourselves. Returns
// the message body when found.
func waitForMessage(ctx context.Context, client *http.Client, roomID, ownUserID string) (string, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("timed out waiting for message in room %s", roomID)
		case <-ticker.C:
			body, found, err := pollForMessage(ctx, client, roomID, ownUserID)
			if err != nil {
				// Log but keep trying — the room might not be synced yet.
				log("poll error (retrying): %v", err)
				continue
			}
			if found {
				return body, nil
			}
		}
	}
}

func pollForMessage(ctx context.Context, client *http.Client, roomID, ownUserID string) (string, bool, error) {
	requestURL := fmt.Sprintf(
		"http://proxy/http/matrix/_matrix/client/v3/rooms/%s/messages?dir=b&limit=20",
		url.PathEscape(roomID))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return "", false, err
	}
	response, err := client.Do(request)
	if err != nil {
		return "", false, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(response.Body)
		return "", false, fmt.Errorf("HTTP %d: %s", response.StatusCode, responseBody)
	}

	var messagesResponse struct {
		Chunk []struct {
			Type    string         `json:"type"`
			Sender  string         `json:"sender"`
			Content map[string]any `json:"content"`
		} `json:"chunk"`
	}
	if err := json.NewDecoder(response.Body).Decode(&messagesResponse); err != nil {
		return "", false, fmt.Errorf("decode messages: %w", err)
	}

	for _, event := range messagesResponse.Chunk {
		if event.Type != "m.room.message" {
			continue
		}
		// Skip our own messages.
		if event.Sender == ownUserID {
			continue
		}
		msgtype, _ := event.Content["msgtype"].(string)
		if msgtype != "m.text" {
			continue
		}
		body, _ := event.Content["body"].(string)
		if body != "" {
			return body, true, nil
		}
	}

	return "", false, nil
}
