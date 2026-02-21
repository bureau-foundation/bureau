// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package credential

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/sealed"
	"github.com/bureau-foundation/bureau/messaging"
)

// mustUserID parses a user ID string or panics. For test code only.
func mustUserID(raw string) ref.UserID {
	userID, err := ref.ParseUserID(raw)
	if err != nil {
		panic(fmt.Sprintf("invalid test user ID %q: %v", raw, err))
	}
	return userID
}

// mustRoomID parses a room ID string or panics. For test code only.
func mustRoomID(raw string) ref.RoomID {
	roomID, err := ref.ParseRoomID(raw)
	if err != nil {
		panic(fmt.Sprintf("invalid test room ID %q: %v", raw, err))
	}
	return roomID
}

// mockSession implements messaging.Session for unit testing. Only the
// methods used by credential.Provision and credential.List are wired
// up; every other method panics so that unexpected calls are caught
// immediately.
type mockSession struct {
	userID ref.UserID

	// getStateEvent is called by Provision to fetch the machine's public key.
	getStateEvent func(ctx context.Context, roomID ref.RoomID, eventType, stateKey string) (json.RawMessage, error)

	// getRoomState is called by List to enumerate credential events.
	getRoomState func(ctx context.Context, roomID ref.RoomID) ([]messaging.Event, error)

	// resolveAlias is called by both Provision and List to look up config rooms.
	resolveAlias func(ctx context.Context, alias ref.RoomAlias) (ref.RoomID, error)

	// sendStateEvent is called by Provision to publish the credential event.
	sendStateEvent func(ctx context.Context, roomID ref.RoomID, eventType, stateKey string, content any) (string, error)
}

func (m *mockSession) UserID() ref.UserID { return m.userID }
func (m *mockSession) Close() error       { return nil }

func (m *mockSession) GetStateEvent(ctx context.Context, roomID ref.RoomID, eventType, stateKey string) (json.RawMessage, error) {
	if m.getStateEvent == nil {
		panic("GetStateEvent not implemented")
	}
	return m.getStateEvent(ctx, roomID, eventType, stateKey)
}

func (m *mockSession) GetRoomState(ctx context.Context, roomID ref.RoomID) ([]messaging.Event, error) {
	if m.getRoomState == nil {
		panic("GetRoomState not implemented")
	}
	return m.getRoomState(ctx, roomID)
}

func (m *mockSession) ResolveAlias(ctx context.Context, alias ref.RoomAlias) (ref.RoomID, error) {
	if m.resolveAlias == nil {
		panic("ResolveAlias not implemented")
	}
	return m.resolveAlias(ctx, alias)
}

func (m *mockSession) SendStateEvent(ctx context.Context, roomID ref.RoomID, eventType, stateKey string, content any) (string, error) {
	if m.sendStateEvent == nil {
		panic("SendStateEvent not implemented")
	}
	return m.sendStateEvent(ctx, roomID, eventType, stateKey, content)
}

func (m *mockSession) WhoAmI(ctx context.Context) (ref.UserID, error) {
	panic("WhoAmI not implemented")
}

func (m *mockSession) SendEvent(ctx context.Context, roomID ref.RoomID, eventType string, content any) (string, error) {
	panic("SendEvent not implemented")
}

func (m *mockSession) SendMessage(ctx context.Context, roomID ref.RoomID, content messaging.MessageContent) (string, error) {
	panic("SendMessage not implemented")
}

func (m *mockSession) CreateRoom(ctx context.Context, request messaging.CreateRoomRequest) (*messaging.CreateRoomResponse, error) {
	panic("CreateRoom not implemented")
}

func (m *mockSession) InviteUser(ctx context.Context, roomID ref.RoomID, userID ref.UserID) error {
	panic("InviteUser not implemented")
}

func (m *mockSession) JoinRoom(ctx context.Context, roomID ref.RoomID) (ref.RoomID, error) {
	panic("JoinRoom not implemented")
}

func (m *mockSession) JoinedRooms(ctx context.Context) ([]ref.RoomID, error) {
	panic("JoinedRooms not implemented")
}

func (m *mockSession) GetRoomMembers(ctx context.Context, roomID ref.RoomID) ([]messaging.RoomMember, error) {
	panic("GetRoomMembers not implemented")
}

func (m *mockSession) GetDisplayName(ctx context.Context, userID ref.UserID) (string, error) {
	panic("GetDisplayName not implemented")
}

func (m *mockSession) RoomMessages(ctx context.Context, roomID ref.RoomID, options messaging.RoomMessagesOptions) (*messaging.RoomMessagesResponse, error) {
	panic("RoomMessages not implemented")
}

func (m *mockSession) ThreadMessages(ctx context.Context, roomID ref.RoomID, threadRootID string, options messaging.ThreadMessagesOptions) (*messaging.ThreadMessagesResponse, error) {
	panic("ThreadMessages not implemented")
}

func (m *mockSession) Sync(ctx context.Context, options messaging.SyncOptions) (*messaging.SyncResponse, error) {
	panic("Sync not implemented")
}

// Compile-time check: *mockSession implements messaging.Session.
var _ messaging.Session = (*mockSession)(nil)

// testMachine builds a valid ref.Machine for use in tests. Uses
// "bureau.local" as the server and "my_bureau/fleet/prod" as the fleet.
func testMachine(t *testing.T) ref.Machine {
	t.Helper()
	namespace, err := ref.NewNamespace(ref.MustParseServerName("bureau.local"), "my_bureau")
	if err != nil {
		t.Fatalf("NewNamespace: %v", err)
	}
	fleet, err := ref.NewFleet(namespace, "prod")
	if err != nil {
		t.Fatalf("NewFleet: %v", err)
	}
	machine, err := ref.NewMachine(fleet, "workstation")
	if err != nil {
		t.Fatalf("NewMachine: %v", err)
	}
	return machine
}

// testEntity builds a valid ref.Entity for use in tests. The account
// localpart is "agent/test-principal", giving a fleet-scoped localpart
// of "my_bureau/fleet/prod/agent/test-principal".
func testEntity(t *testing.T) ref.Entity {
	t.Helper()
	machine := testMachine(t)
	entity, err := ref.NewEntityFromAccountLocalpart(machine.Fleet(), "agent/test-principal")
	if err != nil {
		t.Fatalf("NewEntityFromAccountLocalpart: %v", err)
	}
	return entity
}

// testKeypair generates a real age keypair for encryption tests.
// The keypair is closed automatically when the test completes.
func testKeypair(t *testing.T) *sealed.Keypair {
	t.Helper()
	keypair, err := sealed.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	t.Cleanup(func() { keypair.Close() })
	return keypair
}

// machineKeyJSON returns a JSON-encoded MachineKey state event content
// with the given algorithm and public key.
func machineKeyJSON(algorithm, publicKey string) json.RawMessage {
	key := schema.MachineKey{
		Algorithm: algorithm,
		PublicKey: publicKey,
	}
	data, err := json.Marshal(key)
	if err != nil {
		panic(fmt.Sprintf("marshaling MachineKey: %v", err))
	}
	return data
}

// --- Provision tests ---

func TestProvision_MissingMachine(t *testing.T) {
	session := &mockSession{userID: mustUserID("@operator:bureau.local")}
	_, err := Provision(context.Background(), session, ProvisionParams{
		Principal:     testEntity(t),
		MachineRoomID: mustRoomID("!room:bureau.local"),
		Credentials:   map[string]string{"TOKEN": "secret"},
	})
	if err == nil {
		t.Fatal("expected error for missing machine, got nil")
	}
	if got := err.Error(); got != "machine is required" {
		t.Errorf("error = %q, want %q", got, "machine is required")
	}
}

func TestProvision_ZeroPrincipal(t *testing.T) {
	machine := testMachine(t)
	session := &mockSession{userID: mustUserID("@operator:bureau.local")}
	_, err := Provision(context.Background(), session, ProvisionParams{
		Machine:       machine,
		MachineRoomID: mustRoomID("!room:bureau.local"),
		Credentials:   map[string]string{"TOKEN": "secret"},
	})
	if err == nil {
		t.Fatal("expected error for zero principal, got nil")
	}
	if got := err.Error(); got != "principal is required" {
		t.Errorf("error = %q, want %q", got, "principal is required")
	}
}

func TestProvision_EmptyCredentials(t *testing.T) {
	machine := testMachine(t)
	session := &mockSession{userID: mustUserID("@operator:bureau.local")}
	_, err := Provision(context.Background(), session, ProvisionParams{
		Machine:       machine,
		Principal:     testEntity(t),
		MachineRoomID: mustRoomID("!room:bureau.local"),
		Credentials:   map[string]string{},
	})
	if err == nil {
		t.Fatal("expected error for empty credentials, got nil")
	}
	if got := err.Error(); got != "credentials map is empty" {
		t.Errorf("error = %q, want %q", got, "credentials map is empty")
	}
}

func TestProvision_MissingMachineRoomID(t *testing.T) {
	machine := testMachine(t)
	session := &mockSession{userID: mustUserID("@operator:bureau.local")}
	_, err := Provision(context.Background(), session, ProvisionParams{
		Machine:     machine,
		Principal:   testEntity(t),
		Credentials: map[string]string{"TOKEN": "secret"},
	})
	if err == nil {
		t.Fatal("expected error for missing machine room ID, got nil")
	}
	if got := err.Error(); got != "machine room ID is required" {
		t.Errorf("error = %q, want %q", got, "machine room ID is required")
	}
}

func TestProvision_MachineKeyFetchFails(t *testing.T) {
	machine := testMachine(t)
	session := &mockSession{
		userID: mustUserID("@operator:bureau.local"),
		getStateEvent: func(_ context.Context, _ ref.RoomID, _, _ string) (json.RawMessage, error) {
			return nil, fmt.Errorf("homeserver unreachable")
		},
	}
	_, err := Provision(context.Background(), session, ProvisionParams{
		Machine:       machine,
		Principal:     testEntity(t),
		MachineRoomID: mustRoomID("!room:bureau.local"),
		Credentials:   map[string]string{"TOKEN": "secret"},
	})
	if err == nil {
		t.Fatal("expected error when machine key fetch fails, got nil")
	}
	expected := `fetching machine key for "my_bureau/fleet/prod/machine/workstation": homeserver unreachable`
	if got := err.Error(); got != expected {
		t.Errorf("error = %q, want %q", got, expected)
	}
}

func TestProvision_WrongKeyAlgorithm(t *testing.T) {
	machine := testMachine(t)
	keypair := testKeypair(t)
	session := &mockSession{
		userID: mustUserID("@operator:bureau.local"),
		getStateEvent: func(_ context.Context, _ ref.RoomID, _, _ string) (json.RawMessage, error) {
			return machineKeyJSON("rsa-4096", keypair.PublicKey), nil
		},
	}
	_, err := Provision(context.Background(), session, ProvisionParams{
		Machine:       machine,
		Principal:     testEntity(t),
		MachineRoomID: mustRoomID("!room:bureau.local"),
		Credentials:   map[string]string{"TOKEN": "secret"},
	})
	if err == nil {
		t.Fatal("expected error for wrong key algorithm, got nil")
	}
	expected := `unsupported machine key algorithm: "rsa-4096" (expected age-x25519)`
	if got := err.Error(); got != expected {
		t.Errorf("error = %q, want %q", got, expected)
	}
}

func TestProvision_InvalidPublicKey(t *testing.T) {
	machine := testMachine(t)
	session := &mockSession{
		userID: mustUserID("@operator:bureau.local"),
		getStateEvent: func(_ context.Context, _ ref.RoomID, _, _ string) (json.RawMessage, error) {
			return machineKeyJSON("age-x25519", "not-a-real-age-key"), nil
		},
	}
	_, err := Provision(context.Background(), session, ProvisionParams{
		Machine:       machine,
		Principal:     testEntity(t),
		MachineRoomID: mustRoomID("!room:bureau.local"),
		Credentials:   map[string]string{"TOKEN": "secret"},
	})
	if err == nil {
		t.Fatal("expected error for invalid public key, got nil")
	}
}

func TestProvision_ConfigRoomResolveFails(t *testing.T) {
	machine := testMachine(t)
	keypair := testKeypair(t)
	session := &mockSession{
		userID: mustUserID("@operator:bureau.local"),
		getStateEvent: func(_ context.Context, _ ref.RoomID, _, _ string) (json.RawMessage, error) {
			return machineKeyJSON("age-x25519", keypair.PublicKey), nil
		},
		resolveAlias: func(_ context.Context, _ ref.RoomAlias) (ref.RoomID, error) {
			return ref.RoomID{}, fmt.Errorf("alias not found")
		},
	}
	_, err := Provision(context.Background(), session, ProvisionParams{
		Machine:       machine,
		Principal:     testEntity(t),
		MachineRoomID: mustRoomID("!room:bureau.local"),
		Credentials:   map[string]string{"TOKEN": "secret"},
	})
	if err == nil {
		t.Fatal("expected error when config room resolve fails, got nil")
	}
	expectedPrefix := "resolving config room"
	if got := err.Error(); len(got) < len(expectedPrefix) || got[:len(expectedPrefix)] != expectedPrefix {
		t.Errorf("error = %q, want prefix %q", got, expectedPrefix)
	}
}

func TestProvision_SendStateEventFails(t *testing.T) {
	machine := testMachine(t)
	keypair := testKeypair(t)
	session := &mockSession{
		userID: mustUserID("@operator:bureau.local"),
		getStateEvent: func(_ context.Context, _ ref.RoomID, _, _ string) (json.RawMessage, error) {
			return machineKeyJSON("age-x25519", keypair.PublicKey), nil
		},
		resolveAlias: func(_ context.Context, _ ref.RoomAlias) (ref.RoomID, error) {
			return mustRoomID("!config:bureau.local"), nil
		},
		sendStateEvent: func(_ context.Context, _ ref.RoomID, _, _ string, _ any) (string, error) {
			return "", fmt.Errorf("permission denied")
		},
	}
	_, err := Provision(context.Background(), session, ProvisionParams{
		Machine:       machine,
		Principal:     testEntity(t),
		MachineRoomID: mustRoomID("!room:bureau.local"),
		Credentials:   map[string]string{"TOKEN": "secret"},
	})
	if err == nil {
		t.Fatal("expected error when send state event fails, got nil")
	}
	expectedPrefix := "publishing credentials"
	if got := err.Error(); len(got) < len(expectedPrefix) || got[:len(expectedPrefix)] != expectedPrefix {
		t.Errorf("error = %q, want prefix %q", got, expectedPrefix)
	}
}

func TestProvision_HappyPath(t *testing.T) {
	machine := testMachine(t)
	entity := testEntity(t)
	keypair := testKeypair(t)

	var capturedRoomID ref.RoomID
	var capturedEventType, capturedStateKey string
	var capturedContent any

	session := &mockSession{
		userID: mustUserID("@operator:bureau.local"),
		getStateEvent: func(_ context.Context, roomID ref.RoomID, eventType, stateKey string) (json.RawMessage, error) {
			if roomID.String() != "!machine-room:bureau.local" {
				t.Errorf("GetStateEvent roomID = %q, want %q", roomID, "!machine-room:bureau.local")
			}
			if eventType != schema.EventTypeMachineKey {
				t.Errorf("GetStateEvent eventType = %q, want %q", eventType, schema.EventTypeMachineKey)
			}
			if stateKey != machine.Localpart() {
				t.Errorf("GetStateEvent stateKey = %q, want %q", stateKey, machine.Localpart())
			}
			return machineKeyJSON("age-x25519", keypair.PublicKey), nil
		},
		resolveAlias: func(_ context.Context, alias ref.RoomAlias) (ref.RoomID, error) {
			if alias.String() != machine.RoomAlias().String() {
				t.Errorf("ResolveAlias alias = %q, want %q", alias, machine.RoomAlias())
			}
			return mustRoomID("!config:bureau.local"), nil
		},
		sendStateEvent: func(_ context.Context, roomID ref.RoomID, eventType, stateKey string, content any) (string, error) {
			capturedRoomID = roomID
			capturedEventType = eventType
			capturedStateKey = stateKey
			capturedContent = content
			return "$event123", nil
		},
	}

	result, err := Provision(context.Background(), session, ProvisionParams{
		Machine:       machine,
		Principal:     entity,
		MachineRoomID: mustRoomID("!machine-room:bureau.local"),
		Credentials:   map[string]string{"MATRIX_TOKEN": "syt_secret", "API_KEY": "key123"},
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// Verify the result fields.
	if result.EventID != "$event123" {
		t.Errorf("EventID = %q, want %q", result.EventID, "$event123")
	}
	if result.ConfigRoomID.String() != "!config:bureau.local" {
		t.Errorf("ConfigRoomID = %q, want %q", result.ConfigRoomID, "!config:bureau.local")
	}
	if result.PrincipalID != entity.UserID() {
		t.Errorf("PrincipalID = %q, want %q", result.PrincipalID, entity.UserID())
	}
	if len(result.EncryptedFor) != 1 || result.EncryptedFor[0] != machine.UserID().String() {
		t.Errorf("EncryptedFor = %v, want [%q]", result.EncryptedFor, machine.UserID())
	}
	// Keys should be sorted alphabetically.
	if len(result.Keys) != 2 || result.Keys[0] != "API_KEY" || result.Keys[1] != "MATRIX_TOKEN" {
		t.Errorf("Keys = %v, want [API_KEY, MATRIX_TOKEN]", result.Keys)
	}

	// Verify the SendStateEvent call arguments.
	if capturedRoomID.String() != "!config:bureau.local" {
		t.Errorf("SendStateEvent roomID = %q, want %q", capturedRoomID, "!config:bureau.local")
	}
	if capturedEventType != schema.EventTypeCredentials {
		t.Errorf("SendStateEvent eventType = %q, want %q", capturedEventType, schema.EventTypeCredentials)
	}
	if capturedStateKey != entity.Localpart() {
		t.Errorf("SendStateEvent stateKey = %q, want %q", capturedStateKey, entity.Localpart())
	}

	// Verify the credential event content.
	credentials, ok := capturedContent.(schema.Credentials)
	if !ok {
		t.Fatalf("SendStateEvent content type = %T, want schema.Credentials", capturedContent)
	}
	if credentials.Version != schema.CredentialsVersion {
		t.Errorf("Credentials.Version = %d, want %d", credentials.Version, schema.CredentialsVersion)
	}
	if credentials.Principal != entity.UserID() {
		t.Errorf("Credentials.Principal = %q, want %q", credentials.Principal, entity.UserID())
	}
	if credentials.ProvisionedBy.String() != "@operator:bureau.local" {
		t.Errorf("Credentials.ProvisionedBy = %q, want %q", credentials.ProvisionedBy, "@operator:bureau.local")
	}
	if credentials.Ciphertext == "" {
		t.Error("Credentials.Ciphertext is empty")
	}
	if credentials.ProvisionedAt == "" {
		t.Error("Credentials.ProvisionedAt is empty")
	}
}

func TestProvision_HappyPathWithEscrowKey(t *testing.T) {
	machine := testMachine(t)
	machineKeypair := testKeypair(t)
	escrowKeypair := testKeypair(t)

	session := &mockSession{
		userID: mustUserID("@operator:bureau.local"),
		getStateEvent: func(_ context.Context, _ ref.RoomID, _, _ string) (json.RawMessage, error) {
			return machineKeyJSON("age-x25519", machineKeypair.PublicKey), nil
		},
		resolveAlias: func(_ context.Context, _ ref.RoomAlias) (ref.RoomID, error) {
			return mustRoomID("!config:bureau.local"), nil
		},
		sendStateEvent: func(_ context.Context, _ ref.RoomID, _, _ string, _ any) (string, error) {
			return "$event456", nil
		},
	}

	result, err := Provision(context.Background(), session, ProvisionParams{
		Machine:       machine,
		Principal:     testEntity(t),
		MachineRoomID: mustRoomID("!machine-room:bureau.local"),
		EscrowKey:     escrowKeypair.PublicKey,
		Credentials:   map[string]string{"TOKEN": "secret"},
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if len(result.EncryptedFor) != 2 {
		t.Fatalf("EncryptedFor length = %d, want 2", len(result.EncryptedFor))
	}
	if result.EncryptedFor[0] != machine.UserID().String() {
		t.Errorf("EncryptedFor[0] = %q, want %q", result.EncryptedFor[0], machine.UserID())
	}
	if result.EncryptedFor[1] != "escrow:operator" {
		t.Errorf("EncryptedFor[1] = %q, want %q", result.EncryptedFor[1], "escrow:operator")
	}
}

func TestProvision_InvalidEscrowKey(t *testing.T) {
	machine := testMachine(t)
	keypair := testKeypair(t)
	session := &mockSession{
		userID: mustUserID("@operator:bureau.local"),
		getStateEvent: func(_ context.Context, _ ref.RoomID, _, _ string) (json.RawMessage, error) {
			return machineKeyJSON("age-x25519", keypair.PublicKey), nil
		},
	}

	_, err := Provision(context.Background(), session, ProvisionParams{
		Machine:       machine,
		Principal:     testEntity(t),
		MachineRoomID: mustRoomID("!room:bureau.local"),
		EscrowKey:     "not-a-valid-escrow-key",
		Credentials:   map[string]string{"TOKEN": "secret"},
	})
	if err == nil {
		t.Fatal("expected error for invalid escrow key, got nil")
	}
	expectedPrefix := "invalid escrow key"
	if got := err.Error(); len(got) < len(expectedPrefix) || got[:len(expectedPrefix)] != expectedPrefix {
		t.Errorf("error = %q, want prefix %q", got, expectedPrefix)
	}
}

// --- List tests ---

func TestList_MissingMachine(t *testing.T) {
	session := &mockSession{userID: mustUserID("@operator:bureau.local")}
	_, err := List(context.Background(), session, ref.Machine{})
	if err == nil {
		t.Fatal("expected error for missing machine, got nil")
	}
	if got := err.Error(); got != "machine is required" {
		t.Errorf("error = %q, want %q", got, "machine is required")
	}
}

func TestList_ConfigRoomResolveFails(t *testing.T) {
	machine := testMachine(t)
	session := &mockSession{
		userID: mustUserID("@operator:bureau.local"),
		resolveAlias: func(_ context.Context, _ ref.RoomAlias) (ref.RoomID, error) {
			return ref.RoomID{}, fmt.Errorf("server error")
		},
	}
	_, err := List(context.Background(), session, machine)
	if err == nil {
		t.Fatal("expected error when config room resolve fails, got nil")
	}
	expectedPrefix := "resolving config room"
	if got := err.Error(); len(got) < len(expectedPrefix) || got[:len(expectedPrefix)] != expectedPrefix {
		t.Errorf("error = %q, want prefix %q", got, expectedPrefix)
	}
}

func TestList_ConfigRoomNotFound(t *testing.T) {
	machine := testMachine(t)
	session := &mockSession{
		userID: mustUserID("@operator:bureau.local"),
		resolveAlias: func(_ context.Context, _ ref.RoomAlias) (ref.RoomID, error) {
			return ref.RoomID{}, &messaging.MatrixError{
				Code:       messaging.ErrCodeNotFound,
				Message:    "Room alias not found",
				StatusCode: 404,
			}
		},
	}
	_, err := List(context.Background(), session, machine)
	if err == nil {
		t.Fatal("expected error for not-found config room, got nil")
	}
	// The M_NOT_FOUND branch produces a specific error message rather than
	// wrapping the Matrix error.
	expectedSubstring := "no config room found"
	if got := err.Error(); !containsSubstring(got, expectedSubstring) {
		t.Errorf("error = %q, want substring %q", got, expectedSubstring)
	}
}

func TestList_RoomStateFetchFails(t *testing.T) {
	machine := testMachine(t)
	session := &mockSession{
		userID: mustUserID("@operator:bureau.local"),
		resolveAlias: func(_ context.Context, _ ref.RoomAlias) (ref.RoomID, error) {
			return mustRoomID("!config:bureau.local"), nil
		},
		getRoomState: func(_ context.Context, _ ref.RoomID) ([]messaging.Event, error) {
			return nil, fmt.Errorf("forbidden")
		},
	}
	_, err := List(context.Background(), session, machine)
	if err == nil {
		t.Fatal("expected error when room state fetch fails, got nil")
	}
	expectedPrefix := "reading state from config room"
	if got := err.Error(); len(got) < len(expectedPrefix) || got[:len(expectedPrefix)] != expectedPrefix {
		t.Errorf("error = %q, want prefix %q", got, expectedPrefix)
	}
}

func TestList_NoCredentialEvents(t *testing.T) {
	machine := testMachine(t)
	memberStateKey := "@someone:bureau.local"
	session := &mockSession{
		userID: mustUserID("@operator:bureau.local"),
		resolveAlias: func(_ context.Context, _ ref.RoomAlias) (ref.RoomID, error) {
			return mustRoomID("!config:bureau.local"), nil
		},
		getRoomState: func(_ context.Context, _ ref.RoomID) ([]messaging.Event, error) {
			return []messaging.Event{
				{
					Type:     "m.room.member",
					StateKey: &memberStateKey,
					Content:  map[string]any{"membership": "join"},
				},
			}, nil
		},
	}
	result, err := List(context.Background(), session, machine)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if result.ConfigRoomID.String() != "!config:bureau.local" {
		t.Errorf("ConfigRoomID = %q, want %q", result.ConfigRoomID, "!config:bureau.local")
	}
	if result.Machine != machine {
		t.Errorf("Machine = %v, want %v", result.Machine, machine)
	}
	if len(result.Bundles) != 0 {
		t.Errorf("Bundles length = %d, want 0", len(result.Bundles))
	}
}

func TestList_HappyPathMultipleBundles(t *testing.T) {
	machine := testMachine(t)

	stateKeyAlpha := "iree/amdgpu/pm"
	stateKeyBeta := "sysadmin"

	credentialsAlpha := schema.Credentials{
		Version:       1,
		Principal:     mustUserID("@iree/amdgpu/pm:bureau.local"),
		EncryptedFor:  []string{machine.UserID().String()},
		Keys:          []string{"MATRIX_TOKEN"},
		Ciphertext:    "encrypted-alpha",
		ProvisionedBy: mustUserID("@operator:bureau.local"),
		ProvisionedAt: "2026-02-19T10:00:00Z",
	}
	credentialsBeta := schema.Credentials{
		Version:       1,
		Principal:     mustUserID("@sysadmin:bureau.local"),
		EncryptedFor:  []string{machine.UserID().String(), "escrow:operator"},
		Keys:          []string{"API_KEY", "SECRET"},
		Ciphertext:    "encrypted-beta",
		ProvisionedBy: mustUserID("@admin:bureau.local"),
		ProvisionedAt: "2026-02-19T11:00:00Z",
	}

	// Event.Content is map[string]any in the messaging.Event type, so we
	// round-trip through JSON to get the map representation that the real
	// sync API would return.
	alphaContentMap := marshalToMap(t, credentialsAlpha)
	betaContentMap := marshalToMap(t, credentialsBeta)

	session := &mockSession{
		userID: mustUserID("@operator:bureau.local"),
		resolveAlias: func(_ context.Context, alias ref.RoomAlias) (ref.RoomID, error) {
			if alias.String() != machine.RoomAlias().String() {
				t.Errorf("ResolveAlias alias = %q, want %q", alias, machine.RoomAlias())
			}
			return mustRoomID("!config:bureau.local"), nil
		},
		getRoomState: func(_ context.Context, roomID ref.RoomID) ([]messaging.Event, error) {
			if roomID.String() != "!config:bureau.local" {
				t.Errorf("GetRoomState roomID = %q, want %q", roomID, "!config:bureau.local")
			}
			emptyStateKey := ""
			return []messaging.Event{
				{
					Type:     "m.room.create",
					StateKey: &emptyStateKey,
					Content:  map[string]any{},
				},
				{
					Type:     schema.EventTypeCredentials,
					StateKey: &stateKeyAlpha,
					Content:  alphaContentMap,
				},
				{
					// Credential event with nil StateKey should be skipped.
					Type:    schema.EventTypeCredentials,
					Content: map[string]any{},
				},
				{
					// Credential event with empty StateKey should be skipped.
					Type:     schema.EventTypeCredentials,
					StateKey: &emptyStateKey,
					Content:  map[string]any{},
				},
				{
					Type:     schema.EventTypeCredentials,
					StateKey: &stateKeyBeta,
					Content:  betaContentMap,
				},
			}, nil
		},
	}

	result, err := List(context.Background(), session, machine)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if result.ConfigRoomID.String() != "!config:bureau.local" {
		t.Errorf("ConfigRoomID = %q, want %q", result.ConfigRoomID, "!config:bureau.local")
	}
	if len(result.Bundles) != 2 {
		t.Fatalf("Bundles length = %d, want 2", len(result.Bundles))
	}

	// First bundle: alpha
	bundleAlpha := result.Bundles[0]
	if bundleAlpha.StateKey != "iree/amdgpu/pm" {
		t.Errorf("Bundles[0].StateKey = %q, want %q", bundleAlpha.StateKey, "iree/amdgpu/pm")
	}
	if bundleAlpha.Principal.String() != "@iree/amdgpu/pm:bureau.local" {
		t.Errorf("Bundles[0].Principal = %q, want %q", bundleAlpha.Principal, "@iree/amdgpu/pm:bureau.local")
	}
	if len(bundleAlpha.EncryptedFor) != 1 || bundleAlpha.EncryptedFor[0] != machine.UserID().String() {
		t.Errorf("Bundles[0].EncryptedFor = %v, want [%q]", bundleAlpha.EncryptedFor, machine.UserID())
	}
	if len(bundleAlpha.Keys) != 1 || bundleAlpha.Keys[0] != "MATRIX_TOKEN" {
		t.Errorf("Bundles[0].Keys = %v, want [MATRIX_TOKEN]", bundleAlpha.Keys)
	}
	if bundleAlpha.ProvisionedBy.String() != "@operator:bureau.local" {
		t.Errorf("Bundles[0].ProvisionedBy = %q, want %q", bundleAlpha.ProvisionedBy, "@operator:bureau.local")
	}
	if bundleAlpha.ProvisionedAt != "2026-02-19T10:00:00Z" {
		t.Errorf("Bundles[0].ProvisionedAt = %q, want %q", bundleAlpha.ProvisionedAt, "2026-02-19T10:00:00Z")
	}

	// Second bundle: beta
	bundleBeta := result.Bundles[1]
	if bundleBeta.StateKey != "sysadmin" {
		t.Errorf("Bundles[1].StateKey = %q, want %q", bundleBeta.StateKey, "sysadmin")
	}
	if bundleBeta.Principal.String() != "@sysadmin:bureau.local" {
		t.Errorf("Bundles[1].Principal = %q, want %q", bundleBeta.Principal, "@sysadmin:bureau.local")
	}
	if len(bundleBeta.EncryptedFor) != 2 {
		t.Fatalf("Bundles[1].EncryptedFor length = %d, want 2", len(bundleBeta.EncryptedFor))
	}
	if bundleBeta.EncryptedFor[1] != "escrow:operator" {
		t.Errorf("Bundles[1].EncryptedFor[1] = %q, want %q", bundleBeta.EncryptedFor[1], "escrow:operator")
	}
	if len(bundleBeta.Keys) != 2 {
		t.Fatalf("Bundles[1].Keys length = %d, want 2", len(bundleBeta.Keys))
	}
	if bundleBeta.ProvisionedBy.String() != "@admin:bureau.local" {
		t.Errorf("Bundles[1].ProvisionedBy = %q, want %q", bundleBeta.ProvisionedBy, "@admin:bureau.local")
	}
}

// --- AsProvisionFunc tests ---

func TestAsProvisionFunc_DelegatesToProvision(t *testing.T) {
	machine := testMachine(t)
	keypair := testKeypair(t)

	session := &mockSession{
		userID: mustUserID("@operator:bureau.local"),
		getStateEvent: func(_ context.Context, _ ref.RoomID, _, _ string) (json.RawMessage, error) {
			return machineKeyJSON("age-x25519", keypair.PublicKey), nil
		},
		resolveAlias: func(_ context.Context, _ ref.RoomAlias) (ref.RoomID, error) {
			return mustRoomID("!config:bureau.local"), nil
		},
		sendStateEvent: func(_ context.Context, _ ref.RoomID, _, _ string, _ any) (string, error) {
			return "$event789", nil
		},
	}

	provisionFunc := AsProvisionFunc()
	configRoomID, err := provisionFunc(
		context.Background(),
		session,
		machine,
		testEntity(t),
		mustRoomID("!machine-room:bureau.local"),
		map[string]string{"TOKEN": "secret"},
	)
	if err != nil {
		t.Fatalf("AsProvisionFunc: %v", err)
	}
	if configRoomID.String() != "!config:bureau.local" {
		t.Errorf("configRoomID = %q, want %q", configRoomID, "!config:bureau.local")
	}
}

func TestAsProvisionFunc_PropagatesError(t *testing.T) {
	machine := testMachine(t)
	session := &mockSession{
		userID: mustUserID("@operator:bureau.local"),
		getStateEvent: func(_ context.Context, _ ref.RoomID, _, _ string) (json.RawMessage, error) {
			return nil, fmt.Errorf("fetch failed")
		},
	}

	provisionFunc := AsProvisionFunc()
	_, err := provisionFunc(
		context.Background(),
		session,
		machine,
		testEntity(t),
		mustRoomID("!machine-room:bureau.local"),
		map[string]string{"TOKEN": "secret"},
	)
	if err == nil {
		t.Fatal("expected error propagated from Provision, got nil")
	}
}

// --- helpers ---

// marshalToMap marshals a value to JSON and back to map[string]any,
// matching the representation that a real Matrix sync response would
// produce (since messaging.Event.Content is map[string]any).
func marshalToMap(t *testing.T, value any) map[string]any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshalToMap: Marshal: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("marshalToMap: Unmarshal: %v", err)
	}
	return result
}

// containsSubstring reports whether s contains substr.
func containsSubstring(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstring(s, substr)
}

func searchSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
