// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/ipc"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/messaging"
)

// credentialServiceRole is the service role name for the daemon's
// credential provisioning socket. Principals with credential/* grants
// get this socket mounted at /run/bureau/service/credential.sock
// and a service token at /run/bureau/service/token/credential.token.
const credentialServiceRole = "credential"

// provisionRequest holds the fields decoded from the CBOR request body
// for a "provision-credential" action. The "action" and "token" fields
// are handled by the SocketServer framework before the handler is called.
type provisionRequest struct {
	Principal string `cbor:"principal"` // target principal localpart
	Key       string `cbor:"key"`       // credential key name
	Value     string `cbor:"value"`     // plaintext credential value
}

// provisionResponse is the CBOR response for a successful credential
// provisioning request. Returned as the "data" field in the SocketServer
// response envelope.
type provisionResponse struct {
	Principal string   `cbor:"principal"` // target principal localpart
	Key       string   `cbor:"key"`       // provisioned key name
	Keys      []string `cbor:"keys"`      // all keys in the updated bundle
}

// startCredentialService creates and starts the credential provisioning
// socket server. The server listens at <runDir>/credential.sock and
// authenticates requests using the daemon's token signing keypair.
//
// Returns the socket path for use in service mounts.
func (d *Daemon) startCredentialService(ctx context.Context) (string, error) {
	socketPath := d.credentialSocketPath()

	authConfig := &service.AuthConfig{
		PublicKey: d.tokenSigningPublicKey,
		Audience:  credentialServiceRole,
		Blacklist: servicetoken.NewBlacklist(),
		Clock:     d.clock,
	}

	server := service.NewSocketServer(socketPath, d.logger, authConfig)

	server.HandleAuth("provision-credential", d.handleProvisionCredential)

	go func() {
		if err := server.Serve(ctx); err != nil && ctx.Err() == nil {
			d.logger.Error("credential service socket error", "error", err)
		}
	}()

	d.logger.Info("credential service started", "socket", socketPath)
	return socketPath, nil
}

// credentialSocketPath returns the path for the daemon's credential
// service socket. The daemon is not a principal, so this uses a fixed
// name rather than a localpart-derived path.
func (d *Daemon) credentialSocketPath() string {
	return d.runDir + "/credential.sock"
}

// handleProvisionCredential processes a credential provisioning request.
// It verifies the caller has a credential/provision/key/<keyname> grant
// targeting the requested principal, then delegates the decrypt→merge→
// re-encrypt cycle to the launcher via IPC.
func (d *Daemon) handleProvisionCredential(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	var request provisionRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	if request.Principal == "" {
		return nil, fmt.Errorf("principal is required")
	}
	if request.Key == "" {
		return nil, fmt.Errorf("key is required")
	}
	if request.Value == "" {
		return nil, fmt.Errorf("value is required")
	}

	// Check that the caller's grants authorize provisioning this
	// specific key for this specific principal. The target is the
	// bare account localpart because service token grants use
	// localpart-level matching (tokens are daemon-scoped via audience).
	action := schema.ActionCredentialProvisionKeyPrefix + request.Key
	if !servicetoken.GrantsAllow(token.Grants, action, request.Principal) {
		d.logger.Warn("credential provision denied",
			"caller", token.Subject,
			"action", action,
			"target", request.Principal,
		)
		return nil, fmt.Errorf("access denied: no grant for %s on %s", action, request.Principal)
	}

	// Construct a fleet-scoped Entity from the bare account localpart
	// so we can use it for readCredentials (which derives the state
	// key from Entity.Localpart()) and for the credential state key.
	principalEntity, err := ref.NewEntityFromAccountLocalpart(d.fleet, request.Principal)
	if err != nil {
		return nil, fmt.Errorf("invalid principal %q: %w", request.Principal, err)
	}

	// Read the current credentials for the target principal. If none
	// exist yet, the launcher will create a fresh bundle.
	var currentCiphertext string
	credentials, err := d.readCredentials(ctx, principalEntity)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			// No existing credentials — the launcher will create a fresh bundle.
			d.logger.Info("no existing credentials for principal, creating fresh bundle",
				"principal", request.Principal,
			)
		} else {
			return nil, fmt.Errorf("reading credentials for %s: %w", request.Principal, err)
		}
	} else {
		currentCiphertext = credentials.Ciphertext
	}

	// Build recipient key list. The machine's age public key is always
	// included so the launcher can decrypt the bundle on future reads.
	recipientKeys, err := d.credentialRecipientKeys(credentials)
	if err != nil {
		return nil, fmt.Errorf("resolving recipient keys: %w", err)
	}

	// Delegate the decrypt→merge→re-encrypt to the launcher (which
	// holds the machine's age private key).
	response, err := d.launcherRequest(ctx, launcherIPCRequest{
		Action:               ipc.ActionProvisionCredential,
		Principal:            request.Principal,
		KeyName:              request.Key,
		KeyValue:             request.Value,
		EncryptedCredentials: currentCiphertext,
		RecipientKeys:        recipientKeys,
	})
	if err != nil {
		return nil, fmt.Errorf("launcher IPC: %w", err)
	}
	if !response.OK {
		return nil, fmt.Errorf("launcher: %s", response.Error)
	}

	// Publish the updated credentials to Matrix. The normal credential
	// rotation detection in the reconcile loop will see the ciphertext
	// change and restart the affected sandbox.
	now := d.clock.Now()
	updatedCredentials := schema.Credentials{
		Version:       1,
		Principal:     principalEntity.UserID(),
		Keys:          response.UpdatedKeys,
		Ciphertext:    response.UpdatedCiphertext,
		ProvisionedBy: token.Subject,
		ProvisionedAt: now.Format(time.RFC3339),
	}

	// Preserve the EncryptedFor metadata from the existing event, or
	// build it from the recipient keys for fresh bundles.
	if credentials != nil && len(credentials.EncryptedFor) > 0 {
		updatedCredentials.EncryptedFor = credentials.EncryptedFor
	} else {
		updatedCredentials.EncryptedFor = recipientKeys
	}

	credentialStateKey := principalEntity.Localpart()
	if _, err := d.session.SendStateEvent(ctx, d.configRoomID, schema.EventTypeCredentials, credentialStateKey, updatedCredentials); err != nil {
		return nil, fmt.Errorf("publishing updated credentials: %w", err)
	}

	d.logger.Info("credential provisioned",
		"caller", token.Subject,
		"principal", request.Principal,
		"key", request.Key,
		"total_keys", len(response.UpdatedKeys),
	)

	return &provisionResponse{
		Principal: request.Principal,
		Key:       request.Key,
		Keys:      response.UpdatedKeys,
	}, nil
}

// credentialRecipientKeys builds the list of age public keys to encrypt
// a credential bundle to. Always includes the machine's public key.
// If the existing credential event has an EncryptedFor list, additional
// recipients (escrow keys, multi-machine keys) are preserved so the
// re-encrypted bundle remains accessible to all original recipients.
func (d *Daemon) credentialRecipientKeys(existing *schema.Credentials) ([]string, error) {
	if d.machinePublicKey == "" {
		return nil, fmt.Errorf("machine age public key not loaded")
	}

	// Start with the machine's own key (always needed for the launcher
	// to decrypt the bundle).
	keys := []string{d.machinePublicKey}

	// If the existing event lists additional recipients beyond the
	// machine key (e.g., escrow keys), preserve them. We identify
	// additional keys by checking which EncryptedFor entries are
	// valid age public keys that differ from our machine key.
	if existing != nil {
		for _, entry := range existing.EncryptedFor {
			if entry != d.machinePublicKey && isAgePublicKey(entry) {
				keys = append(keys, entry)
			}
		}
	}

	return keys, nil
}

// isAgePublicKey returns true if the string looks like an age x25519
// public key (starts with "age1").
func isAgePublicKey(s string) bool {
	return len(s) > 4 && s[:4] == "age1"
}

// hasCredentialGrants checks whether a principal has any credential/*
// grants in the authorization index. Used to determine whether to
// mount the daemon's credential service socket into a sandbox.
func (d *Daemon) hasCredentialGrants(principal ref.Entity) bool {
	grants := d.authorizationIndex.Grants(principal.UserID())
	for _, grant := range grants {
		filtered := filterGrantsForService([]schema.Grant{grant}, credentialServiceRole)
		if len(filtered) > 0 {
			return true
		}
	}
	return false
}

// appendCredentialServiceMount adds the daemon's credential service
// socket to the service mount list if the principal has credential/*
// grants. Returns the original mounts unchanged if no credential
// grants are present.
func (d *Daemon) appendCredentialServiceMount(principal ref.Entity, mounts []launcherServiceMount) []launcherServiceMount {
	if d.credentialSocketPath() == "" || !d.hasCredentialGrants(principal) {
		return mounts
	}
	return append(mounts, launcherServiceMount{
		Role:       credentialServiceRole,
		SocketPath: d.credentialSocketPath(),
	})
}

// credentialServiceRoles returns ["credential"] if the principal has
// credential/* grants, or nil otherwise. Used when computing which
// service tokens need refreshing.
func (d *Daemon) credentialServiceRoles(principal ref.Entity) []string {
	if d.hasCredentialGrants(principal) {
		return []string{credentialServiceRole}
	}
	return nil
}

// sortedKeys returns the sorted keys of a string map.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
