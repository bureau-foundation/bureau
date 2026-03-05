// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package credential

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// testFleet builds a valid ref.Fleet for use in tests. Uses "bureau.local"
// as the server and "my_bureau" as the namespace with fleet name "prod".
func testFleet(t *testing.T) ref.Fleet {
	t.Helper()
	namespace, err := ref.NewNamespace(ref.MustParseServerName("bureau.local"), "my_bureau")
	if err != nil {
		t.Fatalf("NewNamespace: %v", err)
	}
	fleet, err := ref.NewFleet(namespace, "prod")
	if err != nil {
		t.Fatalf("NewFleet: %v", err)
	}
	return fleet
}

// makeMachineKeyEvent constructs a messaging.Event that looks like an
// m.bureau.machine_key state event from a real /sync response.
func makeMachineKeyEvent(stateKey string, algorithm string, publicKey string) messaging.Event {
	content := map[string]any{
		"algorithm":  algorithm,
		"public_key": publicKey,
	}
	return messaging.Event{
		Type:     schema.EventTypeMachineKey,
		StateKey: &stateKey,
		Content:  content,
	}
}

// --- FleetProvision validation tests ---

func TestFleetProvision_MissingFleet(t *testing.T) {
	t.Parallel()
	session := &mockSession{userID: mustUserID("@operator:bureau.local")}
	_, err := FleetProvision(context.Background(), session, FleetProvisionParams{
		TargetRoomID:  mustRoomID("!target:bureau.local"),
		StateKey:      "builder",
		MachineRoomID: mustRoomID("!machine:bureau.local"),
		Credentials:   map[string]string{"TOKEN": "secret"},
	})
	if err == nil {
		t.Fatal("expected error for missing fleet, got nil")
	}
	if got := err.Error(); got != "fleet is required" {
		t.Errorf("error = %q, want %q", got, "fleet is required")
	}
}

func TestFleetProvision_MissingTargetRoomID(t *testing.T) {
	t.Parallel()
	session := &mockSession{userID: mustUserID("@operator:bureau.local")}
	_, err := FleetProvision(context.Background(), session, FleetProvisionParams{
		Fleet:         testFleet(t),
		StateKey:      "builder",
		MachineRoomID: mustRoomID("!machine:bureau.local"),
		Credentials:   map[string]string{"TOKEN": "secret"},
	})
	if err == nil {
		t.Fatal("expected error for missing target room ID, got nil")
	}
	if got := err.Error(); got != "target room ID is required" {
		t.Errorf("error = %q, want %q", got, "target room ID is required")
	}
}

func TestFleetProvision_MissingStateKey(t *testing.T) {
	t.Parallel()
	session := &mockSession{userID: mustUserID("@operator:bureau.local")}
	_, err := FleetProvision(context.Background(), session, FleetProvisionParams{
		Fleet:         testFleet(t),
		TargetRoomID:  mustRoomID("!target:bureau.local"),
		MachineRoomID: mustRoomID("!machine:bureau.local"),
		Credentials:   map[string]string{"TOKEN": "secret"},
	})
	if err == nil {
		t.Fatal("expected error for missing state key, got nil")
	}
	if got := err.Error(); got != "state key is required" {
		t.Errorf("error = %q, want %q", got, "state key is required")
	}
}

func TestFleetProvision_MissingMachineRoomID(t *testing.T) {
	t.Parallel()
	session := &mockSession{userID: mustUserID("@operator:bureau.local")}
	_, err := FleetProvision(context.Background(), session, FleetProvisionParams{
		Fleet:        testFleet(t),
		TargetRoomID: mustRoomID("!target:bureau.local"),
		StateKey:     "builder",
		Credentials:  map[string]string{"TOKEN": "secret"},
	})
	if err == nil {
		t.Fatal("expected error for missing machine room ID, got nil")
	}
	if got := err.Error(); got != "machine room ID is required" {
		t.Errorf("error = %q, want %q", got, "machine room ID is required")
	}
}

func TestFleetProvision_EmptyCredentials(t *testing.T) {
	t.Parallel()
	session := &mockSession{userID: mustUserID("@operator:bureau.local")}
	_, err := FleetProvision(context.Background(), session, FleetProvisionParams{
		Fleet:         testFleet(t),
		TargetRoomID:  mustRoomID("!target:bureau.local"),
		StateKey:      "builder",
		MachineRoomID: mustRoomID("!machine:bureau.local"),
		Credentials:   map[string]string{},
	})
	if err == nil {
		t.Fatal("expected error for empty credentials, got nil")
	}
	if got := err.Error(); got != "credentials map is empty" {
		t.Errorf("error = %q, want %q", got, "credentials map is empty")
	}
}

// --- FleetProvision machine enumeration tests ---

func TestFleetProvision_NoMachinesInFleet(t *testing.T) {
	t.Parallel()
	session := &mockSession{
		userID: mustUserID("@operator:bureau.local"),
		getRoomState: func(_ context.Context, _ ref.RoomID) ([]messaging.Event, error) {
			return []messaging.Event{}, nil
		},
	}
	_, err := FleetProvision(context.Background(), session, FleetProvisionParams{
		Fleet:         testFleet(t),
		TargetRoomID:  mustRoomID("!target:bureau.local"),
		StateKey:      "builder",
		MachineRoomID: mustRoomID("!machine:bureau.local"),
		Credentials:   map[string]string{"TOKEN": "secret"},
	})
	if err == nil {
		t.Fatal("expected error for no machines, got nil")
	}
	if !strings.Contains(err.Error(), "no machines with valid keys") {
		t.Errorf("error = %q, want substring %q", err.Error(), "no machines with valid keys")
	}
}

func TestFleetProvision_MachineRoomFetchFails(t *testing.T) {
	t.Parallel()
	session := &mockSession{
		userID: mustUserID("@operator:bureau.local"),
		getRoomState: func(_ context.Context, _ ref.RoomID) ([]messaging.Event, error) {
			return nil, fmt.Errorf("homeserver unreachable")
		},
	}
	_, err := FleetProvision(context.Background(), session, FleetProvisionParams{
		Fleet:         testFleet(t),
		TargetRoomID:  mustRoomID("!target:bureau.local"),
		StateKey:      "builder",
		MachineRoomID: mustRoomID("!machine:bureau.local"),
		Credentials:   map[string]string{"TOKEN": "secret"},
	})
	if err == nil {
		t.Fatal("expected error for machine room fetch failure, got nil")
	}
	if !strings.Contains(err.Error(), "enumerating fleet machine keys") {
		t.Errorf("error = %q, want substring %q", err.Error(), "enumerating fleet machine keys")
	}
}

func TestFleetProvision_SkipsDecommissionedMachines(t *testing.T) {
	t.Parallel()

	keypairActive := testKeypair(t)

	session := &mockSession{
		userID: mustUserID("@operator:bureau.local"),
		getRoomState: func(_ context.Context, _ ref.RoomID) ([]messaging.Event, error) {
			return []messaging.Event{
				// Active machine with valid key.
				makeMachineKeyEvent("my_bureau/fleet/prod/machine/gpu-box", "age-x25519", keypairActive.PublicKey),
				// Decommissioned machine (empty public key).
				makeMachineKeyEvent("my_bureau/fleet/prod/machine/old-box", "age-x25519", ""),
				// Machine with wrong algorithm (skipped).
				makeMachineKeyEvent("my_bureau/fleet/prod/machine/rsa-box", "rsa-4096", "not-an-age-key"),
			}, nil
		},
		sendStateEvent: func(_ context.Context, _ ref.RoomID, _ ref.EventType, _ string, _ any) (ref.EventID, error) {
			return ref.MustParseEventID("$event123"), nil
		},
	}

	result, err := FleetProvision(context.Background(), session, FleetProvisionParams{
		Fleet:         testFleet(t),
		TargetRoomID:  mustRoomID("!target:bureau.local"),
		StateKey:      "builder",
		MachineRoomID: mustRoomID("!machine:bureau.local"),
		Credentials:   map[string]string{"TOKEN": "secret"},
	})
	if err != nil {
		t.Fatalf("FleetProvision: %v", err)
	}

	// Only the active machine should be included.
	if result.MachineCount != 1 {
		t.Errorf("MachineCount = %d, want 1", result.MachineCount)
	}
	if len(result.EncryptedFor) != 1 {
		t.Fatalf("EncryptedFor length = %d, want 1", len(result.EncryptedFor))
	}
	if !strings.Contains(result.EncryptedFor[0], "gpu-box") {
		t.Errorf("EncryptedFor[0] = %q, want to contain 'gpu-box'", result.EncryptedFor[0])
	}
}

// --- FleetProvision happy path ---

func TestFleetProvision_HappyPathMultipleMachines(t *testing.T) {
	t.Parallel()

	keypairA := testKeypair(t)
	keypairB := testKeypair(t)

	var capturedRoomID ref.RoomID
	var capturedEventType ref.EventType
	var capturedStateKey string
	var capturedContent any

	session := &mockSession{
		userID: mustUserID("@operator:bureau.local"),
		getRoomState: func(_ context.Context, roomID ref.RoomID) ([]messaging.Event, error) {
			if roomID.String() != "!machine:bureau.local" {
				t.Errorf("GetRoomState roomID = %q, want %q", roomID, "!machine:bureau.local")
			}
			emptyStateKey := ""
			return []messaging.Event{
				{Type: "m.room.create", StateKey: &emptyStateKey, Content: map[string]any{}},
				makeMachineKeyEvent("my_bureau/fleet/prod/machine/alpha", "age-x25519", keypairA.PublicKey),
				makeMachineKeyEvent("my_bureau/fleet/prod/machine/beta", "age-x25519", keypairB.PublicKey),
			}, nil
		},
		sendStateEvent: func(_ context.Context, roomID ref.RoomID, eventType ref.EventType, stateKey string, content any) (ref.EventID, error) {
			capturedRoomID = roomID
			capturedEventType = eventType
			capturedStateKey = stateKey
			capturedContent = content
			return ref.MustParseEventID("$fleet-event-1"), nil
		},
	}

	result, err := FleetProvision(context.Background(), session, FleetProvisionParams{
		Fleet:         testFleet(t),
		TargetRoomID:  mustRoomID("!target:bureau.local"),
		StateKey:      "builder",
		MachineRoomID: mustRoomID("!machine:bureau.local"),
		Credentials:   map[string]string{"ATTIC_PUSH_TOKEN": "push-secret", "CACHIX_AUTH_TOKEN": "cachix-secret"},
	})
	if err != nil {
		t.Fatalf("FleetProvision: %v", err)
	}

	// Verify result fields.
	if result.EventID != ref.MustParseEventID("$fleet-event-1") {
		t.Errorf("EventID = %q, want %q", result.EventID, "$fleet-event-1")
	}
	if result.TargetRoomID.String() != "!target:bureau.local" {
		t.Errorf("TargetRoomID = %q, want %q", result.TargetRoomID, "!target:bureau.local")
	}
	if result.MachineCount != 2 {
		t.Errorf("MachineCount = %d, want 2", result.MachineCount)
	}

	// EncryptedFor should contain both machines, sorted by localpart.
	if len(result.EncryptedFor) != 2 {
		t.Fatalf("EncryptedFor length = %d, want 2", len(result.EncryptedFor))
	}
	if !strings.Contains(result.EncryptedFor[0], "alpha") {
		t.Errorf("EncryptedFor[0] = %q, want to contain 'alpha'", result.EncryptedFor[0])
	}
	if !strings.Contains(result.EncryptedFor[1], "beta") {
		t.Errorf("EncryptedFor[1] = %q, want to contain 'beta'", result.EncryptedFor[1])
	}

	// Keys should be sorted alphabetically.
	if len(result.Keys) != 2 || result.Keys[0] != "ATTIC_PUSH_TOKEN" || result.Keys[1] != "CACHIX_AUTH_TOKEN" {
		t.Errorf("Keys = %v, want [ATTIC_PUSH_TOKEN, CACHIX_AUTH_TOKEN]", result.Keys)
	}

	// Verify the SendStateEvent call arguments.
	if capturedRoomID.String() != "!target:bureau.local" {
		t.Errorf("SendStateEvent roomID = %q, want %q", capturedRoomID, "!target:bureau.local")
	}
	if capturedEventType != schema.EventTypeCredentials {
		t.Errorf("SendStateEvent eventType = %q, want %q", capturedEventType, schema.EventTypeCredentials)
	}
	if capturedStateKey != "builder" {
		t.Errorf("SendStateEvent stateKey = %q, want %q", capturedStateKey, "builder")
	}

	// Verify the credential event content.
	credentials, ok := capturedContent.(schema.Credentials)
	if !ok {
		t.Fatalf("SendStateEvent content type = %T, want schema.Credentials", capturedContent)
	}
	if credentials.Version != schema.CredentialsVersion {
		t.Errorf("Version = %d, want %d", credentials.Version, schema.CredentialsVersion)
	}
	if credentials.Ciphertext == "" {
		t.Error("Ciphertext is empty")
	}
	if credentials.ProvisionedBy.String() != "@operator:bureau.local" {
		t.Errorf("ProvisionedBy = %q, want %q", credentials.ProvisionedBy, "@operator:bureau.local")
	}
	if credentials.ProvisionedAt == "" {
		t.Error("ProvisionedAt is empty")
	}
}

func TestFleetProvision_HappyPathWithEscrowKey(t *testing.T) {
	t.Parallel()

	machineKeypair := testKeypair(t)
	escrowKeypair := testKeypair(t)

	session := &mockSession{
		userID: mustUserID("@operator:bureau.local"),
		getRoomState: func(_ context.Context, _ ref.RoomID) ([]messaging.Event, error) {
			return []messaging.Event{
				makeMachineKeyEvent("my_bureau/fleet/prod/machine/solo", "age-x25519", machineKeypair.PublicKey),
			}, nil
		},
		sendStateEvent: func(_ context.Context, _ ref.RoomID, _ ref.EventType, _ string, _ any) (ref.EventID, error) {
			return ref.MustParseEventID("$escrow-event"), nil
		},
	}

	result, err := FleetProvision(context.Background(), session, FleetProvisionParams{
		Fleet:         testFleet(t),
		TargetRoomID:  mustRoomID("!target:bureau.local"),
		StateKey:      "builder",
		MachineRoomID: mustRoomID("!machine:bureau.local"),
		EscrowKey:     escrowKeypair.PublicKey,
		Credentials:   map[string]string{"TOKEN": "secret"},
	})
	if err != nil {
		t.Fatalf("FleetProvision: %v", err)
	}

	// EncryptedFor should include machine + escrow.
	if len(result.EncryptedFor) != 2 {
		t.Fatalf("EncryptedFor length = %d, want 2", len(result.EncryptedFor))
	}
	if !strings.Contains(result.EncryptedFor[0], "solo") {
		t.Errorf("EncryptedFor[0] = %q, want to contain 'solo'", result.EncryptedFor[0])
	}
	if result.EncryptedFor[1] != "escrow:operator" {
		t.Errorf("EncryptedFor[1] = %q, want %q", result.EncryptedFor[1], "escrow:operator")
	}
	if result.MachineCount != 1 {
		t.Errorf("MachineCount = %d, want 1 (escrow not counted)", result.MachineCount)
	}
}

func TestFleetProvision_InvalidEscrowKey(t *testing.T) {
	t.Parallel()

	machineKeypair := testKeypair(t)

	session := &mockSession{
		userID: mustUserID("@operator:bureau.local"),
		getRoomState: func(_ context.Context, _ ref.RoomID) ([]messaging.Event, error) {
			return []messaging.Event{
				makeMachineKeyEvent("my_bureau/fleet/prod/machine/solo", "age-x25519", machineKeypair.PublicKey),
			}, nil
		},
	}

	_, err := FleetProvision(context.Background(), session, FleetProvisionParams{
		Fleet:         testFleet(t),
		TargetRoomID:  mustRoomID("!target:bureau.local"),
		StateKey:      "builder",
		MachineRoomID: mustRoomID("!machine:bureau.local"),
		EscrowKey:     "not-a-valid-escrow-key",
		Credentials:   map[string]string{"TOKEN": "secret"},
	})
	if err == nil {
		t.Fatal("expected error for invalid escrow key, got nil")
	}
	if !strings.Contains(err.Error(), "invalid escrow key") {
		t.Errorf("error = %q, want substring %q", err.Error(), "invalid escrow key")
	}
}

func TestFleetProvision_SendStateEventFails(t *testing.T) {
	t.Parallel()

	machineKeypair := testKeypair(t)

	session := &mockSession{
		userID: mustUserID("@operator:bureau.local"),
		getRoomState: func(_ context.Context, _ ref.RoomID) ([]messaging.Event, error) {
			return []messaging.Event{
				makeMachineKeyEvent("my_bureau/fleet/prod/machine/solo", "age-x25519", machineKeypair.PublicKey),
			}, nil
		},
		sendStateEvent: func(_ context.Context, _ ref.RoomID, _ ref.EventType, _ string, _ any) (ref.EventID, error) {
			return ref.EventID{}, fmt.Errorf("permission denied")
		},
	}

	_, err := FleetProvision(context.Background(), session, FleetProvisionParams{
		Fleet:         testFleet(t),
		TargetRoomID:  mustRoomID("!target:bureau.local"),
		StateKey:      "builder",
		MachineRoomID: mustRoomID("!machine:bureau.local"),
		Credentials:   map[string]string{"TOKEN": "secret"},
	})
	if err == nil {
		t.Fatal("expected error when SendStateEvent fails, got nil")
	}
	if !strings.Contains(err.Error(), "publishing fleet credentials") {
		t.Errorf("error = %q, want substring %q", err.Error(), "publishing fleet credentials")
	}
}

// --- enumerateMachineKeys tests ---

func TestEnumerateMachineKeys_FiltersCorrectly(t *testing.T) {
	t.Parallel()

	keypairGood := testKeypair(t)

	session := &mockSession{
		userID: mustUserID("@operator:bureau.local"),
		getRoomState: func(_ context.Context, _ ref.RoomID) ([]messaging.Event, error) {
			emptyStateKey := ""
			return []messaging.Event{
				// Non-machine-key event (should be skipped).
				{Type: "m.room.create", StateKey: &emptyStateKey, Content: map[string]any{}},
				// Valid machine key.
				makeMachineKeyEvent("machine/good", "age-x25519", keypairGood.PublicKey),
				// Machine with nil StateKey (should be skipped).
				{Type: schema.EventTypeMachineKey, StateKey: nil, Content: map[string]any{}},
				// Empty public key (decommissioned, should be skipped).
				makeMachineKeyEvent("machine/decommissioned", "age-x25519", ""),
				// Wrong algorithm (should be skipped).
				makeMachineKeyEvent("machine/rsa", "rsa-4096", "not-age"),
				// Invalid public key format (should be skipped).
				makeMachineKeyEvent("machine/badkey", "age-x25519", "not-a-valid-age-key"),
			}, nil
		},
	}

	keys, err := enumerateMachineKeys(context.Background(), session, mustRoomID("!machine:bureau.local"))
	if err != nil {
		t.Fatalf("enumerateMachineKeys: %v", err)
	}

	if len(keys) != 1 {
		t.Fatalf("expected 1 key, got %d: %v", len(keys), keys)
	}

	key, exists := keys["machine/good"]
	if !exists {
		t.Fatal("expected key for 'machine/good'")
	}
	if key.PublicKey != keypairGood.PublicKey {
		t.Errorf("key.PublicKey = %q, want %q", key.PublicKey, keypairGood.PublicKey)
	}
}

// --- Credential event content: fleet credentials omit Principal field ---

func TestFleetProvision_CredentialEventOmitsPrincipal(t *testing.T) {
	t.Parallel()

	machineKeypair := testKeypair(t)

	var capturedContent any
	session := &mockSession{
		userID: mustUserID("@operator:bureau.local"),
		getRoomState: func(_ context.Context, _ ref.RoomID) ([]messaging.Event, error) {
			return []messaging.Event{
				makeMachineKeyEvent("my_bureau/fleet/prod/machine/solo", "age-x25519", machineKeypair.PublicKey),
			}, nil
		},
		sendStateEvent: func(_ context.Context, _ ref.RoomID, _ ref.EventType, _ string, content any) (ref.EventID, error) {
			capturedContent = content
			return ref.MustParseEventID("$ev"), nil
		},
	}

	_, err := FleetProvision(context.Background(), session, FleetProvisionParams{
		Fleet:         testFleet(t),
		TargetRoomID:  mustRoomID("!target:bureau.local"),
		StateKey:      "builder",
		MachineRoomID: mustRoomID("!machine:bureau.local"),
		Credentials:   map[string]string{"TOKEN": "secret"},
	})
	if err != nil {
		t.Fatalf("FleetProvision: %v", err)
	}

	// Fleet credentials are not principal-scoped, so the Principal
	// field should be zero (the credential_ref state key identifies
	// the bundle, not a specific principal).
	credentials, ok := capturedContent.(schema.Credentials)
	if !ok {
		t.Fatalf("content type = %T, want schema.Credentials", capturedContent)
	}

	// Marshal to JSON to verify the principal field is omitted or zero.
	data, marshalErr := json.Marshal(credentials)
	if marshalErr != nil {
		t.Fatalf("Marshal: %v", marshalErr)
	}
	var raw map[string]any
	if unmarshalErr := json.Unmarshal(data, &raw); unmarshalErr != nil {
		t.Fatalf("Unmarshal: %v", unmarshalErr)
	}

	// The Principal field should be empty string or absent since it's
	// a zero-value UserID.
	principalValue, exists := raw["principal"]
	if exists && principalValue != "" {
		t.Errorf("expected empty/absent principal for fleet credentials, got %q", principalValue)
	}
}
