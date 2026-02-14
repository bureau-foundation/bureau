// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"crypto/rand"
	"fmt"
	"io"
	"time"
)

// authChannelLabel is the data channel label for the mutual authentication
// handshake. Both peers recognize this label and route it to the auth
// handler instead of the HTTP request handler.
const authChannelLabel = "auth"

// authNonceSize is the size of the random challenge nonce in bytes.
const authNonceSize = 32

// authSignatureSize is the size of an Ed25519 signature in bytes.
const authSignatureSize = 64

// authTimeout is the maximum time allowed for the entire peer
// authentication handshake (channel creation, nonce exchange, signing,
// verification). If auth does not complete within this window, the
// PeerConnection is torn down.
const authTimeout = 10 * time.Second

// PeerAuthenticator provides cryptographic identity verification for
// transport connections between peer daemons. When configured on a
// [WebRTCTransport], each new PeerConnection must complete a mutual
// challenge-response handshake before HTTP data channels are accepted.
//
// After ICE connects, both peers exchange random 32-byte nonces over a
// dedicated "auth" data channel, sign each other's nonces with their
// Ed25519 private keys, and verify the signatures using the peer's
// published public key. This binds the transport connection to the
// machines' cryptographic identities, preventing impersonation by rogue
// peers that gain access to the signaling channel.
type PeerAuthenticator interface {
	// Sign signs the given message with the local machine's Ed25519
	// private key. Returns a 64-byte Ed25519 signature.
	Sign(message []byte) []byte

	// VerifyPeer verifies that signature is a valid Ed25519 signature
	// of message produced by the machine identified by peerLocalpart.
	// The implementation looks up the peer's public key (typically from
	// Matrix room state) and verifies the signature. Returns an error
	// if the peer's public key is unknown or the signature is invalid.
	VerifyPeer(peerLocalpart string, message, signature []byte) error
}

// runPeerAuth executes the mutual authentication protocol on a data
// channel. Both peers run this function simultaneously on the same
// channel. The protocol is:
//
//  1. Send a 32-byte random nonce
//  2. Read the peer's 32-byte nonce
//  3. Sign (peerNonce || peerLocalpart) — binding the response to the
//     specific challenger's identity
//  4. Send the 64-byte Ed25519 signature
//  5. Read the peer's 64-byte signature
//  6. Verify it against (ownNonce || ownLocalpart) using the peer's key
//
// The localpart binding in step 3 prevents a valid signature for peer A
// from being replayed to authenticate against peer B.
//
// Writes and reads are interleaved using a background writer goroutine
// to avoid deadlock on synchronous channels (such as net.Pipe), where
// Write blocks until the peer Reads. Without concurrent write/read,
// both sides would block on their initial Write simultaneously.
//
// The caller is responsible for closing the channel after this returns.
func runPeerAuth(channel io.ReadWriter, authenticator PeerAuthenticator, localpart, peerLocalpart string) error {
	// Generate random nonce.
	nonce := make([]byte, authNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("generating auth nonce: %w", err)
	}

	// writeErrors collects errors from the background writer goroutine.
	// The writer sends both the nonce and (later) the signature.
	writeErrors := make(chan error, 1)
	signatureToSend := make(chan []byte, 1)

	// Background writer: sends our nonce, then waits for the signature
	// to be computed by the main goroutine, then sends the signature.
	go func() {
		if _, err := channel.Write(nonce); err != nil {
			writeErrors <- fmt.Errorf("sending auth nonce: %w", err)
			return
		}
		signature, ok := <-signatureToSend
		if !ok {
			return
		}
		if _, err := channel.Write(signature); err != nil {
			writeErrors <- fmt.Errorf("sending auth signature: %w", err)
			return
		}
		writeErrors <- nil
	}()

	// Main goroutine: read the peer's nonce.
	peerNonce := make([]byte, authNonceSize)
	if _, err := io.ReadFull(channel, peerNonce); err != nil {
		close(signatureToSend)
		return fmt.Errorf("reading peer nonce: %w", err)
	}

	// Sign (peerNonce || peerLocalpart): "I am responding to this
	// challenge from the machine that claims to be <peerLocalpart>."
	signedMessage := make([]byte, 0, authNonceSize+len(peerLocalpart))
	signedMessage = append(signedMessage, peerNonce...)
	signedMessage = append(signedMessage, peerLocalpart...)
	signature := authenticator.Sign(signedMessage)

	// Hand the signature to the background writer.
	signatureToSend <- signature

	// Read the peer's signature.
	peerSignature := make([]byte, authSignatureSize)
	if _, err := io.ReadFull(channel, peerSignature); err != nil {
		return fmt.Errorf("reading peer signature: %w", err)
	}

	// Wait for the writer goroutine to finish.
	if err := <-writeErrors; err != nil {
		return err
	}

	// Verify: peer signed (nonce || localpart), i.e., the peer
	// responded to OUR challenge bound to OUR identity.
	verifyMessage := make([]byte, 0, authNonceSize+len(localpart))
	verifyMessage = append(verifyMessage, nonce...)
	verifyMessage = append(verifyMessage, localpart...)
	if err := authenticator.VerifyPeer(peerLocalpart, verifyMessage, peerSignature); err != nil {
		return fmt.Errorf("peer %s failed authentication: %w", peerLocalpart, err)
	}

	return nil
}
