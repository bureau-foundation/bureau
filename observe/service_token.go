// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// ServiceTokenResponse is the daemon's response to a mint_service_token
// request. On success, contains a signed service token and the socket
// path for the service. On failure, OK is false and Error describes
// the problem.
type ServiceTokenResponse struct {
	// OK is true if the token was minted successfully.
	OK bool `json:"ok"`

	// Error describes why the request failed (empty on success).
	Error string `json:"error,omitempty"`

	// Token is the base64-encoded signed service token bytes.
	// Decode with base64.StdEncoding to get the raw CBOR+Ed25519
	// token suitable for passing to service.NewServiceClientFromToken.
	Token string `json:"token,omitempty"`

	// SocketPath is the host-side Unix socket path for the service.
	// The operator connects to this path using a service client.
	SocketPath string `json:"socket_path,omitempty"`

	// TTLSeconds is the token lifetime in seconds. The caller should
	// refresh before this expires (recommend 80% of TTL).
	TTLSeconds int `json:"ttl_seconds,omitempty"`

	// ExpiresAt is the Unix timestamp when the token expires.
	ExpiresAt int64 `json:"expires_at,omitempty"`
}

// TokenBytes decodes the base64-encoded token string and returns
// the raw service token bytes. Returns an error if the token field
// is empty or not valid base64.
func (r *ServiceTokenResponse) TokenBytes() ([]byte, error) {
	if r.Token == "" {
		return nil, fmt.Errorf("empty token in service token response")
	}
	return base64.StdEncoding.DecodeString(r.Token)
}

// MintServiceToken connects to the daemon's observe socket and requests
// a signed service token for the given service role. The observer's
// Matrix access token is used for authentication; the daemon looks up
// the observer's grants, filters them for the service namespace, and
// mints a signed CBOR token.
//
// The response includes the service's socket path for direct connection
// and the token's TTL for refresh scheduling.
func MintServiceToken(daemonSocket string, serviceRole string, observerUserID string, observerToken string) (*ServiceTokenResponse, error) {
	connection, err := net.DialTimeout("unix", daemonSocket, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial daemon observe socket: %w", err)
	}
	defer connection.Close()

	connection.SetDeadline(time.Now().Add(10 * time.Second)) //nolint:realclock // kernel I/O deadline

	request := map[string]any{
		"action":       "mint_service_token",
		"service_role": serviceRole,
		"observer":     observerUserID,
		"token":        observerToken,
	}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return nil, fmt.Errorf("send mint_service_token request: %w", err)
	}

	reader := bufio.NewReader(connection)
	responseLine, err := reader.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read mint_service_token response: %w", err)
	}

	var response ServiceTokenResponse
	if err := json.Unmarshal(responseLine, &response); err != nil {
		return nil, fmt.Errorf("unmarshal mint_service_token response: %w", err)
	}

	if !response.OK {
		return nil, fmt.Errorf("mint_service_token failed: %s", response.Error)
	}

	return &response, nil
}
