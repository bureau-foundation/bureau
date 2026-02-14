// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"sort"
	"time"
)

// tokenRefreshInterval is the period between token refresh ticks. Each
// tick checks all running principals and re-mints tokens for any that
// are past the refresh threshold. Also sweeps expired temporal grants.
const tokenRefreshInterval = 1 * time.Minute

// tokenRefreshThreshold is 80% of tokenTTL. Tokens older than this
// are re-minted before they expire. With a 5-minute TTL this gives
// agents a fresh token 1 minute before the old one expires.
const tokenRefreshThreshold = tokenTTL * 4 / 5

// tokenRefreshCandidate holds the snapshot of a running principal's
// state needed to decide whether to refresh its service tokens.
type tokenRefreshCandidate struct {
	localpart        string
	requiredServices []string
	lastMint         time.Time
}

// tokenRefreshLoop is a background goroutine that periodically refreshes
// service tokens for running principals and sweeps expired temporal
// grants from the authorization index.
//
// Token refresh: service tokens are minted with a 5-minute TTL at
// sandbox creation. This goroutine re-mints them at 80% of TTL (4
// minutes) so agents always have a valid token. The token directory is
// bind-mounted as a directory into the sandbox, so atomic write+rename
// on the host is visible to the agent immediately via VFS path traversal.
//
// Temporal grant sweep: calls authorizationIndex.SweepExpired() to
// remove grants that have passed their ExpiresAt timestamp. Affected
// principals get their lastTokenMint reset to force an immediate
// re-mint, ensuring their tokens no longer carry the expired grants.
func (d *Daemon) tokenRefreshLoop(ctx context.Context) {
	// Run an immediate sweep on startup. This handles daemon restart
	// recovery: adopted principals have expired tokens from the
	// previous daemon instance and need fresh ones immediately.
	d.refreshTokens(ctx)

	ticker := d.clock.NewTicker(tokenRefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.refreshTokens(ctx)
		}
	}
}

// refreshTokens performs one cycle of temporal grant sweeping and token
// refresh. Safe to call concurrently with reconcile — uses reconcileMu
// for state access and the authorization index's own lock for grant
// operations.
func (d *Daemon) refreshTokens(ctx context.Context) {
	now := d.clock.Now()

	// Sweep expired temporal grants. Returns localparts of principals
	// whose grant sets changed (grants were removed).
	affected := d.authorizationIndex.SweepExpired(now)
	if len(affected) > 0 {
		d.logger.Info("swept expired temporal grants",
			"affected_principals", affected,
		)

		// Force re-mint for affected principals so their tokens
		// no longer carry the expired grants.
		d.reconcileMu.Lock()
		for _, localpart := range affected {
			if d.running[localpart] {
				d.lastTokenMint[localpart] = time.Time{}
			}
		}
		d.reconcileMu.Unlock()
	}

	// Snapshot running principals that need token refresh.
	candidates := d.tokenRefreshCandidates(now)
	if len(candidates) == 0 {
		return
	}

	// Re-mint tokens outside the lock — mintServiceTokens reads from
	// the authorization index (its own RWMutex) and writes to disk.
	refreshed := 0
	for _, candidate := range candidates {
		// Check context cancellation between principals to allow
		// prompt shutdown.
		if ctx.Err() != nil {
			return
		}

		_, minted, err := d.mintServiceTokens(candidate.localpart, candidate.requiredServices)
		if err != nil {
			d.logger.Error("token refresh failed",
				"principal", candidate.localpart,
				"error", err,
			)
			continue
		}

		// Update the mint timestamp and record the new tokens for
		// emergency revocation tracking. recordMintedTokens also
		// prunes expired entries from previous mints.
		d.reconcileMu.Lock()
		d.lastTokenMint[candidate.localpart] = d.clock.Now()
		d.recordMintedTokens(candidate.localpart, minted)
		d.reconcileMu.Unlock()

		refreshed++
		d.logger.Info("refreshed service tokens",
			"principal", candidate.localpart,
			"services", candidate.requiredServices,
		)
	}

	if refreshed > 0 {
		d.logger.Info("token refresh cycle complete",
			"refreshed", refreshed,
			"total_candidates", len(candidates),
		)
	}
}

// tokenRefreshCandidates returns running principals whose service tokens
// need refreshing: those with required services whose lastTokenMint is
// zero (never minted or force-reset) or older than tokenRefreshThreshold.
//
// Acquires reconcileMu.RLock and releases it before returning.
func (d *Daemon) tokenRefreshCandidates(now time.Time) []tokenRefreshCandidate {
	d.reconcileMu.RLock()
	defer d.reconcileMu.RUnlock()

	var candidates []tokenRefreshCandidate
	for localpart := range d.running {
		spec := d.lastSpecs[localpart]
		if spec == nil || len(spec.RequiredServices) == 0 {
			continue
		}

		lastMint := d.lastTokenMint[localpart]
		if lastMint.IsZero() || now.Sub(lastMint) >= tokenRefreshThreshold {
			candidates = append(candidates, tokenRefreshCandidate{
				localpart:        localpart,
				requiredServices: spec.RequiredServices,
				lastMint:         lastMint,
			})
		}
	}

	// Sort for deterministic ordering in logs and tests.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].localpart < candidates[j].localpart
	})

	return candidates
}
