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
	"github.com/bureau-foundation/bureau/messaging"
)

// FleetProvisionParams holds the parameters for encrypting and publishing
// a credential bundle to a fleet-scoped room, encrypted to all machines
// in the fleet.
//
// Unlike single-machine Provision, which encrypts to one machine's key
// and publishes to that machine's config room, FleetProvision encrypts
// to every machine in the fleet simultaneously. This is used for
// template-bound credentials (credential_ref) that any machine in the
// fleet may need to decrypt at sandbox creation time.
type FleetProvisionParams struct {
	// Fleet identifies the target fleet. Used for machine enumeration
	// and identity labeling in the EncryptedFor list.
	Fleet ref.Fleet

	// TargetRoomID is the Matrix room ID where the m.bureau.credentials
	// state event will be published. This is typically a dedicated
	// credentials room referenced by template credential_ref fields.
	// The room must already exist and the session must have permission
	// to write state events to it.
	TargetRoomID ref.RoomID

	// StateKey is the state key for the m.bureau.credentials event.
	// Identifies the credential bundle within the room (e.g., "builder"
	// for a nix-builder template's credentials). This is the value
	// that appears after the colon in a credential_ref
	// (e.g., "bureau/creds:builder" → state key "builder").
	StateKey string

	// MachineRoomID is the Matrix room ID of the fleet's machine room
	// where machine public keys are published as m.bureau.machine_key
	// state events. All machines with valid keys in this room will be
	// included as encryption recipients.
	MachineRoomID ref.RoomID

	// EscrowKey is an optional operator escrow age public key. When
	// set, the credential bundle is encrypted to all machine keys
	// plus the escrow key. The escrow key enables re-seal operations
	// when fleet membership changes: the operator decrypts with the
	// escrow private key and re-encrypts to the updated machine list.
	//
	// For fleet credentials, an escrow key is strongly recommended.
	// Without one, adding a new machine to the fleet requires the
	// operator to re-provision all fleet credentials from plaintext
	// source material, rather than re-sealing the existing bundle.
	EscrowKey string

	// Credentials maps credential names to their values. These are the
	// secrets that will be encrypted (e.g., "ATTIC_PUSH_TOKEN" → "...",
	// "CACHIX_AUTH_TOKEN" → "..."). Must contain at least one entry.
	Credentials map[string]string
}

// FleetProvisionResult holds the result of a successful fleet credential
// provisioning.
type FleetProvisionResult struct {
	// EventID is the Matrix event ID of the published m.bureau.credentials
	// state event.
	EventID ref.EventID

	// TargetRoomID is the room where the credentials were published.
	TargetRoomID ref.RoomID

	// EncryptedFor lists all recipients that can decrypt the bundle.
	// Each entry is a machine user ID (e.g.,
	// "@bureau/fleet/prod/machine/gpu-box:server") or "escrow:operator".
	EncryptedFor []string

	// Keys lists the credential names (not values) in the bundle.
	Keys []string

	// MachineCount is the number of fleet machines the bundle was
	// encrypted to (excluding the escrow key).
	MachineCount int
}

// FleetProvision encrypts a credential bundle to all machines in a fleet
// and publishes it as an m.bureau.credentials state event to the target
// room.
//
// Machine public keys are enumerated from m.bureau.machine_key state
// events in the fleet's machine room (params.MachineRoomID). Machines
// with empty public keys (decommissioned) are skipped. The fleet must
// have at least one machine with a valid key.
//
// The session must have permission to read the machine room (for public
// keys) and write state events to the target room.
func FleetProvision(ctx context.Context, session messaging.Session, params FleetProvisionParams) (*FleetProvisionResult, error) {
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
	if len(params.Credentials) == 0 {
		return nil, fmt.Errorf("credentials map is empty")
	}

	// Collect and sort credential key names for deterministic output.
	credentialKeys := make([]string, 0, len(params.Credentials))
	for key := range params.Credentials {
		credentialKeys = append(credentialKeys, key)
	}
	sort.Strings(credentialKeys)

	// Marshal the credential bundle to JSON.
	credentialJSON, err := json.Marshal(params.Credentials)
	if err != nil {
		return nil, fmt.Errorf("marshaling credentials: %w", err)
	}

	// Enumerate machine keys from the fleet's machine room.
	machineKeys, err := enumerateMachineKeys(ctx, session, params.MachineRoomID)
	if err != nil {
		return nil, fmt.Errorf("enumerating fleet machine keys: %w", err)
	}
	if len(machineKeys) == 0 {
		return nil, fmt.Errorf("no machines with valid keys found in fleet machine room %s", params.MachineRoomID)
	}

	// Build the recipient list: all machine keys + optional escrow key.
	recipientKeys := make([]string, 0, len(machineKeys)+1)
	encryptedFor := make([]string, 0, len(machineKeys)+1)

	// Sort by machine localpart for deterministic recipient ordering.
	machineLocalparts := make([]string, 0, len(machineKeys))
	for localpart := range machineKeys {
		machineLocalparts = append(machineLocalparts, localpart)
	}
	sort.Strings(machineLocalparts)

	for _, localpart := range machineLocalparts {
		key := machineKeys[localpart]
		recipientKeys = append(recipientKeys, key.PublicKey)
		// Construct the machine user ID for the EncryptedFor list.
		machineRef, parseErr := ref.ParseMachine(localpart, params.Fleet.Server())
		if parseErr != nil {
			return nil, fmt.Errorf("constructing machine ref for %q: %w", localpart, parseErr)
		}
		encryptedFor = append(encryptedFor, machineRef.UserID().String())
	}

	machineCount := len(machineKeys)

	if params.EscrowKey != "" {
		if err := sealed.ParsePublicKey(params.EscrowKey); err != nil {
			return nil, fmt.Errorf("invalid escrow key: %w", err)
		}
		recipientKeys = append(recipientKeys, params.EscrowKey)
		encryptedFor = append(encryptedFor, "escrow:operator")
	}

	// Encrypt the credential bundle to all recipients.
	ciphertext, err := sealed.EncryptJSON(credentialJSON, recipientKeys)
	if err != nil {
		return nil, fmt.Errorf("encrypting credentials for %d machines: %w", machineCount, err)
	}

	// Publish the credentials state event.
	credentialEvent := schema.Credentials{
		Version:       schema.CredentialsVersion,
		EncryptedFor:  encryptedFor,
		Keys:          credentialKeys,
		Ciphertext:    ciphertext,
		ProvisionedBy: session.UserID(),
		ProvisionedAt: time.Now().UTC().Format(time.RFC3339),
	}

	eventID, err := session.SendStateEvent(ctx, params.TargetRoomID, schema.EventTypeCredentials, params.StateKey, credentialEvent)
	if err != nil {
		return nil, fmt.Errorf("publishing fleet credentials to %s: %w", params.TargetRoomID, err)
	}

	return &FleetProvisionResult{
		EventID:      eventID,
		TargetRoomID: params.TargetRoomID,
		EncryptedFor: encryptedFor,
		Keys:         credentialKeys,
		MachineCount: machineCount,
	}, nil
}

// enumerateMachineKeys reads all m.bureau.machine_key state events from
// a fleet's machine room and returns a map from machine localpart to
// MachineKey. Machines with empty public keys (decommissioned) or
// unsupported algorithms are skipped.
func enumerateMachineKeys(ctx context.Context, session messaging.Session, machineRoomID ref.RoomID) (map[string]schema.MachineKey, error) {
	events, err := session.GetRoomState(ctx, machineRoomID)
	if err != nil {
		return nil, fmt.Errorf("fetching room state from %s: %w", machineRoomID, err)
	}

	machineKeys := make(map[string]schema.MachineKey)
	for _, event := range events {
		if event.Type != schema.EventTypeMachineKey || event.StateKey == nil {
			continue
		}

		// Re-marshal the content map to JSON for type-safe deserialization.
		contentJSON, marshalErr := json.Marshal(event.Content)
		if marshalErr != nil {
			continue
		}

		var key schema.MachineKey
		if unmarshalErr := json.Unmarshal(contentJSON, &key); unmarshalErr != nil {
			continue
		}

		// Skip decommissioned machines (empty public key) and unsupported
		// algorithms.
		if key.PublicKey == "" || key.Algorithm != "age-x25519" {
			continue
		}

		// Validate the public key format before including it.
		if validateErr := sealed.ParsePublicKey(key.PublicKey); validateErr != nil {
			continue
		}

		machineKeys[*event.StateKey] = key
	}

	return machineKeys, nil
}
