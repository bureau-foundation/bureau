// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package servicetoken

import (
	"crypto/ed25519"
	"errors"
	"testing"
)

func TestSignUpstreamConfig_RoundTrip(t *testing.T) {
	public, private, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	config := &UpstreamConfig{
		UpstreamSocket: "/run/bureau/tunnel/artifact-cache.sock",
		IssuedAt:       1735689500,
	}

	signed, err := SignUpstreamConfig(private, config)
	if err != nil {
		t.Fatalf("SignUpstreamConfig: %v", err)
	}

	decoded, err := VerifyUpstreamConfig(public, signed)
	if err != nil {
		t.Fatalf("VerifyUpstreamConfig: %v", err)
	}

	if decoded.UpstreamSocket != "/run/bureau/tunnel/artifact-cache.sock" {
		t.Errorf("UpstreamSocket = %q, want %q", decoded.UpstreamSocket, "/run/bureau/tunnel/artifact-cache.sock")
	}
	if decoded.IssuedAt != 1735689500 {
		t.Errorf("IssuedAt = %d, want 1735689500", decoded.IssuedAt)
	}
}

func TestSignUpstreamConfig_EmptySocket(t *testing.T) {
	public, private, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	config := &UpstreamConfig{
		UpstreamSocket: "",
		IssuedAt:       1735689500,
	}

	signed, err := SignUpstreamConfig(private, config)
	if err != nil {
		t.Fatalf("SignUpstreamConfig: %v", err)
	}

	decoded, err := VerifyUpstreamConfig(public, signed)
	if err != nil {
		t.Fatalf("VerifyUpstreamConfig: %v", err)
	}

	if decoded.UpstreamSocket != "" {
		t.Errorf("UpstreamSocket = %q, want empty", decoded.UpstreamSocket)
	}
}

func TestVerifyUpstreamConfig_WrongKey(t *testing.T) {
	_, signingKey, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	wrongPublic, _, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	config := &UpstreamConfig{
		UpstreamSocket: "/some/path",
		IssuedAt:       1735689500,
	}

	signed, err := SignUpstreamConfig(signingKey, config)
	if err != nil {
		t.Fatalf("SignUpstreamConfig: %v", err)
	}

	_, err = VerifyUpstreamConfig(wrongPublic, signed)
	if !errors.Is(err, ErrUpstreamConfigBadSig) {
		t.Errorf("VerifyUpstreamConfig with wrong key: got %v, want %v", err, ErrUpstreamConfigBadSig)
	}
}

func TestVerifyUpstreamConfig_TruncatedData(t *testing.T) {
	_, err := VerifyUpstreamConfig(make(ed25519.PublicKey, ed25519.PublicKeySize), []byte("short"))
	if !errors.Is(err, ErrUpstreamConfigTooShort) {
		t.Errorf("VerifyUpstreamConfig with truncated data: got %v, want %v", err, ErrUpstreamConfigTooShort)
	}
}

func TestVerifyUpstreamConfig_TamperedPayload(t *testing.T) {
	public, private, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	config := &UpstreamConfig{
		UpstreamSocket: "/run/bureau/tunnel/artifact-cache.sock",
		IssuedAt:       1735689500,
	}

	signed, err := SignUpstreamConfig(private, config)
	if err != nil {
		t.Fatalf("SignUpstreamConfig: %v", err)
	}

	// Flip a byte in the payload region.
	signed[0] ^= 0xff

	_, err = VerifyUpstreamConfig(public, signed)
	if !errors.Is(err, ErrUpstreamConfigBadSig) {
		t.Errorf("VerifyUpstreamConfig with tampered payload: got %v, want %v", err, ErrUpstreamConfigBadSig)
	}
}
