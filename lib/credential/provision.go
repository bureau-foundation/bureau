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

// ProvisionParams holds the parameters for encrypting and publishing a
// credential bundle to a machine's config room.
type ProvisionParams struct {
	// Machine identifies the target machine.
	Machine ref.Machine

	// Principal identifies the target principal as a fleet-scoped entity
	// reference. The Entity's UserID() is used as the MATRIX_USER_ID in the
	// credential bundle, and Localpart() is used as the state event key.
	Principal ref.Entity

	// MachineRoomID is the Matrix room ID of the fleet's machine room
	// where the machine's public key is published. This is the
	// fleet-scoped machine room (e.g., the room behind
	// #bureau/fleet/prod/machine:server).
	MachineRoomID ref.RoomID

	// EscrowKey is an optional operator escrow age public key. When set,
	// the credential bundle is encrypted to both the machine key and the
	// escrow key, allowing the operator to recover credentials without
	// the machine's private key.
	EscrowKey string

	// Credentials maps credential names to their values. These are the
	// secrets that will be encrypted (e.g., "MATRIX_TOKEN" → "syt_...").
	// Must contain at least one entry.
	Credentials map[string]string
}

// ProvisionResult holds the result of a successful credential provisioning.
type ProvisionResult struct {
	// EventID is the Matrix event ID of the published m.bureau.credentials
	// state event.
	EventID ref.EventID

	// ConfigRoomID is the Matrix room ID of the machine's config room
	// where the credentials were published.
	ConfigRoomID ref.RoomID

	// PrincipalID is the full Matrix user ID of the principal
	// (e.g., "@iree/amdgpu/pm:bureau.local").
	PrincipalID ref.UserID

	// EncryptedFor lists the identities that can decrypt this bundle
	// (machine user ID, plus escrow identifier if an escrow key was provided).
	EncryptedFor []string

	// Keys lists the credential names (not values) in the bundle.
	Keys []string
}

// Provision encrypts a credential bundle with the machine's age public key
// and publishes it as an m.bureau.credentials state event to the machine's
// config room.
//
// The machine's public key is fetched from the m.bureau.machine_key state
// event in the fleet's machine room (params.MachineRoomID). The config
// room is resolved from the machine's config room alias.
//
// The session must have permission to read the machine room (for the public
// key) and write state events to the config room.
func Provision(ctx context.Context, session messaging.Session, params ProvisionParams) (*ProvisionResult, error) {
	if params.Machine.IsZero() {
		return nil, fmt.Errorf("machine is required")
	}
	if params.Principal.IsZero() {
		return nil, fmt.Errorf("principal is required")
	}
	if len(params.Credentials) == 0 {
		return nil, fmt.Errorf("credentials map is empty")
	}
	if params.MachineRoomID.IsZero() {
		return nil, fmt.Errorf("machine room ID is required")
	}

	machineLocalpart := params.Machine.Localpart()

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

	// Fetch the machine's public key from the fleet machine room.
	machineKeyContent, err := session.GetStateEvent(ctx, params.MachineRoomID, schema.EventTypeMachineKey, machineLocalpart)
	if err != nil {
		return nil, fmt.Errorf("fetching machine key for %q: %w", machineLocalpart, err)
	}

	var machineKey schema.MachineKey
	if err := json.Unmarshal(machineKeyContent, &machineKey); err != nil {
		return nil, fmt.Errorf("parsing machine key: %w", err)
	}

	if machineKey.Algorithm != "age-x25519" {
		return nil, fmt.Errorf("unsupported machine key algorithm: %q (expected age-x25519)", machineKey.Algorithm)
	}
	if err := sealed.ParsePublicKey(machineKey.PublicKey); err != nil {
		return nil, fmt.Errorf("invalid machine public key: %w", err)
	}

	// Build the recipient list: machine key + optional escrow key.
	recipientKeys := []string{machineKey.PublicKey}
	encryptedFor := []string{params.Machine.UserID().String()}

	if params.EscrowKey != "" {
		if err := sealed.ParsePublicKey(params.EscrowKey); err != nil {
			return nil, fmt.Errorf("invalid escrow key: %w", err)
		}
		recipientKeys = append(recipientKeys, params.EscrowKey)
		encryptedFor = append(encryptedFor, "escrow:operator")
	}

	// Encrypt the credential bundle.
	ciphertext, err := sealed.EncryptJSON(credentialJSON, recipientKeys)
	if err != nil {
		return nil, fmt.Errorf("encrypting credentials: %w", err)
	}

	// Resolve the config room. The room must already exist (created by
	// bureau machine provision or the daemon's first-boot flow).
	configRoomID, err := session.ResolveAlias(ctx, params.Machine.RoomAlias())
	if err != nil {
		return nil, fmt.Errorf("resolving config room %q: %w", params.Machine.RoomAlias(), err)
	}

	// Publish the credentials state event. The state key is the
	// fleet-scoped localpart, matching how the daemon reads credentials.
	principalUserID := params.Principal.UserID()
	credentialEvent := schema.Credentials{
		Version:       schema.CredentialsVersion,
		Principal:     principalUserID,
		EncryptedFor:  encryptedFor,
		Keys:          credentialKeys,
		Ciphertext:    ciphertext,
		ProvisionedBy: session.UserID(),
		ProvisionedAt: time.Now().UTC().Format(time.RFC3339),
	}

	stateKey := params.Principal.Localpart()
	eventID, err := session.SendStateEvent(ctx, configRoomID, schema.EventTypeCredentials, stateKey, credentialEvent)
	if err != nil {
		return nil, fmt.Errorf("publishing credentials for %q: %w", principalUserID, err)
	}

	return &ProvisionResult{
		EventID:      eventID,
		ConfigRoomID: configRoomID,
		PrincipalID:  principalUserID,
		EncryptedFor: encryptedFor,
		Keys:         credentialKeys,
	}, nil
}
