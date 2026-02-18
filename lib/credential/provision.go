// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package credential

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/sealed"
	"github.com/bureau-foundation/bureau/messaging"
)

// ProvisionParams holds the parameters for encrypting and publishing a
// credential bundle to a machine's config room.
type ProvisionParams struct {
	// MachineName is the machine's localpart (e.g., "machine/workstation").
	MachineName string

	// Principal is the principal's localpart (e.g., "iree/amdgpu/pm").
	Principal string

	// ServerName is the Matrix server name (e.g., "bureau.local").
	ServerName string

	// MachineRoomID is the Matrix room ID of the fleet's machine room
	// where the machine's public key is published. This is the
	// fleet-scoped machine room (e.g., the room behind
	// #bureau/fleet/prod/machine:server).
	MachineRoomID string

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
	EventID string

	// ConfigRoomID is the Matrix room ID of the machine's config room
	// where the credentials were published.
	ConfigRoomID string

	// PrincipalID is the full Matrix user ID of the principal
	// (e.g., "@iree/amdgpu/pm:bureau.local").
	PrincipalID string

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
// room is resolved from the standard alias #bureau/config/<machineName>.
//
// The session must have permission to read the machine room (for the public
// key) and write state events to the config room.
func Provision(ctx context.Context, session messaging.Session, params ProvisionParams) (*ProvisionResult, error) {
	if err := principal.ValidateLocalpart(params.MachineName); err != nil {
		return nil, fmt.Errorf("invalid machine name: %w", err)
	}
	if err := principal.ValidateLocalpart(params.Principal); err != nil {
		return nil, fmt.Errorf("invalid principal name: %w", err)
	}
	if len(params.Credentials) == 0 {
		return nil, fmt.Errorf("credentials map is empty")
	}
	if params.MachineRoomID == "" {
		return nil, fmt.Errorf("machine room ID is required")
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

	// Fetch the machine's public key from the fleet machine room.
	machineKeyContent, err := session.GetStateEvent(ctx, params.MachineRoomID, schema.EventTypeMachineKey, params.MachineName)
	if err != nil {
		return nil, fmt.Errorf("fetching machine key for %q: %w", params.MachineName, err)
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
	encryptedFor := []string{principal.MatrixUserID(params.MachineName, params.ServerName)}

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
	configAlias := principal.RoomAlias(schema.ConfigRoomAlias(params.MachineName), params.ServerName)
	configRoomID, err := session.ResolveAlias(ctx, configAlias)
	if err != nil {
		return nil, fmt.Errorf("resolving config room %q: %w", configAlias, err)
	}

	// Publish the credentials state event.
	principalUserID := principal.MatrixUserID(params.Principal, params.ServerName)
	credentialEvent := schema.Credentials{
		Version:       schema.CredentialsVersion,
		Principal:     principalUserID,
		EncryptedFor:  encryptedFor,
		Keys:          credentialKeys,
		Ciphertext:    ciphertext,
		ProvisionedBy: session.UserID(),
		ProvisionedAt: time.Now().UTC().Format(time.RFC3339),
	}

	eventID, err := session.SendStateEvent(ctx, configRoomID, schema.EventTypeCredentials, params.Principal, credentialEvent)
	if err != nil {
		return nil, fmt.Errorf("publishing credentials for %q: %w", params.Principal, err)
	}

	return &ProvisionResult{
		EventID:      eventID,
		ConfigRoomID: configRoomID,
		PrincipalID:  principalUserID,
		EncryptedFor: encryptedFor,
		Keys:         credentialKeys,
	}, nil
}
