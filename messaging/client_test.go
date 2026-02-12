// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package messaging

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bureau-foundation/bureau/lib/secret"
)

// testBuffer creates a secret.Buffer from a string for testing. The buffer
// is automatically closed when the test completes.
func testBuffer(t *testing.T, value string) *secret.Buffer {
	t.Helper()
	buffer, err := secret.NewFromString(value)
	if err != nil {
		t.Fatalf("creating test buffer: %v", err)
	}
	t.Cleanup(func() { buffer.Close() })
	return buffer
}

func TestNewClient(t *testing.T) {
	t.Run("valid URL", func(t *testing.T) {
		client, err := NewClient(ClientConfig{HomeserverURL: "http://localhost:6167"})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}
		if client == nil {
			t.Fatal("NewClient returned nil")
		}
	})

	t.Run("empty URL", func(t *testing.T) {
		_, err := NewClient(ClientConfig{})
		if err == nil {
			t.Fatal("expected error for empty URL")
		}
	})

	t.Run("invalid URL", func(t *testing.T) {
		_, err := NewClient(ClientConfig{HomeserverURL: "://invalid"})
		if err == nil {
			t.Fatal("expected error for invalid URL")
		}
	})
}

func TestRegister(t *testing.T) {
	t.Run("successful registration with UIAA", func(t *testing.T) {
		callCount := 0
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Path != "/_matrix/client/v3/register" {
				t.Errorf("unexpected path: %s", request.URL.Path)
				writer.WriteHeader(http.StatusNotFound)
				return
			}

			callCount++
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatalf("failed to decode request body: %v", err)
			}

			if callCount == 1 {
				// First request: return 401 with UIAA session.
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(writer).Encode(map[string]any{
					"session": "test-session-123",
					"flows": []map[string]any{
						{"stages": []string{"m.login.registration_token"}},
					},
				})
				return
			}

			// Second request: verify auth and return success.
			auth, ok := body["auth"].(map[string]any)
			if !ok {
				t.Fatal("second request missing auth")
			}
			if auth["type"] != "m.login.registration_token" {
				t.Errorf("unexpected auth type: %v", auth["type"])
			}
			if auth["token"] != "test-reg-token" {
				t.Errorf("unexpected registration token: %v", auth["token"])
			}
			if auth["session"] != "test-session-123" {
				t.Errorf("unexpected session: %v", auth["session"])
			}
			if body["username"] != "alice" {
				t.Errorf("unexpected username: %v", body["username"])
			}

			writer.Header().Set("Content-Type", "application/json")
			json.NewEncoder(writer).Encode(AuthResponse{
				UserID:      "@alice:test.local",
				AccessToken: "syt_alice_token",
				DeviceID:    "DEVICE1",
			})
		}))
		defer server.Close()

		client, err := NewClient(ClientConfig{HomeserverURL: server.URL})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		session, err := client.Register(context.Background(), RegisterRequest{
			Username:          "alice",
			Password:          testBuffer(t, "password123"),
			RegistrationToken: testBuffer(t, "test-reg-token"),
		})
		if err != nil {
			t.Fatalf("Register failed: %v", err)
		}
		defer session.Close()

		if session.UserID() != "@alice:test.local" {
			t.Errorf("unexpected user ID: %s", session.UserID())
		}
		if session.AccessToken() != "syt_alice_token" {
			t.Errorf("unexpected access token: %s", session.AccessToken())
		}
		if session.DeviceID() != "DEVICE1" {
			t.Errorf("unexpected device ID: %s", session.DeviceID())
		}
		if callCount != 2 {
			t.Errorf("expected 2 requests (UIAA flow), got %d", callCount)
		}
	})

	t.Run("user already exists", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(writer).Encode(MatrixError{
				Code:    ErrCodeUserInUse,
				Message: "User ID already taken.",
			})
		}))
		defer server.Close()

		client, err := NewClient(ClientConfig{HomeserverURL: server.URL})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		_, err = client.Register(context.Background(), RegisterRequest{
			Username:          "alice",
			Password:          testBuffer(t, "password123"),
			RegistrationToken: testBuffer(t, "test-reg-token"),
		})
		if err == nil {
			t.Fatal("expected error for existing user")
		}
		if !IsMatrixError(err, ErrCodeUserInUse) {
			t.Errorf("expected M_USER_IN_USE error, got: %v", err)
		}
	})

	t.Run("validation errors", func(t *testing.T) {
		client, _ := NewClient(ClientConfig{HomeserverURL: "http://localhost:1"})

		_, err := client.Register(context.Background(), RegisterRequest{})
		if err == nil {
			t.Fatal("expected error for empty username")
		}

		_, err = client.Register(context.Background(), RegisterRequest{Username: "alice"})
		if err == nil {
			t.Fatal("expected error for nil password")
		}
	})
}

func TestLogin(t *testing.T) {
	t.Run("successful login", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Path != "/_matrix/client/v3/login" {
				t.Errorf("unexpected path: %s", request.URL.Path)
				writer.WriteHeader(http.StatusNotFound)
				return
			}

			var body LoginRequest
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatalf("failed to decode request body: %v", err)
			}
			if body.Type != "m.login.password" {
				t.Errorf("unexpected login type: %s", body.Type)
			}
			if body.User != "bob" {
				t.Errorf("unexpected username: %s", body.User)
			}

			writer.Header().Set("Content-Type", "application/json")
			json.NewEncoder(writer).Encode(AuthResponse{
				UserID:      "@bob:test.local",
				AccessToken: "syt_bob_token",
				DeviceID:    "DEVICE2",
			})
		}))
		defer server.Close()

		client, err := NewClient(ClientConfig{HomeserverURL: server.URL})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		session, err := client.Login(context.Background(), "bob", testBuffer(t, "secret"))
		if err != nil {
			t.Fatalf("Login failed: %v", err)
		}
		defer session.Close()

		if session.UserID() != "@bob:test.local" {
			t.Errorf("unexpected user ID: %s", session.UserID())
		}
		if session.AccessToken() != "syt_bob_token" {
			t.Errorf("unexpected access token: %s", session.AccessToken())
		}
	})

	t.Run("invalid credentials", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusForbidden)
			json.NewEncoder(writer).Encode(MatrixError{
				Code:    ErrCodeForbidden,
				Message: "Invalid password",
			})
		}))
		defer server.Close()

		client, err := NewClient(ClientConfig{HomeserverURL: server.URL})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		_, err = client.Login(context.Background(), "bob", testBuffer(t, "wrong"))
		if err == nil {
			t.Fatal("expected error for invalid credentials")
		}
		if !IsMatrixError(err, ErrCodeForbidden) {
			t.Errorf("expected M_FORBIDDEN error, got: %v", err)
		}
	})

	t.Run("validation errors", func(t *testing.T) {
		client, _ := NewClient(ClientConfig{HomeserverURL: "http://localhost:1"})

		_, err := client.Login(context.Background(), "", testBuffer(t, "password"))
		if err == nil {
			t.Fatal("expected error for empty username")
		}

		_, err = client.Login(context.Background(), "alice", nil)
		if err == nil {
			t.Fatal("expected error for nil password")
		}
	})
}

func TestSessionFromToken(t *testing.T) {
	client, err := NewClient(ClientConfig{HomeserverURL: "http://localhost:1"})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	session, err := client.SessionFromToken("@alice:test.local", "syt_token")
	if err != nil {
		t.Fatalf("SessionFromToken failed: %v", err)
	}
	defer session.Close()

	if session.UserID() != "@alice:test.local" {
		t.Errorf("unexpected user ID: %s", session.UserID())
	}
	if session.AccessToken() != "syt_token" {
		t.Errorf("unexpected access token: %s", session.AccessToken())
	}
	// DeviceID is empty when created from token (not from login/register).
	if session.DeviceID() != "" {
		t.Errorf("expected empty device ID, got: %s", session.DeviceID())
	}
}

func TestMatrixError(t *testing.T) {
	t.Run("error message format", func(t *testing.T) {
		err := &MatrixError{
			Code:       ErrCodeForbidden,
			Message:    "Access denied",
			StatusCode: 403,
		}
		expected := "matrix: M_FORBIDDEN (403): Access denied"
		if err.Error() != expected {
			t.Errorf("unexpected error message: %s", err.Error())
		}
	})

	t.Run("IsMatrixError", func(t *testing.T) {
		err := &MatrixError{Code: ErrCodeNotFound, Message: "not found", StatusCode: 404}
		if !IsMatrixError(err, ErrCodeNotFound) {
			t.Error("IsMatrixError should match M_NOT_FOUND")
		}
		if IsMatrixError(err, ErrCodeForbidden) {
			t.Error("IsMatrixError should not match M_FORBIDDEN")
		}
	})

	t.Run("non-matrix error returns false", func(t *testing.T) {
		err := context.Canceled
		if IsMatrixError(err, ErrCodeNotFound) {
			t.Error("IsMatrixError should return false for non-matrix errors")
		}
	})
}
