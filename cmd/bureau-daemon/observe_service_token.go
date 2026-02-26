// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"time"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/observe"
)

// handleMintServiceToken mints a signed service token for the
// authenticated observer. The token carries the observer's grants
// filtered to the requested service namespace and is signed with the
// daemon's Ed25519 key. The response includes the host-side socket
// path so the caller can connect to the service directly.
//
// Authentication is handled by handleObserveClient before dispatch.
// The observer identity in request.Observer has been verified.
func (d *Daemon) handleMintServiceToken(clientConnection net.Conn, request observeRequest) {
	if request.ServiceRole == "" {
		d.sendObserveError(clientConnection, "service_role is required for mint_service_token")
		return
	}

	observer, err := ref.ParseUserID(request.Observer)
	if err != nil {
		d.sendObserveError(clientConnection,
			"invalid observer identity: "+err.Error())
		return
	}

	if d.tokenSigningPrivateKey == nil {
		d.sendObserveError(clientConnection,
			"token signing keypair not initialized")
		return
	}

	// Get the observer's resolved grants and filter to those whose
	// action patterns could match the service namespace.
	grants := d.authorizationIndex.Grants(observer)
	filtered := filterGrantsForService(grants, request.ServiceRole)

	tokenGrants := make([]servicetoken.Grant, len(filtered))
	for i, grant := range filtered {
		tokenGrants[i] = servicetoken.Grant{
			Actions: grant.Actions,
			Targets: grant.Targets,
		}
	}

	tokenID, err := generateTokenID()
	if err != nil {
		d.sendObserveError(clientConnection,
			"generating token ID: "+err.Error())
		return
	}

	now := d.clock.Now()
	token := &servicetoken.Token{
		Subject:   observer,
		Machine:   d.machine,
		Audience:  request.ServiceRole,
		Grants:    tokenGrants,
		ID:        tokenID,
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(tokenTTL).Unix(),
	}

	tokenBytes, err := servicetoken.Mint(d.tokenSigningPrivateKey, token)
	if err != nil {
		d.sendObserveError(clientConnection,
			"minting service token: "+err.Error())
		return
	}

	// Resolve the service socket path. Search the config room for
	// the m.bureau.service_binding state event with the role as state key.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	socketPath, err := d.resolveServiceSocket(ctx, request.ServiceRole, []ref.RoomID{d.configRoomID})
	if err != nil {
		d.sendObserveError(clientConnection,
			fmt.Sprintf("no service binding found for role %q in config room %s (%s)",
				request.ServiceRole, d.machine.RoomAlias(), d.configRoomID))
		return
	}

	d.logger.Info("minted operator service token",
		"observer", observer,
		"service_role", request.ServiceRole,
		"token_id", tokenID,
		"grants", len(tokenGrants),
		"socket_path", socketPath,
		"expires_at", now.Add(tokenTTL).Format(time.RFC3339),
	)

	d.sendObserveJSON(clientConnection, observe.ServiceTokenResponse{
		OK:         true,
		Token:      base64.StdEncoding.EncodeToString(tokenBytes),
		SocketPath: socketPath,
		TTLSeconds: int(tokenTTL.Seconds()),
		ExpiresAt:  now.Add(tokenTTL).Unix(),
	})
}
