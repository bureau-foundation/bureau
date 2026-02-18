// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package matrix

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bureau-foundation/bureau/messaging"
)

func TestUserCreate_MissingUsername(t *testing.T) {
	command := userCreateCommand()
	err := command.Execute([]string{
		"--credential-file", "/dev/null",
	})
	if err == nil {
		t.Fatal("expected error for missing username")
	}
	if !strings.Contains(err.Error(), "username is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestUserCreate_TooManyArgs(t *testing.T) {
	command := userCreateCommand()
	err := command.Execute([]string{
		"--credential-file", "/dev/null",
		"alice", "extra",
	})
	if err == nil {
		t.Fatal("expected error for extra argument")
	}
}

func TestUserCreate_OperatorRequiresCredentialFile(t *testing.T) {
	command := userCreateCommand()
	err := command.Execute([]string{
		"--operator",
		"--registration-token-file", "/dev/null",
		"alice",
	})
	if err == nil {
		t.Fatal("expected error for --operator without --credential-file")
	}
	if !strings.Contains(err.Error(), "--operator requires --credential-file") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestUserCreate_Operator_NewUser(t *testing.T) {
	var (
		mutex          sync.Mutex
		gotRegister    bool
		invitedRoomIDs []string
		joinedRoomIDs  []string
	)

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")

		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/_matrix/client/v3/register":
			mutex.Lock()
			gotRegister = true
			mutex.Unlock()
			json.NewEncoder(writer).Encode(map[string]any{
				"user_id":      "@alice:bureau.local",
				"access_token": "new-token",
			})

		case request.Method == http.MethodPost && strings.Contains(request.URL.Path, "/invite"):
			parts := strings.Split(request.URL.Path, "/rooms/")
			if len(parts) == 2 {
				roomPart := strings.TrimSuffix(parts[1], "/invite")
				mutex.Lock()
				invitedRoomIDs = append(invitedRoomIDs, roomPart)
				mutex.Unlock()
			}
			json.NewEncoder(writer).Encode(struct{}{})

		case request.Method == http.MethodPost && strings.Contains(request.URL.Path, "/join"):
			parts := strings.Split(request.URL.Path, "/join/")
			if len(parts) == 2 {
				mutex.Lock()
				joinedRoomIDs = append(joinedRoomIDs, parts[1])
				mutex.Unlock()
			}
			json.NewEncoder(writer).Encode(map[string]any{"room_id": "!room:bureau.local"})

		default:
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
			http.Error(writer, `{"errcode":"M_UNKNOWN"}`, http.StatusNotFound)
		}
	}))
	defer server.Close()

	credentialFile := writeTestCredentials(t, server.URL)

	command := userCreateCommand()
	err := command.Execute([]string{
		"--credential-file", credentialFile,
		"--operator",
		"alice",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mutex.Lock()
	defer mutex.Unlock()

	if !gotRegister {
		t.Error("register endpoint was not called")
	}
	expectedRoomCount := 1 + len(standardRooms) // space + all standard rooms
	if len(invitedRoomIDs) != expectedRoomCount {
		t.Errorf("expected %d room invites, got %d: %v", expectedRoomCount, len(invitedRoomIDs), invitedRoomIDs)
	}
	if len(joinedRoomIDs) != expectedRoomCount {
		t.Errorf("expected %d room joins, got %d: %v", expectedRoomCount, len(joinedRoomIDs), joinedRoomIDs)
	}

	expectedRooms := map[string]bool{
		"!space:bureau.local":    false,
		"!system:bureau.local":   false,
		"!template:bureau.local": false,
		"!pipeline:bureau.local": false,
		"!artifact:bureau.local": false,
	}
	for _, roomID := range invitedRoomIDs {
		if _, ok := expectedRooms[roomID]; !ok {
			t.Errorf("unexpected room invite: %s", roomID)
		} else {
			expectedRooms[roomID] = true
		}
	}
	for roomID, invited := range expectedRooms {
		if !invited {
			t.Errorf("missing invite to %s", roomID)
		}
	}
}

// TestUserCreate_Operator_ExistingUser tests the idempotent path where the
// account already exists (derived password, no --password-file). The code
// logs in with the derived password and proceeds to invite + join.
func TestUserCreate_Operator_ExistingUser(t *testing.T) {
	var (
		mutex       sync.Mutex
		gotLogin    bool
		inviteCount int
		joinCount   int
	)

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")

		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/_matrix/client/v3/register":
			writer.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(writer).Encode(messaging.MatrixError{
				Code:    messaging.ErrCodeUserInUse,
				Message: "User ID already taken",
			})

		case request.Method == http.MethodPost && request.URL.Path == "/_matrix/client/v3/login":
			mutex.Lock()
			gotLogin = true
			mutex.Unlock()
			json.NewEncoder(writer).Encode(map[string]any{
				"user_id":      "@alice:bureau.local",
				"access_token": "existing-token",
			})

		case request.Method == http.MethodPost && strings.Contains(request.URL.Path, "/invite"):
			mutex.Lock()
			inviteCount++
			mutex.Unlock()
			json.NewEncoder(writer).Encode(struct{}{})

		case request.Method == http.MethodPost && strings.Contains(request.URL.Path, "/join"):
			mutex.Lock()
			joinCount++
			mutex.Unlock()
			json.NewEncoder(writer).Encode(map[string]any{"room_id": "!room:bureau.local"})

		default:
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
			http.Error(writer, `{"errcode":"M_UNKNOWN"}`, http.StatusNotFound)
		}
	}))
	defer server.Close()

	credentialFile := writeTestCredentials(t, server.URL)

	command := userCreateCommand()
	err := command.Execute([]string{
		"--credential-file", credentialFile,
		"--operator",
		"alice",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mutex.Lock()
	defer mutex.Unlock()

	if !gotLogin {
		t.Error("login was not called for existing account")
	}
	expectedRoomCount := 1 + len(standardRooms) // space + all standard rooms
	if inviteCount != expectedRoomCount {
		t.Errorf("expected %d invites, got %d", expectedRoomCount, inviteCount)
	}
	if joinCount != expectedRoomCount {
		t.Errorf("expected %d joins, got %d", expectedRoomCount, joinCount)
	}
}

// TestUserCreate_Operator_AlreadyMember tests re-running --operator when the
// user is already a full member of all rooms. Invite returns M_FORBIDDEN
// (already member), join is a no-op.
func TestUserCreate_Operator_AlreadyMember(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")

		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/_matrix/client/v3/register":
			writer.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(writer).Encode(messaging.MatrixError{
				Code:    messaging.ErrCodeUserInUse,
				Message: "User ID already taken",
			})

		case request.Method == http.MethodPost && request.URL.Path == "/_matrix/client/v3/login":
			json.NewEncoder(writer).Encode(map[string]any{
				"user_id":      "@alice:bureau.local",
				"access_token": "existing-token",
			})

		case request.Method == http.MethodPost && strings.Contains(request.URL.Path, "/invite"):
			writer.WriteHeader(http.StatusForbidden)
			json.NewEncoder(writer).Encode(messaging.MatrixError{
				Code:    messaging.ErrCodeForbidden,
				Message: "User is already in the room",
			})

		case request.Method == http.MethodPost && strings.Contains(request.URL.Path, "/join"):
			// Already a member — join is a no-op, returns room ID.
			json.NewEncoder(writer).Encode(map[string]any{"room_id": "!room:bureau.local"})

		default:
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
			http.Error(writer, `{"errcode":"M_UNKNOWN"}`, http.StatusNotFound)
		}
	}))
	defer server.Close()

	credentialFile := writeTestCredentials(t, server.URL)

	command := userCreateCommand()
	err := command.Execute([]string{
		"--credential-file", credentialFile,
		"--operator",
		"alice",
	})
	if err != nil {
		t.Fatalf("expected success when user is already a member everywhere, got: %v", err)
	}
}

func TestUserCreate_NonOperator_UserExists_Fails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")

		if request.Method == http.MethodPost && request.URL.Path == "/_matrix/client/v3/register" {
			writer.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(writer).Encode(messaging.MatrixError{
				Code:    messaging.ErrCodeUserInUse,
				Message: "User ID already taken",
			})
			return
		}
		t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
	}))
	defer server.Close()

	credentialFile := writeTestCredentials(t, server.URL)

	command := userCreateCommand()
	err := command.Execute([]string{
		"--credential-file", credentialFile,
		"alice",
	})
	if err == nil {
		t.Fatal("expected error for existing user without --operator")
	}
	if !strings.Contains(err.Error(), "register user") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestUserCreate_NonOperator_NoInvites(t *testing.T) {
	var gotInvite bool

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")

		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/_matrix/client/v3/register":
			json.NewEncoder(writer).Encode(map[string]any{
				"user_id":      "@alice:bureau.local",
				"access_token": "new-token",
			})

		case strings.Contains(request.URL.Path, "/invite"):
			gotInvite = true
			t.Error("invite should not be called without --operator")
			json.NewEncoder(writer).Encode(struct{}{})

		default:
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
			http.Error(writer, `{"errcode":"M_UNKNOWN"}`, http.StatusNotFound)
		}
	}))
	defer server.Close()

	credentialFile := writeTestCredentials(t, server.URL)

	command := userCreateCommand()
	err := command.Execute([]string{
		"--credential-file", credentialFile,
		"alice",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotInvite {
		t.Error("invite was called without --operator flag")
	}
}

// TestUserCreate_Operator_ExistingUser_PasswordVerified tests that when an
// account exists and --password-file is set, the code logs in to verify the
// password and proceeds to invite + join.
func TestUserCreate_Operator_ExistingUser_PasswordVerified(t *testing.T) {
	var (
		mutex       sync.Mutex
		gotLogin    bool
		inviteCount int
		joinCount   int
	)

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")

		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/_matrix/client/v3/register":
			writer.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(writer).Encode(messaging.MatrixError{
				Code:    messaging.ErrCodeUserInUse,
				Message: "User ID already taken",
			})

		case request.Method == http.MethodPost && request.URL.Path == "/_matrix/client/v3/login":
			mutex.Lock()
			gotLogin = true
			mutex.Unlock()
			json.NewEncoder(writer).Encode(map[string]any{
				"user_id":      "@alice:bureau.local",
				"access_token": "existing-token",
			})

		case request.Method == http.MethodPost && strings.Contains(request.URL.Path, "/invite"):
			mutex.Lock()
			inviteCount++
			mutex.Unlock()
			json.NewEncoder(writer).Encode(struct{}{})

		case request.Method == http.MethodPost && strings.Contains(request.URL.Path, "/join"):
			mutex.Lock()
			joinCount++
			mutex.Unlock()
			json.NewEncoder(writer).Encode(map[string]any{"room_id": "!room:bureau.local"})

		default:
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
			http.Error(writer, `{"errcode":"M_UNKNOWN"}`, http.StatusNotFound)
		}
	}))
	defer server.Close()

	credentialFile := writeTestCredentials(t, server.URL)
	passwordFile := writeTestPasswordFile(t, "correct-password")

	command := userCreateCommand()
	err := command.Execute([]string{
		"--credential-file", credentialFile,
		"--password-file", passwordFile,
		"--operator",
		"alice",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mutex.Lock()
	defer mutex.Unlock()

	if !gotLogin {
		t.Error("login endpoint was not called to verify password")
	}
	expectedRoomCount := 1 + len(standardRooms) // space + all standard rooms
	if inviteCount != expectedRoomCount {
		t.Errorf("expected %d invites after password verification, got %d", expectedRoomCount, inviteCount)
	}
	if joinCount != expectedRoomCount {
		t.Errorf("expected %d joins after password verification, got %d", expectedRoomCount, joinCount)
	}
}

// TestUserCreate_Operator_ExistingUser_PasswordMismatch tests that when an
// account exists and the provided password doesn't match, the command fails
// loudly instead of silently discarding the password.
func TestUserCreate_Operator_ExistingUser_PasswordMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")

		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/_matrix/client/v3/register":
			writer.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(writer).Encode(messaging.MatrixError{
				Code:    messaging.ErrCodeUserInUse,
				Message: "User ID already taken",
			})

		case request.Method == http.MethodPost && request.URL.Path == "/_matrix/client/v3/login":
			writer.WriteHeader(http.StatusForbidden)
			json.NewEncoder(writer).Encode(messaging.MatrixError{
				Code:    messaging.ErrCodeForbidden,
				Message: "Wrong username or password",
			})

		default:
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
			http.Error(writer, `{"errcode":"M_UNKNOWN"}`, http.StatusNotFound)
		}
	}))
	defer server.Close()

	credentialFile := writeTestCredentials(t, server.URL)
	passwordFile := writeTestPasswordFile(t, "wrong-password")

	command := userCreateCommand()
	err := command.Execute([]string{
		"--credential-file", credentialFile,
		"--password-file", passwordFile,
		"--operator",
		"alice",
	})
	if err == nil {
		t.Fatal("expected error for password mismatch on existing account")
	}
	if !strings.Contains(err.Error(), "already exists but the provided password does not match") {
		t.Errorf("unexpected error: %v", err)
	}
}

// writeTestPasswordFile writes a password to a temp file and returns its path.
func writeTestPasswordFile(t *testing.T, password string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(path, []byte(password), 0600); err != nil {
		t.Fatalf("write test password file: %v", err)
	}
	return path
}

// writeTestCredentials writes a credential file for testing and returns its path.
func writeTestCredentials(t *testing.T, homeserverURL string) string {
	t.Helper()

	content := strings.Join([]string{
		"MATRIX_HOMESERVER_URL=" + homeserverURL,
		"MATRIX_ADMIN_USER=@bureau-admin:bureau.local",
		"MATRIX_ADMIN_TOKEN=test-admin-token",
		"MATRIX_REGISTRATION_TOKEN=test-registration-token",
		"MATRIX_SPACE_ROOM=!space:bureau.local",
		"MATRIX_SYSTEM_ROOM=!system:bureau.local",
		"MATRIX_TEMPLATE_ROOM=!template:bureau.local",
		"MATRIX_PIPELINE_ROOM=!pipeline:bureau.local",
		"MATRIX_ARTIFACT_ROOM=!artifact:bureau.local",
	}, "\n")

	path := filepath.Join(t.TempDir(), "bureau-creds")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write test credentials: %v", err)
	}
	return path
}
