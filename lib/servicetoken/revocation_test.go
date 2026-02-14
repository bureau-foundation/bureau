// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package servicetoken

import (
	"crypto/ed25519"
	"errors"
	"testing"
)

func TestSignRevocation_RoundTrip(t *testing.T) {
	public, private, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	request := &RevocationRequest{
		Entries: []RevocationEntry{
			{TokenID: "aabbccdd11223344", ExpiresAt: 1735689600},
			{TokenID: "eeff00112233aabb", ExpiresAt: 1735689900},
		},
		IssuedAt: 1735689500,
	}

	signed, err := SignRevocation(private, request)
	if err != nil {
		t.Fatalf("SignRevocation: %v", err)
	}

	decoded, err := VerifyRevocation(public, signed)
	if err != nil {
		t.Fatalf("VerifyRevocation: %v", err)
	}

	if len(decoded.Entries) != 2 {
		t.Fatalf("Entries length = %d, want 2", len(decoded.Entries))
	}
	if decoded.Entries[0].TokenID != "aabbccdd11223344" {
		t.Errorf("Entries[0].TokenID = %q, want %q", decoded.Entries[0].TokenID, "aabbccdd11223344")
	}
	if decoded.Entries[0].ExpiresAt != 1735689600 {
		t.Errorf("Entries[0].ExpiresAt = %d, want 1735689600", decoded.Entries[0].ExpiresAt)
	}
	if decoded.Entries[1].TokenID != "eeff00112233aabb" {
		t.Errorf("Entries[1].TokenID = %q, want %q", decoded.Entries[1].TokenID, "eeff00112233aabb")
	}
	if decoded.IssuedAt != 1735689500 {
		t.Errorf("IssuedAt = %d, want 1735689500", decoded.IssuedAt)
	}
}

func TestVerifyRevocation_WrongKey(t *testing.T) {
	_, signingKey, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	wrongPublic, _, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	request := &RevocationRequest{
		Entries:  []RevocationEntry{{TokenID: "aabb", ExpiresAt: 1735689600}},
		IssuedAt: 1735689500,
	}

	signed, err := SignRevocation(signingKey, request)
	if err != nil {
		t.Fatalf("SignRevocation: %v", err)
	}

	_, err = VerifyRevocation(wrongPublic, signed)
	if !errors.Is(err, ErrRevocationBadSig) {
		t.Errorf("VerifyRevocation with wrong key: got %v, want %v", err, ErrRevocationBadSig)
	}
}

func TestVerifyRevocation_TruncatedData(t *testing.T) {
	_, err := VerifyRevocation(make(ed25519.PublicKey, ed25519.PublicKeySize), []byte("short"))
	if !errors.Is(err, ErrRevocationTooShort) {
		t.Errorf("VerifyRevocation with truncated data: got %v, want %v", err, ErrRevocationTooShort)
	}
}

func TestVerifyRevocation_ExactlySignatureSize(t *testing.T) {
	// Data that is exactly 64 bytes (signature size) — no room for payload.
	data := make([]byte, signatureSize)
	_, err := VerifyRevocation(make(ed25519.PublicKey, ed25519.PublicKeySize), data)
	if !errors.Is(err, ErrRevocationTooShort) {
		t.Errorf("VerifyRevocation with signature-only data: got %v, want %v", err, ErrRevocationTooShort)
	}
}

func TestVerifyRevocation_TamperedPayload(t *testing.T) {
	public, private, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	request := &RevocationRequest{
		Entries:  []RevocationEntry{{TokenID: "aabb", ExpiresAt: 1735689600}},
		IssuedAt: 1735689500,
	}

	signed, err := SignRevocation(private, request)
	if err != nil {
		t.Fatalf("SignRevocation: %v", err)
	}

	// Flip a byte in the payload region.
	signed[0] ^= 0xff

	_, err = VerifyRevocation(public, signed)
	if !errors.Is(err, ErrRevocationBadSig) {
		t.Errorf("VerifyRevocation with tampered payload: got %v, want %v", err, ErrRevocationBadSig)
	}
}

func TestSignRevocation_SingleEntry(t *testing.T) {
	public, private, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	request := &RevocationRequest{
		Entries:  []RevocationEntry{{TokenID: "single-token", ExpiresAt: 9999999999}},
		IssuedAt: 1735689500,
	}

	signed, err := SignRevocation(private, request)
	if err != nil {
		t.Fatalf("SignRevocation: %v", err)
	}

	decoded, err := VerifyRevocation(public, signed)
	if err != nil {
		t.Fatalf("VerifyRevocation: %v", err)
	}

	if len(decoded.Entries) != 1 {
		t.Fatalf("Entries length = %d, want 1", len(decoded.Entries))
	}
	if decoded.Entries[0].TokenID != "single-token" {
		t.Errorf("TokenID = %q, want %q", decoded.Entries[0].TokenID, "single-token")
	}
}
