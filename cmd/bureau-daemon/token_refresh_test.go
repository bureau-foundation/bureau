// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

// newTokenRefreshDaemon creates a test daemon configured for token
// refresh tests: signing keypair and stateDir set up. Returns the
// daemon, fake clock, and public key for token verification.
func newTokenRefreshDaemon(t *testing.T) (*Daemon, *clock.FakeClock, ed25519.PublicKey) {
	t.Helper()

	daemon, fakeClock := newTestDaemon(t)

	_, privateKey, err := servicetoken.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	daemon.machine, daemon.fleet = testMachineSetup(t, "test", "bureau.local")
	daemon.stateDir = t.TempDir()
	daemon.tokenSigningPrivateKey = privateKey

	publicKey := privateKey.Public().(ed25519.PublicKey)
	return daemon, fakeClock, publicKey
}

func TestRefreshTokens_RefreshesAtThreshold(t *testing.T) {
	t.Parallel()

	daemon, fakeClock, publicKey := newTokenRefreshDaemon(t)

	// Set up a running principal with required services and grants.
	daemon.authorizationIndex.SetPrincipal("agent/alpha", schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{"ticket/create", "ticket/read"}},
		},
	})
	daemon.running["agent/alpha"] = true
	daemon.lastSpecs["agent/alpha"] = &schema.SandboxSpec{
		RequiredServices: []string{"ticket"},
	}

	// Mint initial tokens.
	tokenDirectory, _, err := daemon.mintServiceTokens("agent/alpha", []string{"ticket"})
	if err != nil {
		t.Fatalf("initial mint: %v", err)
	}
	daemon.lastTokenMint["agent/alpha"] = fakeClock.Now()

	// Read the initial token to get its expiry.
	initialToken := readAndVerifyToken(t, tokenDirectory, "ticket", publicKey, fakeClock.Now())
	initialExpiry := initialToken.ExpiresAt

	// Advance clock to just BEFORE the refresh threshold (80% of 5min = 4min).
	fakeClock.Advance(3*time.Minute + 59*time.Second)

	// refreshTokens should NOT refresh yet.
	daemon.refreshTokens(context.Background())

	tokenAfterNoRefresh := readAndVerifyToken(t, tokenDirectory, "ticket", publicKey, fakeClock.Now())
	if tokenAfterNoRefresh.ExpiresAt != initialExpiry {
		t.Errorf("token should not have been refreshed before threshold; expiry changed from %d to %d",
			initialExpiry, tokenAfterNoRefresh.ExpiresAt)
	}

	// Advance past the threshold.
	fakeClock.Advance(2 * time.Second)

	// Now refreshTokens should refresh.
	daemon.refreshTokens(context.Background())

	refreshedToken := readAndVerifyToken(t, tokenDirectory, "ticket", publicKey, fakeClock.Now())
	if refreshedToken.ExpiresAt <= initialExpiry {
		t.Errorf("token should have later expiry after refresh; got %d, initial was %d",
			refreshedToken.ExpiresAt, initialExpiry)
	}

	// The refreshed token should expire 5 minutes from the current clock
	// time (which is startTime + 4min01s).
	expectedExpiry := fakeClock.Now().Add(tokenTTL).Unix()
	if refreshedToken.ExpiresAt != expectedExpiry {
		t.Errorf("refreshed token expiry = %d, want %d (now + TTL)",
			refreshedToken.ExpiresAt, expectedExpiry)
	}

	// lastTokenMint should be updated.
	if daemon.lastTokenMint["agent/alpha"] != fakeClock.Now() {
		t.Errorf("lastTokenMint = %v, want %v", daemon.lastTokenMint["agent/alpha"], fakeClock.Now())
	}
}

func TestRefreshTokens_SweepsExpiredTemporalGrants(t *testing.T) {
	t.Parallel()

	daemon, fakeClock, publicKey := newTokenRefreshDaemon(t)
	startTime := fakeClock.Now()

	// Set up a running principal with a static grant and a temporal grant.
	daemon.authorizationIndex.SetPrincipal("agent/alpha", schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{"ticket/read"}},
		},
	})

	// Add a temporal grant that expires in 3 minutes.
	temporalExpiry := startTime.Add(3 * time.Minute)
	temporalGrant := schema.Grant{
		Actions:   []string{"ticket/create"},
		ExpiresAt: temporalExpiry.Format(time.RFC3339),
		Ticket:    "tkt-test-temporal",
	}
	if !daemon.authorizationIndex.AddTemporalGrant("agent/alpha", temporalGrant) {
		t.Fatal("AddTemporalGrant returned false")
	}

	// Principal should now have 2 grants: 1 static + 1 temporal.
	grants := daemon.authorizationIndex.Grants("agent/alpha")
	if len(grants) != 2 {
		t.Fatalf("expected 2 grants before sweep, got %d", len(grants))
	}

	daemon.running["agent/alpha"] = true
	daemon.lastSpecs["agent/alpha"] = &schema.SandboxSpec{
		RequiredServices: []string{"ticket"},
	}

	// Mint initial tokens (with both grants).
	tokenDirectory, _, err := daemon.mintServiceTokens("agent/alpha", []string{"ticket"})
	if err != nil {
		t.Fatalf("initial mint: %v", err)
	}
	daemon.lastTokenMint["agent/alpha"] = fakeClock.Now()

	initialToken := readAndVerifyToken(t, tokenDirectory, "ticket", publicKey, fakeClock.Now())
	if len(initialToken.Grants) != 2 {
		t.Fatalf("initial token should have 2 grants, got %d", len(initialToken.Grants))
	}

	// Advance past the temporal grant's expiry.
	fakeClock.Advance(3*time.Minute + 1*time.Second)

	// refreshTokens should sweep the expired temporal grant and re-mint.
	daemon.refreshTokens(context.Background())

	// The authorization index should now have only 1 grant.
	grantsAfter := daemon.authorizationIndex.Grants("agent/alpha")
	if len(grantsAfter) != 1 {
		t.Errorf("expected 1 grant after sweep, got %d", len(grantsAfter))
	}

	// The refreshed token should have only 1 grant.
	refreshedToken := readAndVerifyToken(t, tokenDirectory, "ticket", publicKey, fakeClock.Now())
	if len(refreshedToken.Grants) != 1 {
		t.Errorf("refreshed token should have 1 grant after sweep, got %d",
			len(refreshedToken.Grants))
	}
}

func TestRefreshTokens_SkipsPrincipalsWithoutServices(t *testing.T) {
	t.Parallel()

	daemon, fakeClock, _ := newTokenRefreshDaemon(t)

	// Running principal with no RequiredServices.
	daemon.running["agent/noservices"] = true
	daemon.lastSpecs["agent/noservices"] = &schema.SandboxSpec{}

	// Running principal with nil spec.
	daemon.running["agent/nospec"] = true

	// Advance well past threshold.
	fakeClock.Advance(10 * time.Minute)

	// refreshTokens should not panic or create any token files.
	daemon.refreshTokens(context.Background())

	// Verify no token directories were created.
	tokensDir := filepath.Join(daemon.stateDir, "tokens")
	entries, _ := os.ReadDir(tokensDir)
	if len(entries) > 0 {
		t.Errorf("expected no token directories for principals without services, got %d", len(entries))
	}
}

func TestRefreshTokens_InitialSweepMintsForAdoptedPrincipals(t *testing.T) {
	t.Parallel()

	daemon, fakeClock, publicKey := newTokenRefreshDaemon(t)

	// Simulate an adopted principal: running with a spec but no
	// lastTokenMint entry (zero time — daemon restarted).
	daemon.authorizationIndex.SetPrincipal("agent/adopted", schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{"artifact/read"}},
		},
	})
	daemon.running["agent/adopted"] = true
	daemon.lastSpecs["agent/adopted"] = &schema.SandboxSpec{
		RequiredServices: []string{"artifact"},
	}
	// lastTokenMint intentionally NOT set — zero value.

	// Call refreshTokens (simulates the initial sweep in tokenRefreshLoop).
	daemon.refreshTokens(context.Background())

	// Token should have been minted.
	tokenDirectory := filepath.Join(daemon.stateDir, "tokens", "agent/adopted")
	token := readAndVerifyToken(t, tokenDirectory, "artifact", publicKey, fakeClock.Now())

	if token.Subject != "agent/adopted" {
		t.Errorf("Subject = %q, want %q", token.Subject, "agent/adopted")
	}
	if token.Audience != "artifact" {
		t.Errorf("Audience = %q, want %q", token.Audience, "artifact")
	}

	// lastTokenMint should now be set.
	if daemon.lastTokenMint["agent/adopted"].IsZero() {
		t.Error("lastTokenMint should be set after initial sweep")
	}
}

func TestRefreshTokens_GrantChangeTriggersRemint(t *testing.T) {
	t.Parallel()

	daemon, fakeClock, publicKey := newTokenRefreshDaemon(t)

	// Set up a running principal with an initial grant.
	daemon.authorizationIndex.SetPrincipal("agent/alpha", schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{"ticket/read"}},
		},
	})
	daemon.running["agent/alpha"] = true
	daemon.lastSpecs["agent/alpha"] = &schema.SandboxSpec{
		RequiredServices: []string{"ticket"},
	}

	// Mint initial tokens.
	tokenDirectory, _, err := daemon.mintServiceTokens("agent/alpha", []string{"ticket"})
	if err != nil {
		t.Fatalf("initial mint: %v", err)
	}
	daemon.lastTokenMint["agent/alpha"] = fakeClock.Now()

	initialToken := readAndVerifyToken(t, tokenDirectory, "ticket", publicKey, fakeClock.Now())
	if len(initialToken.Grants) != 1 {
		t.Fatalf("initial token should have 1 grant, got %d", len(initialToken.Grants))
	}

	// Simulate a grant change (what reconcile does when grants update):
	// update the index and reset lastTokenMint.
	daemon.authorizationIndex.SetPrincipal("agent/alpha", schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{"ticket/read"}},
			{Actions: []string{"ticket/create", "ticket/close"}},
		},
	})
	daemon.lastTokenMint["agent/alpha"] = time.Time{} // force re-mint

	// Advance only 1 second (well before the normal 4-minute threshold).
	fakeClock.Advance(1 * time.Second)

	// refreshTokens should re-mint because lastTokenMint was zeroed.
	daemon.refreshTokens(context.Background())

	refreshedToken := readAndVerifyToken(t, tokenDirectory, "ticket", publicKey, fakeClock.Now())
	if len(refreshedToken.Grants) != 2 {
		t.Errorf("refreshed token should have 2 grants after grant change, got %d",
			len(refreshedToken.Grants))
	}
}

func TestTokenRefreshCandidates(t *testing.T) {
	t.Parallel()

	daemon, fakeClock, _ := newTokenRefreshDaemon(t)
	startTime := fakeClock.Now()

	// Principal with tokens minted at start time (not yet due).
	daemon.running["agent/fresh"] = true
	daemon.lastSpecs["agent/fresh"] = &schema.SandboxSpec{
		RequiredServices: []string{"ticket"},
	}
	daemon.lastTokenMint["agent/fresh"] = startTime

	// Principal with zero lastTokenMint (adopted, never minted).
	daemon.running["agent/adopted"] = true
	daemon.lastSpecs["agent/adopted"] = &schema.SandboxSpec{
		RequiredServices: []string{"artifact"},
	}

	// Principal with no required services (should be skipped).
	daemon.running["agent/noservices"] = true
	daemon.lastSpecs["agent/noservices"] = &schema.SandboxSpec{}

	// Principal with nil spec (should be skipped).
	daemon.running["agent/nospec"] = true

	// At start time, only agent/adopted should be a candidate (zero mint time).
	candidates := daemon.tokenRefreshCandidates(startTime)
	if len(candidates) != 1 {
		t.Fatalf("at start: expected 1 candidate, got %d", len(candidates))
	}
	if candidates[0].localpart != "agent/adopted" {
		t.Errorf("at start: candidate = %q, want %q", candidates[0].localpart, "agent/adopted")
	}

	// Advance past threshold: both agent/fresh and agent/adopted should
	// be candidates.
	pastThreshold := startTime.Add(tokenRefreshThreshold + 1*time.Second)
	candidates = daemon.tokenRefreshCandidates(pastThreshold)
	if len(candidates) != 2 {
		t.Fatalf("past threshold: expected 2 candidates, got %d", len(candidates))
	}
	// Sorted by localpart.
	if candidates[0].localpart != "agent/adopted" {
		t.Errorf("past threshold: first candidate = %q, want %q", candidates[0].localpart, "agent/adopted")
	}
	if candidates[1].localpart != "agent/fresh" {
		t.Errorf("past threshold: second candidate = %q, want %q", candidates[1].localpart, "agent/fresh")
	}
}

// readAndVerifyToken reads a token file from the given directory and
// role, verifies its Ed25519 signature against the given time, and
// returns the decoded token. The now parameter must match the fake
// clock's current time, since tokens are minted with fake-clock
// timestamps that are in the past relative to wall-clock time.
func readAndVerifyToken(t *testing.T, tokenDirectory, role string, publicKey ed25519.PublicKey, now time.Time) *servicetoken.Token {
	t.Helper()

	tokenPath := filepath.Join(tokenDirectory, role+".token")
	tokenBytes, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("reading token file %s: %v", tokenPath, err)
	}
	if len(tokenBytes) == 0 {
		t.Fatalf("token file %s is empty", tokenPath)
	}

	token, err := servicetoken.VerifyAt(publicKey, tokenBytes, now)
	if err != nil {
		t.Fatalf("Verify token %s: %v", tokenPath, err)
	}

	return token
}
