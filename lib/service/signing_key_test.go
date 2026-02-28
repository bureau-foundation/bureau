// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"crypto/ed25519"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
)

func TestValidateTokenSigningKeyValid(t *testing.T) {
	publicKey, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	content := schema.TokenSigningKeyContent{
		PublicKey: hex.EncodeToString(publicKey),
		Machine:   ref.MustParseUserID("@machine/test:test.local"),
	}

	result, err := validateTokenSigningKey(content, "machine/test")
	if err != nil {
		t.Fatalf("validateTokenSigningKey: %v", err)
	}

	if !result.Equal(publicKey) {
		t.Fatal("parsed key does not match original")
	}
}

func TestValidateTokenSigningKeyEmptyPublicKey(t *testing.T) {
	content := schema.TokenSigningKeyContent{
		PublicKey: "",
		Machine:   ref.MustParseUserID("@machine/test:test.local"),
	}

	_, err := validateTokenSigningKey(content, "machine/test")
	if err == nil {
		t.Fatal("expected error for empty public_key")
	}
	if !strings.Contains(err.Error(), "empty public_key field") {
		t.Errorf("error should mention empty field, got: %v", err)
	}
	if !strings.Contains(err.Error(), "machine/test") {
		t.Errorf("error should include machine identifier, got: %v", err)
	}
}

func TestValidateTokenSigningKeyInvalidHex(t *testing.T) {
	content := schema.TokenSigningKeyContent{
		PublicKey: "zzzz-not-hex",
		Machine:   ref.MustParseUserID("@machine/test:test.local"),
	}

	_, err := validateTokenSigningKey(content, "machine/test")
	if err == nil {
		t.Fatal("expected error for invalid hex")
	}
	if !strings.Contains(err.Error(), "hex-decoding") {
		t.Errorf("error should mention hex decoding, got: %v", err)
	}
}

func TestValidateTokenSigningKeyWrongLength(t *testing.T) {
	// 16 bytes instead of 32 — valid hex but wrong key size.
	shortKey := hex.EncodeToString(make([]byte, 16))
	content := schema.TokenSigningKeyContent{
		PublicKey: shortKey,
		Machine:   ref.MustParseUserID("@machine/test:test.local"),
	}

	_, err := validateTokenSigningKey(content, "machine/test")
	if err == nil {
		t.Fatal("expected error for wrong key length")
	}
	if !strings.Contains(err.Error(), "wrong length") {
		t.Errorf("error should mention wrong length, got: %v", err)
	}
	if !strings.Contains(err.Error(), "16") {
		t.Errorf("error should include actual length, got: %v", err)
	}
}

func TestValidateTokenSigningKeyTooLong(t *testing.T) {
	// 64 bytes instead of 32 — valid hex, wrong size.
	longKey := hex.EncodeToString(make([]byte, 64))
	content := schema.TokenSigningKeyContent{
		PublicKey: longKey,
		Machine:   ref.MustParseUserID("@machine/test:test.local"),
	}

	_, err := validateTokenSigningKey(content, "machine/test")
	if err == nil {
		t.Fatal("expected error for oversized key")
	}
	if !strings.Contains(err.Error(), "wrong length") {
		t.Errorf("error should mention wrong length, got: %v", err)
	}
}
