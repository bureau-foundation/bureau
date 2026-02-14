// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ed25519"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/ipc"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/lib/testutil"
)

// newTokenBlacklistDaemon creates a test daemon configured for token
// blacklist tests: signing keypair, stateDir, and machineName set up.
func newTokenBlacklistDaemon(t *testing.T) (*Daemon, *clock.FakeClock, ed25519.PublicKey) {
	t.Helper()

	daemon, fakeClock := newTestDaemon(t)

	publicKey, privateKey, err := servicetoken.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	daemon.machineName = "machine/test"
	daemon.stateDir = t.TempDir()
	daemon.tokenSigningPrivateKey = privateKey

	return daemon, fakeClock, publicKey
}

func TestRecordMintedTokens_TracksNewTokens(t *testing.T) {
	t.Parallel()

	daemon, _, _ := newTokenBlacklistDaemon(t)

	daemon.recordMintedTokens("agent/alpha", []activeToken{
		{id: "token-1", serviceRole: "ticket", expiresAt: testDaemonEpoch.Add(5 * time.Minute)},
		{id: "token-2", serviceRole: "artifact", expiresAt: testDaemonEpoch.Add(5 * time.Minute)},
	})

	tokens := daemon.activeTokens["agent/alpha"]
	if len(tokens) != 2 {
		t.Fatalf("activeTokens = %d, want 2", len(tokens))
	}
	if tokens[0].id != "token-1" || tokens[1].id != "token-2" {
		t.Errorf("token IDs = [%q, %q], want [token-1, token-2]", tokens[0].id, tokens[1].id)
	}
}

func TestRecordMintedTokens_AccumulatesAcrossRefreshes(t *testing.T) {
	t.Parallel()

	daemon, fakeClock, _ := newTokenBlacklistDaemon(t)

	// First mint at epoch.
	daemon.recordMintedTokens("agent/alpha", []activeToken{
		{id: "token-1", serviceRole: "ticket", expiresAt: testDaemonEpoch.Add(5 * time.Minute)},
	})

	// Refresh 4 minutes later (old token still has 1 min TTL).
	fakeClock.Advance(4 * time.Minute)
	daemon.recordMintedTokens("agent/alpha", []activeToken{
		{id: "token-2", serviceRole: "ticket", expiresAt: testDaemonEpoch.Add(4*time.Minute + 5*time.Minute)},
	})

	// Both tokens should be tracked (old one hasn't expired yet).
	tokens := daemon.activeTokens["agent/alpha"]
	if len(tokens) != 2 {
		t.Fatalf("activeTokens = %d, want 2 (old + new)", len(tokens))
	}
}

func TestRecordMintedTokens_PrunesExpiredOnMint(t *testing.T) {
	t.Parallel()

	daemon, fakeClock, _ := newTokenBlacklistDaemon(t)

	// First mint at epoch, expires at epoch+5min.
	daemon.recordMintedTokens("agent/alpha", []activeToken{
		{id: "token-1", serviceRole: "ticket", expiresAt: testDaemonEpoch.Add(5 * time.Minute)},
	})

	// Advance past the first token's expiry, then mint new ones.
	fakeClock.Advance(6 * time.Minute)
	daemon.recordMintedTokens("agent/alpha", []activeToken{
		{id: "token-2", serviceRole: "ticket", expiresAt: fakeClock.Now().Add(5 * time.Minute)},
	})

	// Only the new token should remain — the old one was pruned.
	tokens := daemon.activeTokens["agent/alpha"]
	if len(tokens) != 1 {
		t.Fatalf("activeTokens = %d, want 1 (old should be pruned)", len(tokens))
	}
	if tokens[0].id != "token-2" {
		t.Errorf("remaining token = %q, want token-2", tokens[0].id)
	}
}

func TestRevokeAndCleanupTokens_RemovesDirectory(t *testing.T) {
	t.Parallel()

	daemon, _, _ := newTokenBlacklistDaemon(t)

	// Create a token directory with a file in it.
	tokenDir := filepath.Join(daemon.stateDir, "tokens", "agent/alpha")
	if err := os.MkdirAll(tokenDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	tokenPath := filepath.Join(tokenDir, "ticket")
	if err := os.WriteFile(tokenPath, []byte("token-bytes"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Set up some active tokens to verify they're cleared.
	daemon.activeTokens["agent/alpha"] = []activeToken{
		{id: "token-1", serviceRole: "ticket", expiresAt: testDaemonEpoch.Add(5 * time.Minute)},
	}
	daemon.lastServiceMounts["agent/alpha"] = []launcherServiceMount{
		{Role: "ticket", SocketPath: "/tmp/nonexistent.sock"},
	}

	daemon.revokeAndCleanupTokens(context.Background(), "agent/alpha")

	// Token directory should be gone.
	if _, err := os.Stat(tokenDir); !os.IsNotExist(err) {
		t.Errorf("token directory should have been removed, got err=%v", err)
	}

	// Tracking state should be cleared.
	if _, exists := daemon.activeTokens["agent/alpha"]; exists {
		t.Error("activeTokens should have been deleted for agent/alpha")
	}
	if _, exists := daemon.lastServiceMounts["agent/alpha"]; exists {
		t.Error("lastServiceMounts should have been deleted for agent/alpha")
	}
}

func TestRevokeAndCleanupTokens_NoTokensIsNoop(t *testing.T) {
	t.Parallel()

	daemon, _, _ := newTokenBlacklistDaemon(t)

	// Should not panic or error when there are no tokens to clean up.
	daemon.revokeAndCleanupTokens(context.Background(), "agent/nonexistent")
}

func TestPushRevocations_SendsToCorrectService(t *testing.T) {
	t.Parallel()

	daemon, _, publicKey := newTokenBlacklistDaemon(t)

	// Start a test service socket with a blacklist.
	socketDir := testutil.SocketDir(t)
	socketPath := filepath.Join(socketDir, "ticket.sock")

	blacklist := servicetoken.NewBlacklist()
	authConfig := &service.AuthConfig{
		PublicKey: publicKey,
		Audience:  "ticket",
		Blacklist: blacklist,
	}

	server := service.NewSocketServer(socketPath, daemon.logger, authConfig)
	server.RegisterRevocationHandler()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		server.Serve(ctx)
	}()

	waitForSocket(t, socketPath)

	// Push revocations from the daemon.
	tokens := []activeToken{
		{id: "aabb1122", serviceRole: "ticket", expiresAt: testDaemonEpoch.Add(5 * time.Minute)},
		{id: "ccdd3344", serviceRole: "ticket", expiresAt: testDaemonEpoch.Add(5 * time.Minute)},
	}
	mounts := []launcherServiceMount{
		{Role: "ticket", SocketPath: socketPath},
	}

	daemon.pushRevocations(ctx, "agent/alpha", tokens, mounts)

	// Verify the blacklist received both token IDs.
	if blacklist.Len() != 2 {
		t.Fatalf("blacklist length = %d, want 2", blacklist.Len())
	}
	if !blacklist.IsRevoked("aabb1122") {
		t.Error("aabb1122 should be revoked")
	}
	if !blacklist.IsRevoked("ccdd3344") {
		t.Error("ccdd3344 should be revoked")
	}

	cancel()
	wg.Wait()
}

func TestPushRevocations_MultipleServices(t *testing.T) {
	t.Parallel()

	daemon, _, publicKey := newTokenBlacklistDaemon(t)

	socketDir := testutil.SocketDir(t)

	// Start two service sockets with separate blacklists.
	ticketPath := filepath.Join(socketDir, "ticket.sock")
	artifactPath := filepath.Join(socketDir, "artifact.sock")

	ticketBlacklist := servicetoken.NewBlacklist()
	artifactBlacklist := servicetoken.NewBlacklist()

	ticketServer := service.NewSocketServer(ticketPath, daemon.logger, &service.AuthConfig{
		PublicKey: publicKey,
		Audience:  "ticket",
		Blacklist: ticketBlacklist,
	})
	ticketServer.RegisterRevocationHandler()

	artifactServer := service.NewSocketServer(artifactPath, daemon.logger, &service.AuthConfig{
		PublicKey: publicKey,
		Audience:  "artifact",
		Blacklist: artifactBlacklist,
	})
	artifactServer.RegisterRevocationHandler()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); ticketServer.Serve(ctx) }()
	go func() { defer wg.Done(); artifactServer.Serve(ctx) }()

	waitForSocket(t, ticketPath)
	waitForSocket(t, artifactPath)

	tokens := []activeToken{
		{id: "ticket-token-1", serviceRole: "ticket", expiresAt: testDaemonEpoch.Add(5 * time.Minute)},
		{id: "artifact-token-1", serviceRole: "artifact", expiresAt: testDaemonEpoch.Add(5 * time.Minute)},
	}
	mounts := []launcherServiceMount{
		{Role: "ticket", SocketPath: ticketPath},
		{Role: "artifact", SocketPath: artifactPath},
	}

	daemon.pushRevocations(ctx, "agent/alpha", tokens, mounts)

	// Each service should only have its own token revoked.
	if ticketBlacklist.Len() != 1 {
		t.Errorf("ticket blacklist = %d, want 1", ticketBlacklist.Len())
	}
	if !ticketBlacklist.IsRevoked("ticket-token-1") {
		t.Error("ticket service should have revoked ticket-token-1")
	}
	if ticketBlacklist.IsRevoked("artifact-token-1") {
		t.Error("ticket service should NOT have artifact-token-1")
	}

	if artifactBlacklist.Len() != 1 {
		t.Errorf("artifact blacklist = %d, want 1", artifactBlacklist.Len())
	}
	if !artifactBlacklist.IsRevoked("artifact-token-1") {
		t.Error("artifact service should have revoked artifact-token-1")
	}
}

func TestPushRevocations_ServiceDownIsBestEffort(t *testing.T) {
	t.Parallel()

	daemon, _, _ := newTokenBlacklistDaemon(t)

	// Use a socket path that doesn't exist — the push should log and
	// continue, not panic or return an error.
	tokens := []activeToken{
		{id: "token-1", serviceRole: "ticket", expiresAt: testDaemonEpoch.Add(5 * time.Minute)},
	}
	mounts := []launcherServiceMount{
		{Role: "ticket", SocketPath: "/tmp/nonexistent-bureau-test.sock"},
	}

	// Should not panic.
	daemon.pushRevocations(context.Background(), "agent/alpha", tokens, mounts)
}

func TestTokenTrackingThroughMintAndRefresh(t *testing.T) {
	t.Parallel()

	daemon, fakeClock, _ := newTokenBlacklistDaemon(t)

	daemon.authorizationIndex.SetPrincipal("agent/alpha", schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{"ticket/**"}},
		},
	})

	// Initial mint at epoch.
	_, minted, err := daemon.mintServiceTokens("agent/alpha", []string{"ticket"})
	if err != nil {
		t.Fatalf("initial mint: %v", err)
	}
	daemon.recordMintedTokens("agent/alpha", minted)

	if len(daemon.activeTokens["agent/alpha"]) != 1 {
		t.Fatalf("after initial mint: activeTokens = %d, want 1",
			len(daemon.activeTokens["agent/alpha"]))
	}
	firstTokenID := daemon.activeTokens["agent/alpha"][0].id

	// Simulate refresh at 80% of TTL (4 minutes).
	fakeClock.Advance(4 * time.Minute)
	_, refreshed, err := daemon.mintServiceTokens("agent/alpha", []string{"ticket"})
	if err != nil {
		t.Fatalf("refresh mint: %v", err)
	}
	daemon.recordMintedTokens("agent/alpha", refreshed)

	// Both old and new tokens should be tracked (old hasn't expired).
	tokens := daemon.activeTokens["agent/alpha"]
	if len(tokens) != 2 {
		t.Fatalf("after refresh: activeTokens = %d, want 2", len(tokens))
	}

	// New token should have a different ID.
	secondTokenID := refreshed[0].id
	if firstTokenID == secondTokenID {
		t.Error("refreshed token should have a different ID from the original")
	}

	// Advance past the first token's expiry (epoch + 5min) and mint again.
	fakeClock.Advance(2 * time.Minute) // now at epoch + 6min
	_, thirdMint, err := daemon.mintServiceTokens("agent/alpha", []string{"ticket"})
	if err != nil {
		t.Fatalf("second refresh mint: %v", err)
	}
	daemon.recordMintedTokens("agent/alpha", thirdMint)

	// The first token (expired at epoch+5min) should be pruned.
	// The second (expires at epoch+4min+5min=epoch+9min) and third should remain.
	tokens = daemon.activeTokens["agent/alpha"]
	if len(tokens) != 2 {
		t.Fatalf("after second refresh: activeTokens = %d, want 2 (first pruned)", len(tokens))
	}

	// Verify the first token ID is gone.
	for _, token := range tokens {
		if token.id == firstTokenID {
			t.Errorf("first token (ID=%s) should have been pruned", firstTokenID)
		}
	}
}

func TestRevokeAndCleanupTokens_FullLifecycle(t *testing.T) {
	t.Parallel()

	daemon, _, publicKey := newTokenBlacklistDaemon(t)

	// Set up authorization and mint tokens.
	daemon.authorizationIndex.SetPrincipal("agent/alpha", schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{"ticket/**"}},
		},
	})

	tokenDir, minted, err := daemon.mintServiceTokens("agent/alpha", []string{"ticket"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	daemon.recordMintedTokens("agent/alpha", minted)

	// Start a service socket to receive revocations.
	socketDir := testutil.SocketDir(t)
	socketPath := filepath.Join(socketDir, "ticket.sock")

	blacklist := servicetoken.NewBlacklist()
	server := service.NewSocketServer(socketPath, daemon.logger, &service.AuthConfig{
		PublicKey: publicKey,
		Audience:  "ticket",
		Blacklist: blacklist,
	})
	server.RegisterRevocationHandler()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		server.Serve(ctx)
	}()

	waitForSocket(t, socketPath)

	// Set up cached service mounts.
	daemon.lastServiceMounts["agent/alpha"] = []launcherServiceMount{
		{Role: "ticket", SocketPath: socketPath},
	}

	// Simulate sandbox destroy.
	daemon.revokeAndCleanupTokens(ctx, "agent/alpha")

	// Service should have the token blacklisted.
	if blacklist.Len() != 1 {
		t.Fatalf("blacklist length = %d, want 1", blacklist.Len())
	}
	if !blacklist.IsRevoked(minted[0].id) {
		t.Errorf("token %q should be revoked on service", minted[0].id)
	}

	// Token directory should be removed from disk.
	if _, err := os.Stat(tokenDir); !os.IsNotExist(err) {
		t.Errorf("token directory should have been removed, err=%v", err)
	}

	// Daemon tracking should be cleared.
	if _, exists := daemon.activeTokens["agent/alpha"]; exists {
		t.Error("activeTokens should be cleared after revoke+cleanup")
	}

	cancel()
	wg.Wait()
}

// waitForSocket polls until a Unix socket file exists, bounded by
// the test context deadline.
func waitForSocket(t *testing.T, path string) {
	t.Helper()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if t.Context().Err() != nil {
			t.Fatalf("socket %s did not appear before test context expired", path)
		}
		runtime.Gosched()
	}
}

// mockLauncherServer sets up a mock launcher that responds to IPC
// requests. Used by tests that need the full destroy flow. Returns
// the socket path and a channel that receives destroyed principal
// localparts.
func mockLauncherServer(t *testing.T) (string, chan string) {
	t.Helper()

	socketDir := testutil.SocketDir(t)
	socketPath := filepath.Join(socketDir, "launcher.sock")
	destroyed := make(chan string, 10)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			var request ipc.Request
			if err := codec.NewDecoder(conn).Decode(&request); err != nil {
				conn.Close()
				continue
			}

			response := ipc.Response{OK: true}
			if request.Action == "destroy-sandbox" {
				destroyed <- request.Principal
			}
			codec.NewEncoder(conn).Encode(response)
			conn.Close()
		}
	}()

	t.Cleanup(func() {
		listener.Close()
	})

	return socketPath, destroyed
}
