// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package credential

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/sealed"
	"github.com/bureau-foundation/bureau/lib/secret"
	"github.com/bureau-foundation/bureau/messaging"
)

// ResealParams holds the parameters for re-encrypting a fleet credential
// bundle to an updated set of machine recipients.
//
// Re-sealing is needed when fleet membership changes: a machine is added
// or removed, and the credential bundle must be re-encrypted so that
// only current fleet members (plus escrow) can decrypt it. The operator
// provides the escrow private key to decrypt the existing bundle, and
// the function re-encrypts to all current machine keys in the fleet.
type ResealParams struct {
	// Fleet identifies the target fleet. Used for machine enumeration
	// and identity labeling in the EncryptedFor list.
	Fleet ref.Fleet

	// TargetRoomID is the Matrix room ID containing the
	// m.bureau.credentials state event to reseal.
	TargetRoomID ref.RoomID

	// StateKey is the state key of the credential event to reseal.
	StateKey string

	// MachineRoomID is the Matrix room ID of the fleet's machine room
	// for enumerating current machine public keys.
	MachineRoomID ref.RoomID

	// EscrowPrivateKey is the operator's escrow private key, used to
	// decrypt the existing credential bundle. Must be a valid age
	// private key in AGE-SECRET-KEY-1... format, stored in a
	// secret.Buffer.
	//
	// The caller is responsible for closing this buffer after Reseal
	// returns. Reseal borrows it for decryption but does not close it.
	EscrowPrivateKey *secret.Buffer

	// EscrowPublicKey is the operator's escrow public key, included in
	// the re-encrypted bundle's recipient list. Must correspond to
	// EscrowPrivateKey.
	EscrowPublicKey string
}

// ResealResult holds the result of a successful credential reseal.
type ResealResult struct {
	// EventID is the Matrix event ID of the updated state event.
	EventID ref.EventID

	// EncryptedFor lists all recipients in the re-sealed bundle.
	EncryptedFor []string

	// Keys lists the credential names in the bundle (unchanged).
	Keys []string

	// MachineCount is the number of fleet machines in the new
	// recipient list (excluding the escrow key).
	MachineCount int

	// PreviousMachineCount is the number of machines in the old
	// recipient list (inferred from EncryptedFor, excluding escrow).
	PreviousMachineCount int
}

// Reseal reads an existing fleet credential event, decrypts it with the
// escrow private key, enumerates current fleet machines, re-encrypts to
// the updated recipient list, and publishes the updated event.
//
// This is the operational primitive for fleet credential maintenance:
//   - After adding a machine: reseal adds the new machine's key
//   - After removing a machine: reseal removes the old machine's key
//   - During key rotation: reseal updates the recipient list
//
// The existing credential event must have been encrypted with the escrow
// key (i.e., the escrow key must be in the age header's recipient list).
// If the event doesn't exist or can't be decrypted, an error is returned.
func Reseal(ctx context.Context, session messaging.Session, params ResealParams) (*ResealResult, error) {
	if params.Fleet.IsZero() {
		return nil, fmt.Errorf("fleet is required")
	}
	if params.TargetRoomID.IsZero() {
		return nil, fmt.Errorf("target room ID is required")
	}
	if params.StateKey == "" {
		return nil, fmt.Errorf("state key is required")
	}
	if params.MachineRoomID.IsZero() {
		return nil, fmt.Errorf("machine room ID is required")
	}
	if params.EscrowPrivateKey == nil {
		return nil, fmt.Errorf("escrow private key is required")
	}
	if params.EscrowPublicKey == "" {
		return nil, fmt.Errorf("escrow public key is required")
	}
	if err := sealed.ParsePublicKey(params.EscrowPublicKey); err != nil {
		return nil, fmt.Errorf("invalid escrow public key: %w", err)
	}

	// Read the existing credential event.
	existing, err := messaging.GetState[schema.Credentials](
		ctx, session, params.TargetRoomID,
		schema.EventTypeCredentials, params.StateKey,
	)
	if err != nil {
		return nil, fmt.Errorf("reading credential event %q from %s: %w",
			params.StateKey, params.TargetRoomID, err)
	}
	if existing.Ciphertext == "" {
		return nil, fmt.Errorf("credential event %q has empty ciphertext", params.StateKey)
	}

	// Count previous machines (EncryptedFor entries minus escrow).
	previousMachineCount := 0
	for _, entry := range existing.EncryptedFor {
		if entry != "escrow:operator" {
			previousMachineCount++
		}
	}

	// Decrypt using the escrow private key.
	plaintext, err := sealed.DecryptJSON(existing.Ciphertext, params.EscrowPrivateKey)
	if err != nil {
		return nil, fmt.Errorf("decrypting credential event %q: %w (is the escrow key correct?)",
			params.StateKey, err)
	}
	defer plaintext.Close()

	// Parse the plaintext to extract credential keys.
	var credentials map[string]string
	if err := json.Unmarshal(plaintext.Bytes(), &credentials); err != nil {
		return nil, fmt.Errorf("parsing decrypted credentials for %q: %w", params.StateKey, err)
	}

	credentialKeys := make([]string, 0, len(credentials))
	for key := range credentials {
		credentialKeys = append(credentialKeys, key)
	}
	sort.Strings(credentialKeys)

	// Enumerate current fleet machine keys.
	machineKeys, err := enumerateMachineKeys(ctx, session, params.MachineRoomID)
	if err != nil {
		return nil, fmt.Errorf("enumerating fleet machine keys: %w", err)
	}
	if len(machineKeys) == 0 {
		return nil, fmt.Errorf("no machines with valid keys found in fleet machine room %s", params.MachineRoomID)
	}

	// Build recipient list: sorted machine keys + escrow.
	machineLocalparts := make([]string, 0, len(machineKeys))
	for localpart := range machineKeys {
		machineLocalparts = append(machineLocalparts, localpart)
	}
	sort.Strings(machineLocalparts)

	recipientKeys := make([]string, 0, len(machineKeys)+1)
	encryptedFor := make([]string, 0, len(machineKeys)+1)

	for _, localpart := range machineLocalparts {
		key := machineKeys[localpart]
		recipientKeys = append(recipientKeys, key.PublicKey)
		machineRef, parseErr := ref.ParseMachine(localpart, params.Fleet.Server())
		if parseErr != nil {
			return nil, fmt.Errorf("constructing machine ref for %q: %w", localpart, parseErr)
		}
		encryptedFor = append(encryptedFor, machineRef.UserID().String())
	}

	recipientKeys = append(recipientKeys, params.EscrowPublicKey)
	encryptedFor = append(encryptedFor, "escrow:operator")

	machineCount := len(machineKeys)

	// Re-encrypt the credential bundle.
	ciphertext, err := sealed.EncryptJSON(plaintext.Bytes(), recipientKeys)
	if err != nil {
		return nil, fmt.Errorf("re-encrypting credentials for %d machines: %w", machineCount, err)
	}

	// Publish the updated credential event. Preserve the original
	// principal and keys from the existing event; update ciphertext,
	// EncryptedFor, and provisioning metadata.
	updatedEvent := schema.Credentials{
		Version:       existing.Version,
		Principal:     existing.Principal,
		EncryptedFor:  encryptedFor,
		Keys:          credentialKeys,
		Ciphertext:    ciphertext,
		ProvisionedBy: session.UserID(),
		ProvisionedAt: time.Now().UTC().Format(time.RFC3339),
	}

	eventID, err := session.SendStateEvent(ctx, params.TargetRoomID, schema.EventTypeCredentials, params.StateKey, updatedEvent)
	if err != nil {
		return nil, fmt.Errorf("publishing resealed credentials to %s: %w", params.TargetRoomID, err)
	}

	return &ResealResult{
		EventID:              eventID,
		EncryptedFor:         encryptedFor,
		Keys:                 credentialKeys,
		MachineCount:         machineCount,
		PreviousMachineCount: previousMachineCount,
	}, nil
}
