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
	"github.com/bureau-foundation/bureau/lib/sealed"
	"github.com/bureau-foundation/bureau/messaging"
)

// makeEncryptedCredentialEvent creates a real encrypted credential event
// suitable for reseal testing. The credentials are encrypted to the given
// recipient keys.
func makeEncryptedCredentialEvent(t *testing.T, recipientKeys []string, encryptedFor []string, credentials map[string]string) schema.Credentials {
	t.Helper()

	credentialJSON, err := json.Marshal(credentials)
	if err != nil {
		t.Fatalf("marshaling credentials: %v", err)
	}

	ciphertext, err := sealed.EncryptJSON(credentialJSON, recipientKeys)
	if err != nil {
		t.Fatalf("encrypting credentials: %v", err)
	}

	keys := make([]string, 0, len(credentials))
	for key := range credentials {
		keys = append(keys, key)
	}

	return schema.Credentials{
		Version:       schema.CredentialsVersion,
		EncryptedFor:  encryptedFor,
		Keys:          keys,
		Ciphertext:    ciphertext,
		ProvisionedBy: mustUserID("@operator:bureau.local"),
		ProvisionedAt: "2026-03-01T10:00:00Z",
	}
}

// --- Reseal validation tests ---

func TestReseal_MissingFleet(t *testing.T) {
	t.Parallel()
	session := &mockSession{userID: mustUserID("@operator:bureau.local")}
	escrowKeypair := testKeypair(t)
	_, err := Reseal(context.Background(), session, ResealParams{
		TargetRoomID:     mustRoomID("!target:bureau.local"),
		StateKey:         "builder",
		MachineRoomID:    mustRoomID("!machine:bureau.local"),
		EscrowPrivateKey: escrowKeypair.PrivateKey,
		EscrowPublicKey:  escrowKeypair.PublicKey,
	})
	if err == nil {
		t.Fatal("expected error for missing fleet")
	}
	if got := err.Error(); got != "fleet is required" {
		t.Errorf("error = %q, want %q", got, "fleet is required")
	}
}

func TestReseal_MissingEscrowPrivateKey(t *testing.T) {
	t.Parallel()
	session := &mockSession{userID: mustUserID("@operator:bureau.local")}
	escrowKeypair := testKeypair(t)
	_, err := Reseal(context.Background(), session, ResealParams{
		Fleet:           testFleet(t),
		TargetRoomID:    mustRoomID("!target:bureau.local"),
		StateKey:        "builder",
		MachineRoomID:   mustRoomID("!machine:bureau.local"),
		EscrowPublicKey: escrowKeypair.PublicKey,
	})
	if err == nil {
		t.Fatal("expected error for missing escrow private key")
	}
	if got := err.Error(); got != "escrow private key is required" {
		t.Errorf("error = %q, want %q", got, "escrow private key is required")
	}
}

func TestReseal_MissingEscrowPublicKey(t *testing.T) {
	t.Parallel()
	session := &mockSession{userID: mustUserID("@operator:bureau.local")}
	escrowKeypair := testKeypair(t)
	_, err := Reseal(context.Background(), session, ResealParams{
		Fleet:            testFleet(t),
		TargetRoomID:     mustRoomID("!target:bureau.local"),
		StateKey:         "builder",
		MachineRoomID:    mustRoomID("!machine:bureau.local"),
		EscrowPrivateKey: escrowKeypair.PrivateKey,
	})
	if err == nil {
		t.Fatal("expected error for missing escrow public key")
	}
	if got := err.Error(); got != "escrow public key is required" {
		t.Errorf("error = %q, want %q", got, "escrow public key is required")
	}
}

func TestReseal_InvalidEscrowPublicKey(t *testing.T) {
	t.Parallel()
	session := &mockSession{userID: mustUserID("@operator:bureau.local")}
	escrowKeypair := testKeypair(t)
	_, err := Reseal(context.Background(), session, ResealParams{
		Fleet:            testFleet(t),
		TargetRoomID:     mustRoomID("!target:bureau.local"),
		StateKey:         "builder",
		MachineRoomID:    mustRoomID("!machine:bureau.local"),
		EscrowPrivateKey: escrowKeypair.PrivateKey,
		EscrowPublicKey:  "not-a-valid-key",
	})
	if err == nil {
		t.Fatal("expected error for invalid escrow public key")
	}
	if !strings.Contains(err.Error(), "invalid escrow public key") {
		t.Errorf("error = %q, want substring %q", err.Error(), "invalid escrow public key")
	}
}

// --- Reseal credential read/decrypt tests ---

func TestReseal_CredentialEventNotFound(t *testing.T) {
	t.Parallel()
	escrowKeypair := testKeypair(t)
	session := &mockSession{
		userID: mustUserID("@operator:bureau.local"),
		getStateEvent: func(_ context.Context, _ ref.RoomID, _ ref.EventType, _ string) (json.RawMessage, error) {
			return nil, &messaging.MatrixError{
				Code:       messaging.ErrCodeNotFound,
				Message:    "Event not found",
				StatusCode: 404,
			}
		},
	}
	_, err := Reseal(context.Background(), session, ResealParams{
		Fleet:            testFleet(t),
		TargetRoomID:     mustRoomID("!target:bureau.local"),
		StateKey:         "builder",
		MachineRoomID:    mustRoomID("!machine:bureau.local"),
		EscrowPrivateKey: escrowKeypair.PrivateKey,
		EscrowPublicKey:  escrowKeypair.PublicKey,
	})
	if err == nil {
		t.Fatal("expected error for missing credential event")
	}
	if !strings.Contains(err.Error(), "reading credential event") {
		t.Errorf("error = %q, want substring %q", err.Error(), "reading credential event")
	}
}

func TestReseal_DecryptionFailsWithWrongKey(t *testing.T) {
	t.Parallel()

	// Encrypt to machine key only (no escrow).
	machineKeypair := testKeypair(t)
	wrongEscrowKeypair := testKeypair(t)

	credEvent := makeEncryptedCredentialEvent(t,
		[]string{machineKeypair.PublicKey},
		[]string{"@machine:bureau.local"},
		map[string]string{"TOKEN": "secret"},
	)

	credJSON, _ := json.Marshal(credEvent)
	session := &mockSession{
		userID: mustUserID("@operator:bureau.local"),
		getStateEvent: func(_ context.Context, _ ref.RoomID, _ ref.EventType, _ string) (json.RawMessage, error) {
			return credJSON, nil
		},
	}

	_, err := Reseal(context.Background(), session, ResealParams{
		Fleet:            testFleet(t),
		TargetRoomID:     mustRoomID("!target:bureau.local"),
		StateKey:         "builder",
		MachineRoomID:    mustRoomID("!machine:bureau.local"),
		EscrowPrivateKey: wrongEscrowKeypair.PrivateKey,
		EscrowPublicKey:  wrongEscrowKeypair.PublicKey,
	})
	if err == nil {
		t.Fatal("expected error when decryption fails with wrong key")
	}
	if !strings.Contains(err.Error(), "decrypting credential event") {
		t.Errorf("error = %q, want substring %q", err.Error(), "decrypting credential event")
	}
}

// --- Reseal happy path ---

func TestReseal_HappyPath_AddMachine(t *testing.T) {
	t.Parallel()

	// Initial setup: 1 machine + escrow.
	machineKeypairA := testKeypair(t)
	escrowKeypair := testKeypair(t)

	// New machine that we want to add.
	machineKeypairB := testKeypair(t)

	// Create the initial credential event encrypted to machine A + escrow.
	credEvent := makeEncryptedCredentialEvent(t,
		[]string{machineKeypairA.PublicKey, escrowKeypair.PublicKey},
		[]string{"@my_bureau/fleet/prod/machine/alpha:bureau.local", "escrow:operator"},
		map[string]string{"ATTIC_PUSH_TOKEN": "push-secret", "CACHIX_AUTH_TOKEN": "cachix-secret"},
	)

	credJSON, _ := json.Marshal(credEvent)

	var capturedContent any
	session := &mockSession{
		userID: mustUserID("@operator:bureau.local"),
		// GetStateEvent returns the existing credential event.
		getStateEvent: func(_ context.Context, _ ref.RoomID, eventType ref.EventType, stateKey string) (json.RawMessage, error) {
			if eventType != schema.EventTypeCredentials {
				return nil, fmt.Errorf("unexpected event type: %s", eventType)
			}
			if stateKey != "builder" {
				return nil, fmt.Errorf("unexpected state key: %s", stateKey)
			}
			return credJSON, nil
		},
		// GetRoomState returns both machines.
		getRoomState: func(_ context.Context, _ ref.RoomID) ([]messaging.Event, error) {
			return []messaging.Event{
				makeMachineKeyEvent("my_bureau/fleet/prod/machine/alpha", "age-x25519", machineKeypairA.PublicKey),
				makeMachineKeyEvent("my_bureau/fleet/prod/machine/beta", "age-x25519", machineKeypairB.PublicKey),
			}, nil
		},
		sendStateEvent: func(_ context.Context, _ ref.RoomID, _ ref.EventType, _ string, content any) (ref.EventID, error) {
			capturedContent = content
			return ref.MustParseEventID("$resealed-1"), nil
		},
	}

	result, err := Reseal(context.Background(), session, ResealParams{
		Fleet:            testFleet(t),
		TargetRoomID:     mustRoomID("!target:bureau.local"),
		StateKey:         "builder",
		MachineRoomID:    mustRoomID("!machine:bureau.local"),
		EscrowPrivateKey: escrowKeypair.PrivateKey,
		EscrowPublicKey:  escrowKeypair.PublicKey,
	})
	if err != nil {
		t.Fatalf("Reseal: %v", err)
	}

	// Verify result.
	if result.EventID != ref.MustParseEventID("$resealed-1") {
		t.Errorf("EventID = %q, want %q", result.EventID, "$resealed-1")
	}
	if result.MachineCount != 2 {
		t.Errorf("MachineCount = %d, want 2", result.MachineCount)
	}
	if result.PreviousMachineCount != 1 {
		t.Errorf("PreviousMachineCount = %d, want 1", result.PreviousMachineCount)
	}

	// EncryptedFor should contain both machines + escrow.
	if len(result.EncryptedFor) != 3 {
		t.Fatalf("EncryptedFor length = %d, want 3", len(result.EncryptedFor))
	}
	if !strings.Contains(result.EncryptedFor[0], "alpha") {
		t.Errorf("EncryptedFor[0] = %q, want to contain 'alpha'", result.EncryptedFor[0])
	}
	if !strings.Contains(result.EncryptedFor[1], "beta") {
		t.Errorf("EncryptedFor[1] = %q, want to contain 'beta'", result.EncryptedFor[1])
	}
	if result.EncryptedFor[2] != "escrow:operator" {
		t.Errorf("EncryptedFor[2] = %q, want %q", result.EncryptedFor[2], "escrow:operator")
	}

	// Keys should be preserved and sorted.
	if len(result.Keys) != 2 || result.Keys[0] != "ATTIC_PUSH_TOKEN" || result.Keys[1] != "CACHIX_AUTH_TOKEN" {
		t.Errorf("Keys = %v, want [ATTIC_PUSH_TOKEN, CACHIX_AUTH_TOKEN]", result.Keys)
	}

	// Verify the re-encrypted credential can be decrypted by both
	// the original machine, the new machine, and the escrow key.
	credentials, ok := capturedContent.(schema.Credentials)
	if !ok {
		t.Fatalf("content type = %T, want schema.Credentials", capturedContent)
	}

	// Verify machine A can decrypt.
	plaintextA, errA := sealed.DecryptJSON(credentials.Ciphertext, machineKeypairA.PrivateKey)
	if errA != nil {
		t.Fatalf("machine A cannot decrypt resealed credentials: %v", errA)
	}
	defer plaintextA.Close()

	var decryptedA map[string]string
	if err := json.Unmarshal(plaintextA.Bytes(), &decryptedA); err != nil {
		t.Fatalf("parsing decrypted A: %v", err)
	}
	if decryptedA["ATTIC_PUSH_TOKEN"] != "push-secret" {
		t.Errorf("machine A: ATTIC_PUSH_TOKEN = %q, want %q", decryptedA["ATTIC_PUSH_TOKEN"], "push-secret")
	}

	// Verify machine B (newly added) can decrypt.
	plaintextB, errB := sealed.DecryptJSON(credentials.Ciphertext, machineKeypairB.PrivateKey)
	if errB != nil {
		t.Fatalf("machine B cannot decrypt resealed credentials: %v", errB)
	}
	defer plaintextB.Close()

	var decryptedB map[string]string
	if err := json.Unmarshal(plaintextB.Bytes(), &decryptedB); err != nil {
		t.Fatalf("parsing decrypted B: %v", err)
	}
	if decryptedB["CACHIX_AUTH_TOKEN"] != "cachix-secret" {
		t.Errorf("machine B: CACHIX_AUTH_TOKEN = %q, want %q", decryptedB["CACHIX_AUTH_TOKEN"], "cachix-secret")
	}

	// Verify escrow can still decrypt.
	plaintextEscrow, errEscrow := sealed.DecryptJSON(credentials.Ciphertext, escrowKeypair.PrivateKey)
	if errEscrow != nil {
		t.Fatalf("escrow cannot decrypt resealed credentials: %v", errEscrow)
	}
	defer plaintextEscrow.Close()
}

func TestReseal_HappyPath_RemoveMachine(t *testing.T) {
	t.Parallel()

	// Initial setup: 2 machines + escrow.
	machineKeypairA := testKeypair(t)
	machineKeypairB := testKeypair(t)
	escrowKeypair := testKeypair(t)

	// Create initial credential encrypted to both machines + escrow.
	credEvent := makeEncryptedCredentialEvent(t,
		[]string{machineKeypairA.PublicKey, machineKeypairB.PublicKey, escrowKeypair.PublicKey},
		[]string{
			"@my_bureau/fleet/prod/machine/alpha:bureau.local",
			"@my_bureau/fleet/prod/machine/beta:bureau.local",
			"escrow:operator",
		},
		map[string]string{"TOKEN": "secret"},
	)

	credJSON, _ := json.Marshal(credEvent)

	var capturedContent any
	session := &mockSession{
		userID: mustUserID("@operator:bureau.local"),
		getStateEvent: func(_ context.Context, _ ref.RoomID, _ ref.EventType, _ string) (json.RawMessage, error) {
			return credJSON, nil
		},
		// Only machine A remains in the fleet (B was decommissioned).
		getRoomState: func(_ context.Context, _ ref.RoomID) ([]messaging.Event, error) {
			return []messaging.Event{
				makeMachineKeyEvent("my_bureau/fleet/prod/machine/alpha", "age-x25519", machineKeypairA.PublicKey),
				// Machine B has empty key (decommissioned).
				makeMachineKeyEvent("my_bureau/fleet/prod/machine/beta", "age-x25519", ""),
			}, nil
		},
		sendStateEvent: func(_ context.Context, _ ref.RoomID, _ ref.EventType, _ string, content any) (ref.EventID, error) {
			capturedContent = content
			return ref.MustParseEventID("$resealed-2"), nil
		},
	}

	result, err := Reseal(context.Background(), session, ResealParams{
		Fleet:            testFleet(t),
		TargetRoomID:     mustRoomID("!target:bureau.local"),
		StateKey:         "builder",
		MachineRoomID:    mustRoomID("!machine:bureau.local"),
		EscrowPrivateKey: escrowKeypair.PrivateKey,
		EscrowPublicKey:  escrowKeypair.PublicKey,
	})
	if err != nil {
		t.Fatalf("Reseal: %v", err)
	}

	if result.MachineCount != 1 {
		t.Errorf("MachineCount = %d, want 1", result.MachineCount)
	}
	if result.PreviousMachineCount != 2 {
		t.Errorf("PreviousMachineCount = %d, want 2", result.PreviousMachineCount)
	}

	// Only machine A + escrow in EncryptedFor.
	if len(result.EncryptedFor) != 2 {
		t.Fatalf("EncryptedFor length = %d, want 2", len(result.EncryptedFor))
	}
	if !strings.Contains(result.EncryptedFor[0], "alpha") {
		t.Errorf("EncryptedFor[0] = %q, want to contain 'alpha'", result.EncryptedFor[0])
	}

	// Verify machine B can NO LONGER decrypt.
	credentials := capturedContent.(schema.Credentials)
	_, errB := sealed.DecryptJSON(credentials.Ciphertext, machineKeypairB.PrivateKey)
	if errB == nil {
		t.Error("decommissioned machine B should not be able to decrypt resealed credentials")
	}

	// Machine A can still decrypt.
	plaintextA, errA := sealed.DecryptJSON(credentials.Ciphertext, machineKeypairA.PrivateKey)
	if errA != nil {
		t.Fatalf("machine A cannot decrypt resealed credentials: %v", errA)
	}
	plaintextA.Close()
}

func TestReseal_NoMachinesAfterEnumeration(t *testing.T) {
	t.Parallel()

	escrowKeypair := testKeypair(t)

	// All machines decommissioned.
	credEvent := makeEncryptedCredentialEvent(t,
		[]string{escrowKeypair.PublicKey},
		[]string{"escrow:operator"},
		map[string]string{"TOKEN": "secret"},
	)
	credJSON, _ := json.Marshal(credEvent)

	session := &mockSession{
		userID: mustUserID("@operator:bureau.local"),
		getStateEvent: func(_ context.Context, _ ref.RoomID, _ ref.EventType, _ string) (json.RawMessage, error) {
			return credJSON, nil
		},
		getRoomState: func(_ context.Context, _ ref.RoomID) ([]messaging.Event, error) {
			return []messaging.Event{}, nil
		},
	}

	_, err := Reseal(context.Background(), session, ResealParams{
		Fleet:            testFleet(t),
		TargetRoomID:     mustRoomID("!target:bureau.local"),
		StateKey:         "builder",
		MachineRoomID:    mustRoomID("!machine:bureau.local"),
		EscrowPrivateKey: escrowKeypair.PrivateKey,
		EscrowPublicKey:  escrowKeypair.PublicKey,
	})
	if err == nil {
		t.Fatal("expected error for no machines after enumeration")
	}
	if !strings.Contains(err.Error(), "no machines with valid keys") {
		t.Errorf("error = %q, want substring %q", err.Error(), "no machines with valid keys")
	}
}
