// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"testing"
)

// testAuthenticator implements PeerAuthenticator for tests using
// in-memory Ed25519 keypairs.
type testAuthenticator struct {
	privateKey ed25519.PrivateKey
	peerKeys   map[string]ed25519.PublicKey
}

func (a *testAuthenticator) Sign(message []byte) []byte {
	return ed25519.Sign(a.privateKey, message)
}

func (a *testAuthenticator) VerifyPeer(peerLocalpart string, message, signature []byte) error {
	publicKey, ok := a.peerKeys[peerLocalpart]
	if !ok {
		return fmt.Errorf("unknown peer: %s", peerLocalpart)
	}
	if !ed25519.Verify(publicKey, message, signature) {
		return fmt.Errorf("Ed25519 signature verification failed for %s", peerLocalpart)
	}
	return nil
}

// newTestKeypair generates a fresh Ed25519 keypair for testing.
func newTestKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating Ed25519 keypair: %v", err)
	}
	return publicKey, privateKey
}

// TestRunPeerAuth_MutualSuccess verifies that two peers with valid
// keypairs and each other's public keys complete authentication.
func TestRunPeerAuth_MutualSuccess(t *testing.T) {
	publicKeyAlpha, privateKeyAlpha := newTestKeypair(t)
	publicKeyBeta, privateKeyBeta := newTestKeypair(t)

	authAlpha := &testAuthenticator{
		privateKey: privateKeyAlpha,
		peerKeys:   map[string]ed25519.PublicKey{"machine/beta": publicKeyBeta},
	}
	authBeta := &testAuthenticator{
		privateKey: privateKeyBeta,
		peerKeys:   map[string]ed25519.PublicKey{"machine/alpha": publicKeyAlpha},
	}

	connectionAlpha, connectionBeta := net.Pipe()

	errors := make(chan error, 2)
	go func() {
		errors <- runPeerAuth(connectionAlpha, authAlpha, "machine/alpha", "machine/beta")
		connectionAlpha.Close()
	}()
	go func() {
		errors <- runPeerAuth(connectionBeta, authBeta, "machine/beta", "machine/alpha")
		connectionBeta.Close()
	}()

	for range 2 {
		if err := <-errors; err != nil {
			t.Fatalf("authentication failed: %v", err)
		}
	}
}

// TestRunPeerAuth_WrongKey verifies that authentication fails when a
// peer presents a signature from a different key than the one the
// verifier expects.
func TestRunPeerAuth_WrongKey(t *testing.T) {
	publicKeyAlpha, privateKeyAlpha := newTestKeypair(t)
	_, privateKeyBeta := newTestKeypair(t)
	_, privateKeyRogue := newTestKeypair(t)

	// Alpha knows the real beta key, but beta's slot is filled by a
	// rogue that has a different private key.
	authAlpha := &testAuthenticator{
		privateKey: privateKeyAlpha,
		peerKeys:   map[string]ed25519.PublicKey{"machine/beta": privateKeyBeta.Public().(ed25519.PublicKey)},
	}
	authRogue := &testAuthenticator{
		privateKey: privateKeyRogue,
		peerKeys:   map[string]ed25519.PublicKey{"machine/alpha": publicKeyAlpha},
	}

	connectionAlpha, connectionRogue := net.Pipe()

	errors := make(chan error, 2)
	go func() {
		errors <- runPeerAuth(connectionAlpha, authAlpha, "machine/alpha", "machine/beta")
		connectionAlpha.Close()
	}()
	go func() {
		errors <- runPeerAuth(connectionRogue, authRogue, "machine/beta", "machine/alpha")
		connectionRogue.Close()
	}()

	// At least one side must fail (the side verifying the rogue's
	// signature). The other side may fail with a read error when the
	// first side tears down.
	var failures int
	for range 2 {
		if err := <-errors; err != nil {
			failures++
		}
	}
	if failures == 0 {
		t.Fatal("expected at least one authentication failure, got none")
	}
}

// TestRunPeerAuth_UnknownPeer verifies that authentication fails when
// the verifier has no public key for the claimed peer identity.
func TestRunPeerAuth_UnknownPeer(t *testing.T) {
	publicKeyAlpha, privateKeyAlpha := newTestKeypair(t)
	_, privateKeyBeta := newTestKeypair(t)

	authAlpha := &testAuthenticator{
		privateKey: privateKeyAlpha,
		peerKeys:   map[string]ed25519.PublicKey{}, // no keys at all
	}
	authBeta := &testAuthenticator{
		privateKey: privateKeyBeta,
		peerKeys:   map[string]ed25519.PublicKey{"machine/alpha": publicKeyAlpha},
	}

	connectionAlpha, connectionBeta := net.Pipe()

	errors := make(chan error, 2)
	go func() {
		errors <- runPeerAuth(connectionAlpha, authAlpha, "machine/alpha", "machine/beta")
		connectionAlpha.Close()
	}()
	go func() {
		errors <- runPeerAuth(connectionBeta, authBeta, "machine/beta", "machine/alpha")
		connectionBeta.Close()
	}()

	var failures int
	for range 2 {
		if err := <-errors; err != nil {
			failures++
		}
	}
	if failures == 0 {
		t.Fatal("expected at least one authentication failure, got none")
	}
}

// TestRunPeerAuth_BrokenChannel verifies that authentication fails
// gracefully when the underlying connection breaks mid-handshake.
func TestRunPeerAuth_BrokenChannel(t *testing.T) {
	_, privateKeyAlpha := newTestKeypair(t)
	publicKeyBeta, _ := newTestKeypair(t)

	authAlpha := &testAuthenticator{
		privateKey: privateKeyAlpha,
		peerKeys:   map[string]ed25519.PublicKey{"machine/beta": publicKeyBeta},
	}

	connectionAlpha, connectionBeta := net.Pipe()

	// Close beta's side immediately to simulate a broken connection.
	connectionBeta.Close()

	err := runPeerAuth(connectionAlpha, authAlpha, "machine/alpha", "machine/beta")
	if err == nil {
		t.Fatal("expected error from broken channel, got nil")
	}
}
