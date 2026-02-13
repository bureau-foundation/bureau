// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
)

func TestParseTokenSigningKeyValid(t *testing.T) {
	publicKey, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	raw := marshalSigningKeyContent(t, schema.TokenSigningKeyContent{
		PublicKey: hex.EncodeToString(publicKey),
		Machine:   "machine/test",
	})

	result, err := parseTokenSigningKey(raw, "machine/test")
	if err != nil {
		t.Fatalf("parseTokenSigningKey: %v", err)
	}

	if !result.Equal(publicKey) {
		t.Fatal("parsed key does not match original")
	}
}

func TestParseTokenSigningKeyEmptyPublicKey(t *testing.T) {
	raw := marshalSigningKeyContent(t, schema.TokenSigningKeyContent{
		PublicKey: "",
		Machine:   "machine/test",
	})

	_, err := parseTokenSigningKey(raw, "machine/test")
	if err == nil {
		t.Fatal("expected error for empty public_key")
	}
	if !strings.Contains(err.Error(), "empty public_key field") {
		t.Errorf("error should mention empty field, got: %v", err)
	}
	if !strings.Contains(err.Error(), "machine/test") {
		t.Errorf("error should include machine localpart, got: %v", err)
	}
}

func TestParseTokenSigningKeyInvalidHex(t *testing.T) {
	raw := marshalSigningKeyContent(t, schema.TokenSigningKeyContent{
		PublicKey: "zzzz-not-hex",
		Machine:   "machine/test",
	})

	_, err := parseTokenSigningKey(raw, "machine/test")
	if err == nil {
		t.Fatal("expected error for invalid hex")
	}
	if !strings.Contains(err.Error(), "hex-decoding") {
		t.Errorf("error should mention hex decoding, got: %v", err)
	}
}

func TestParseTokenSigningKeyWrongLength(t *testing.T) {
	// 16 bytes instead of 32 — valid hex but wrong key size.
	shortKey := hex.EncodeToString(make([]byte, 16))
	raw := marshalSigningKeyContent(t, schema.TokenSigningKeyContent{
		PublicKey: shortKey,
		Machine:   "machine/test",
	})

	_, err := parseTokenSigningKey(raw, "machine/test")
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

func TestParseTokenSigningKeyInvalidJSON(t *testing.T) {
	_, err := parseTokenSigningKey([]byte(`{not json`), "machine/test")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "parsing token signing key") {
		t.Errorf("error should mention parsing, got: %v", err)
	}
}

func TestParseTokenSigningKeyTooLong(t *testing.T) {
	// 64 bytes instead of 32 — valid hex, wrong size.
	longKey := hex.EncodeToString(make([]byte, 64))
	raw := marshalSigningKeyContent(t, schema.TokenSigningKeyContent{
		PublicKey: longKey,
		Machine:   "machine/test",
	})

	_, err := parseTokenSigningKey(raw, "machine/test")
	if err == nil {
		t.Fatal("expected error for oversized key")
	}
	if !strings.Contains(err.Error(), "wrong length") {
		t.Errorf("error should mention wrong length, got: %v", err)
	}
}

// marshalSigningKeyContent marshals a TokenSigningKeyContent to JSON,
// matching how the homeserver delivers state events.
func marshalSigningKeyContent(t *testing.T, content schema.TokenSigningKeyContent) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("marshaling signing key content: %v", err)
	}
	return data
}
