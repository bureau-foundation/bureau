// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package credential

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// Bundle holds the metadata for a single provisioned credential bundle.
// The ciphertext is not included — this is for listing/auditing, not
// decryption.
type Bundle struct {
	// StateKey is the principal localpart used as the state event key.
	StateKey string

	// Principal is the full Matrix user ID of the credential target.
	Principal string

	// EncryptedFor lists who can decrypt the bundle.
	EncryptedFor []string

	// Keys lists the credential names in the bundle.
	Keys []string

	// ProvisionedBy is the Matrix user ID of the provisioner.
	ProvisionedBy string

	// ProvisionedAt is the ISO 8601 timestamp.
	ProvisionedAt string
}

// ListResult holds the result of listing credentials for a machine.
type ListResult struct {
	// MachineName is the machine's localpart.
	MachineName string

	// ConfigRoomID is the machine's config room.
	ConfigRoomID string

	// Bundles contains one entry per provisioned credential bundle.
	Bundles []Bundle
}

// List reads all m.bureau.credentials state events from a machine's config
// room and returns their metadata. Returns an error if the config room
// cannot be found or read.
func List(ctx context.Context, session *messaging.Session, machineName, serverName string) (*ListResult, error) {
	if err := principal.ValidateLocalpart(machineName); err != nil {
		return nil, fmt.Errorf("invalid machine name: %w", err)
	}

	// Resolve the config room.
	configAlias := principal.RoomAlias(schema.ConfigRoomAlias(machineName), serverName)
	configRoomID, err := session.ResolveAlias(ctx, configAlias)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			return nil, fmt.Errorf("no config room found for %q (expected alias %q)", machineName, configAlias)
		}
		return nil, fmt.Errorf("resolving config room %q: %w", configAlias, err)
	}

	// Read all state events to find credential bundles.
	events, err := session.GetRoomState(ctx, configRoomID)
	if err != nil {
		return nil, fmt.Errorf("reading state from config room %s: %w", configRoomID, err)
	}

	var bundles []Bundle
	for _, event := range events {
		if event.Type != schema.EventTypeCredentials || event.StateKey == nil || *event.StateKey == "" {
			continue
		}

		contentJSON, err := json.Marshal(event.Content)
		if err != nil {
			return nil, fmt.Errorf("re-marshaling credentials event for %q: %w", *event.StateKey, err)
		}

		var credentials schema.Credentials
		if err := json.Unmarshal(contentJSON, &credentials); err != nil {
			return nil, fmt.Errorf("parsing credentials event for %q: %w", *event.StateKey, err)
		}

		bundles = append(bundles, Bundle{
			StateKey:      *event.StateKey,
			Principal:     credentials.Principal,
			EncryptedFor:  credentials.EncryptedFor,
			Keys:          credentials.Keys,
			ProvisionedBy: credentials.ProvisionedBy,
			ProvisionedAt: credentials.ProvisionedAt,
		})
	}

	return &ListResult{
		MachineName:  machineName,
		ConfigRoomID: configRoomID,
		Bundles:      bundles,
	}, nil
}
