// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/bureau-foundation/bureau/lib/codec"
)

// dialTimeout is the maximum time to wait for a connection to the
// service socket. This is separate from the server's read/write
// timeouts — it covers only the connect phase.
const dialTimeout = 5 * time.Second

// responseReadTimeout is how long the client waits for the server to
// send a response after writing the request. Matched to the server's
// readTimeout + writeTimeout to account for handler execution time.
const responseReadTimeout = 45 * time.Second

// maxResponseSize is the maximum size of a single CBOR response.
// Matches the server's maxRequestSize for symmetry.
const maxResponseSize = 1024 * 1024

// ServiceError is returned by Call when the server responds with
// ok=false. It wraps the server's error message and the action that
// failed.
type ServiceError struct {
	Action  string
	Message string
}

func (e *ServiceError) Error() string {
	return fmt.Sprintf("service error on %q: %s", e.Action, e.Message)
}

// ServiceClient sends CBOR requests to a Bureau service socket. Each
// Call opens a new connection (matching the server's one-request-per-
// connection model), sends the request, reads the response, and
// closes the connection.
//
// If the client was constructed with a token (via NewServiceClient or
// NewServiceClientFromToken), the token is included in every request
// as the "token" field. Unauthenticated clients (nil token) omit it.
type ServiceClient struct {
	socketPath string
	tokenBytes []byte
}

// NewServiceClient creates an authenticated client by reading the
// service token from tokenPath. The token file is the raw bytes
// written by the daemon at sandbox creation time (CBOR payload +
// Ed25519 signature). Returns an error if the file cannot be read
// or is empty.
func NewServiceClient(socketPath, tokenPath string) (*ServiceClient, error) {
	tokenBytes, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("reading service token from %s: %w", tokenPath, err)
	}
	if len(tokenBytes) == 0 {
		return nil, fmt.Errorf("service token file %s is empty", tokenPath)
	}
	return &ServiceClient{
		socketPath: socketPath,
		tokenBytes: tokenBytes,
	}, nil
}

// NewServiceClientFromToken creates a client with pre-loaded token
// bytes. If tokenBytes is nil, the client sends unauthenticated
// requests (suitable for health checks or testing unauthenticated
// endpoints). Useful in tests and for callers that manage token
// lifecycle themselves (e.g., re-reading on expiry).
func NewServiceClientFromToken(socketPath string, tokenBytes []byte) *ServiceClient {
	return &ServiceClient{
		socketPath: socketPath,
		tokenBytes: tokenBytes,
	}
}

// Call sends a CBOR request to the service and decodes the response.
//
// The fields parameter may contain any handler-specific request
// fields; the client adds "action" and "token" automatically. Pass
// nil for actions that take no additional parameters. The caller must
// not include "action" or "token" keys in the fields map.
//
// On success (response ok=true), if result is non-nil and the
// response contains data, the data is CBOR-decoded into result.
//
// On failure (response ok=false), returns a *ServiceError containing
// the server's error message. Connection and encoding errors are
// returned as plain errors (not *ServiceError).
func (c *ServiceClient) Call(ctx context.Context, action string, fields map[string]any, result any) error {
	request := c.buildRequest(action, fields)

	response, err := c.send(ctx, request)
	if err != nil {
		return fmt.Errorf("calling %q on %s: %w", action, c.socketPath, err)
	}

	if !response.OK {
		return &ServiceError{
			Action:  action,
			Message: response.Error,
		}
	}

	if result != nil && len(response.Data) > 0 {
		if err := codec.Unmarshal(response.Data, result); err != nil {
			return fmt.Errorf("decoding response data for %q: %w", action, err)
		}
	}

	return nil
}

// buildRequest constructs the CBOR request map. Starts with the
// caller's fields (if any), then injects "action" and optionally
// "token".
func (c *ServiceClient) buildRequest(action string, fields map[string]any) map[string]any {
	var request map[string]any
	if fields != nil {
		request = make(map[string]any, len(fields)+2)
		for key, value := range fields {
			request[key] = value
		}
	} else {
		request = make(map[string]any, 2)
	}

	request["action"] = action
	if c.tokenBytes != nil {
		request["token"] = c.tokenBytes
	}

	return request
}

// send connects to the socket, writes the request, and reads the
// response. Each call creates a new connection.
func (c *ServiceClient) send(ctx context.Context, request any) (*Response, error) {
	dialer := net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return nil, fmt.Errorf("connecting: %w", err)
	}
	defer conn.Close()

	// Write the request.
	if err := codec.NewEncoder(conn).Encode(request); err != nil {
		return nil, fmt.Errorf("writing request: %w", err)
	}

	// Half-close the write side. CBOR is self-delimiting so this
	// isn't strictly necessary, but it lets the server's read side
	// see EOF cleanly.
	if unixConn, ok := conn.(*net.UnixConn); ok {
		unixConn.CloseWrite()
	}

	// Read the response.
	conn.SetReadDeadline(time.Now().Add(responseReadTimeout))
	var response Response
	if err := codec.NewDecoder(io.LimitReader(conn, maxResponseSize)).Decode(&response); err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	return &response, nil
}
