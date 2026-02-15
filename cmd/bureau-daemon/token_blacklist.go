// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

// activeToken tracks a minted service token that has not yet naturally
// expired. Used for emergency revocation: when a sandbox is destroyed,
// the daemon sends these token IDs to the relevant services for
// blacklisting.
type activeToken struct {
	id          string
	serviceRole string
	expiresAt   time.Time
}

// recordMintedTokens appends newly minted tokens to the active set for
// a principal and prunes entries whose natural TTL has passed. Must be
// called with reconcileMu held (the active tokens map is protected by
// the same lock as the other per-principal tracking maps).
func (d *Daemon) recordMintedTokens(localpart string, minted []activeToken) {
	now := d.clock.Now()

	// Prune expired entries from the existing set.
	existing := d.activeTokens[localpart]
	pruned := existing[:0]
	for _, token := range existing {
		if now.Before(token.expiresAt) {
			pruned = append(pruned, token)
		}
	}

	d.activeTokens[localpart] = append(pruned, minted...)
}

// revokeAndCleanupTokens handles token lifecycle cleanup when a sandbox
// is destroyed. It pushes revocations to relevant services (best-effort),
// removes the token directory from disk, and clears the daemon's tracking
// state. Must be called with reconcileMu held.
//
// The push to services is best-effort: failures are logged but do not
// block the destroy. The 5-minute token TTL provides a natural fallback
// — even if push fails, tokens expire shortly.
func (d *Daemon) revokeAndCleanupTokens(ctx context.Context, localpart string) {
	tokens := d.activeTokens[localpart]
	mounts := d.lastServiceMounts[localpart]

	if len(tokens) > 0 && len(mounts) > 0 {
		d.pushRevocations(ctx, localpart, tokens, mounts)
	}

	// Remove the token directory from disk.
	if d.stateDir != "" {
		tokenDir := filepath.Join(d.stateDir, "tokens", localpart)
		if err := os.RemoveAll(tokenDir); err != nil {
			d.logger.Error("failed to remove token directory",
				"principal", localpart,
				"path", tokenDir,
				"error", err,
			)
		}
	}

	delete(d.activeTokens, localpart)
	delete(d.lastServiceMounts, localpart)
}

// pushRevocations sends signed revocation requests to each service that
// holds tokens for the given principal. Best-effort: errors are logged
// but the function always returns.
func (d *Daemon) pushRevocations(ctx context.Context, localpart string, tokens []activeToken, mounts []launcherServiceMount) {
	// Build a role → socket path lookup from the cached service mounts.
	socketByRole := make(map[string]string, len(mounts))
	for _, mount := range mounts {
		socketByRole[mount.Role] = mount.SocketPath
	}

	// Group tokens by service role.
	byRole := make(map[string][]activeToken)
	for _, token := range tokens {
		byRole[token.serviceRole] = append(byRole[token.serviceRole], token)
	}

	for role, roleTokens := range byRole {
		socketPath, ok := socketByRole[role]
		if !ok {
			d.logger.Warn("no cached socket path for revocation push",
				"role", role,
				"principal", localpart,
			)
			continue
		}

		entries := make([]servicetoken.RevocationEntry, len(roleTokens))
		for i, token := range roleTokens {
			entries[i] = servicetoken.RevocationEntry{
				TokenID:   token.id,
				ExpiresAt: token.expiresAt.Unix(),
			}
		}

		request := &servicetoken.RevocationRequest{
			Entries:  entries,
			IssuedAt: d.clock.Now().Unix(),
		}

		signed, err := servicetoken.SignRevocation(d.tokenSigningPrivateKey, request)
		if err != nil {
			d.logger.Error("failed to sign revocation request",
				"role", role,
				"principal", localpart,
				"error", err,
			)
			continue
		}

		// Send as an unauthenticated client — the signed blob itself
		// is the authentication (verified against the daemon's public
		// key on the service side).
		//
		// The artifact service uses a different wire protocol
		// (length-prefixed CBOR) from the standard service socket
		// protocol (bare CBOR with Response envelope). Use the
		// correct caller for each.
		revocationPayload := map[string]any{
			"action":     "revoke-tokens",
			"revocation": signed,
		}
		var pushErr error
		if role == "artifact" {
			pushErr = artifactServiceCall(socketPath, revocationPayload, nil)
		} else {
			client := service.NewServiceClientFromToken(socketPath, nil)
			pushErr = client.Call(ctx, "revoke-tokens", map[string]any{
				"revocation": signed,
			}, nil)
		}
		if pushErr != nil {
			d.logger.Warn("revocation push failed (tokens will expire via TTL)",
				"role", role,
				"principal", localpart,
				"token_count", len(roleTokens),
				"error", pushErr,
			)
			continue
		}

		d.logger.Info("pushed token revocations to service",
			"role", role,
			"principal", localpart,
			"revoked", len(roleTokens),
		)
	}
}
