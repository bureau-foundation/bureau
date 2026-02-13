// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package servicetoken

import (
	"crypto/ed25519"
	"errors"
	"testing"
	"time"
)

func testKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	return public, private
}

func TestMintAndVerify(t *testing.T) {
	public, private := testKeypair(t)

	now := time.Now()
	token := &Token{
		Subject:  "iree/amdgpu/pm",
		Machine:  "machine/workstation",
		Audience: "ticket",
		Grants: []Grant{
			{Actions: []string{"ticket/create", "ticket/assign"}},
			{Actions: []string{"ticket/close"}, Targets: []string{"iree/**"}},
		},
		ID:        "a1b2c3d4e5f6",
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(5 * time.Minute).Unix(),
	}

	tokenBytes, err := Mint(private, token)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// Token should be CBOR payload + 64-byte signature.
	if len(tokenBytes) <= signatureSize {
		t.Fatalf("token too short: %d bytes", len(tokenBytes))
	}

	verified, err := Verify(public, tokenBytes)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if verified.Subject != "iree/amdgpu/pm" {
		t.Errorf("Subject = %q, want iree/amdgpu/pm", verified.Subject)
	}
	if verified.Machine != "machine/workstation" {
		t.Errorf("Machine = %q, want machine/workstation", verified.Machine)
	}
	if verified.Audience != "ticket" {
		t.Errorf("Audience = %q, want ticket", verified.Audience)
	}
	if verified.ID != "a1b2c3d4e5f6" {
		t.Errorf("ID = %q, want a1b2c3d4e5f6", verified.ID)
	}
	if len(verified.Grants) != 2 {
		t.Errorf("Grants length = %d, want 2", len(verified.Grants))
	}
	if verified.Grants[0].Actions[0] != "ticket/create" {
		t.Errorf("Grants[0].Actions[0] = %q, want ticket/create", verified.Grants[0].Actions[0])
	}
}

func TestVerify_TamperedPayload(t *testing.T) {
	public, private := testKeypair(t)

	token := &Token{
		Subject:   "agent",
		Machine:   "machine",
		Audience:  "ticket",
		ID:        "id1",
		IssuedAt:  time.Now().Unix(),
		ExpiresAt: time.Now().Add(5 * time.Minute).Unix(),
	}

	tokenBytes, err := Mint(private, token)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// Tamper with a payload byte.
	tokenBytes[0] ^= 0xFF

	_, err = Verify(public, tokenBytes)
	if !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("Verify tampered token: got %v, want ErrInvalidSignature", err)
	}
}

func TestVerify_WrongKey(t *testing.T) {
	_, private := testKeypair(t)
	otherPublic, _ := testKeypair(t)

	token := &Token{
		Subject:   "agent",
		Machine:   "machine",
		Audience:  "ticket",
		ID:        "id1",
		IssuedAt:  time.Now().Unix(),
		ExpiresAt: time.Now().Add(5 * time.Minute).Unix(),
	}

	tokenBytes, err := Mint(private, token)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	_, err = Verify(otherPublic, tokenBytes)
	if !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("Verify with wrong key: got %v, want ErrInvalidSignature", err)
	}
}

func TestVerify_ExpiredToken(t *testing.T) {
	public, private := testKeypair(t)

	now := time.Now()
	token := &Token{
		Subject:   "agent",
		Machine:   "machine",
		Audience:  "ticket",
		ID:        "id1",
		IssuedAt:  now.Add(-10 * time.Minute).Unix(),
		ExpiresAt: now.Add(-5 * time.Minute).Unix(),
	}

	tokenBytes, err := Mint(private, token)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	_, err = Verify(public, tokenBytes)
	if !errors.Is(err, ErrTokenExpired) {
		t.Errorf("Verify expired token: got %v, want ErrTokenExpired", err)
	}
}

func TestVerify_TooShort(t *testing.T) {
	public, _ := testKeypair(t)

	// Exactly 64 bytes (all signature, no payload).
	tokenBytes := make([]byte, signatureSize)
	_, err := Verify(public, tokenBytes)
	if !errors.Is(err, ErrTokenTooShort) {
		t.Errorf("Verify too-short token: got %v, want ErrTokenTooShort", err)
	}

	// Empty.
	_, err = Verify(public, nil)
	if !errors.Is(err, ErrTokenTooShort) {
		t.Errorf("Verify nil token: got %v, want ErrTokenTooShort", err)
	}
}

func TestVerifyAt_Deterministic(t *testing.T) {
	public, private := testKeypair(t)

	expiresAt := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	token := &Token{
		Subject:   "agent",
		Machine:   "machine",
		Audience:  "ticket",
		ID:        "id1",
		IssuedAt:  expiresAt.Add(-5 * time.Minute).Unix(),
		ExpiresAt: expiresAt.Unix(),
	}

	tokenBytes, err := Mint(private, token)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// Before expiry: valid.
	before := expiresAt.Add(-time.Second)
	if _, err := VerifyAt(public, tokenBytes, before); err != nil {
		t.Errorf("before expiry: %v", err)
	}

	// At expiry: expired (not strictly before).
	if _, err := VerifyAt(public, tokenBytes, expiresAt); err == nil {
		t.Error("at expiry: expected error")
	}

	// After expiry: expired.
	after := expiresAt.Add(time.Second)
	if _, err := VerifyAt(public, tokenBytes, after); err == nil {
		t.Error("after expiry: expected error")
	}
}

func TestVerifyForService(t *testing.T) {
	public, private := testKeypair(t)

	token := &Token{
		Subject:   "agent",
		Machine:   "machine",
		Audience:  "ticket",
		ID:        "id1",
		IssuedAt:  time.Now().Unix(),
		ExpiresAt: time.Now().Add(5 * time.Minute).Unix(),
	}

	tokenBytes, err := Mint(private, token)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// Correct audience.
	verified, err := VerifyForService(public, tokenBytes, "ticket")
	if err != nil {
		t.Fatalf("VerifyForService correct audience: %v", err)
	}
	if verified.Subject != "agent" {
		t.Errorf("Subject = %q, want agent", verified.Subject)
	}

	// Wrong audience.
	_, err = VerifyForService(public, tokenBytes, "artifact")
	if !errors.Is(err, ErrAudienceMismatch) {
		t.Errorf("VerifyForService wrong audience: got %v, want ErrAudienceMismatch", err)
	}
}

func TestGrantsAllow(t *testing.T) {
	grants := []Grant{
		{Actions: []string{"ticket/create", "ticket/assign"}},
		{Actions: []string{"ticket/close"}, Targets: []string{"iree/**"}},
	}

	tests := []struct {
		action string
		target string
		want   bool
	}{
		{"ticket/create", "", true},
		{"ticket/assign", "", true},
		{"ticket/close", "iree/amdgpu/pm", true},
		{"ticket/close", "bureau/dev/coder", false},
		{"ticket/close", "", true}, // self-service check on targeted grant
		{"fleet/assign", "", false},
		{"observe", "", false},
	}

	for _, tt := range tests {
		got := GrantsAllow(grants, tt.action, tt.target)
		if got != tt.want {
			t.Errorf("GrantsAllow(%q, %q) = %v, want %v", tt.action, tt.target, got, tt.want)
		}
	}
}

func TestGrantsAllow_WildcardPatterns(t *testing.T) {
	grants := []Grant{
		{Actions: []string{"command/**"}},
		{Actions: []string{"observe/**"}, Targets: []string{"**"}},
	}

	tests := []struct {
		action string
		target string
		want   bool
	}{
		{"command/ticket/create", "", true},
		{"command/artifact/fetch", "", true},
		{"observe/readwrite", "any/principal", true},
		{"observe", "any/principal", true},
		{"interrupt", "", false},
	}

	for _, tt := range tests {
		got := GrantsAllow(grants, tt.action, tt.target)
		if got != tt.want {
			t.Errorf("GrantsAllow(%q, %q) = %v, want %v", tt.action, tt.target, got, tt.want)
		}
	}
}

func TestGrantsAllow_EmptyGrants(t *testing.T) {
	if GrantsAllow(nil, "ticket/create", "") {
		t.Error("nil grants should deny")
	}
	if GrantsAllow([]Grant{}, "ticket/create", "") {
		t.Error("empty grants should deny")
	}
}

func TestMintVerify_NoGrants(t *testing.T) {
	public, private := testKeypair(t)

	token := &Token{
		Subject:   "agent",
		Machine:   "machine",
		Audience:  "ticket",
		ID:        "id1",
		IssuedAt:  time.Now().Unix(),
		ExpiresAt: time.Now().Add(5 * time.Minute).Unix(),
	}

	tokenBytes, err := Mint(private, token)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	verified, err := Verify(public, tokenBytes)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if len(verified.Grants) != 0 {
		t.Errorf("Grants = %v, want empty", verified.Grants)
	}
}

func TestTokenWireSize(t *testing.T) {
	_, private := testKeypair(t)

	// A typical token with a few grants.
	token := &Token{
		Subject:  "bureau/dev/workspace/coder/0",
		Machine:  "machine/workstation",
		Audience: "ticket",
		Grants: []Grant{
			{Actions: []string{"ticket/create", "ticket/assign"}},
			{Actions: []string{"ticket/close"}, Targets: []string{"bureau/dev/workspace/**"}},
		},
		ID:        "a1b2c3d4e5f67890",
		IssuedAt:  1709251200,
		ExpiresAt: 1709251500,
	}

	tokenBytes, err := Mint(private, token)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	payloadSize := len(tokenBytes) - signatureSize
	t.Logf("token wire size: %d bytes total (%d payload + %d signature)",
		len(tokenBytes), payloadSize, signatureSize)

	// Sanity check: a typical token should be well under 1KB.
	if len(tokenBytes) > 1024 {
		t.Errorf("token unexpectedly large: %d bytes", len(tokenBytes))
	}
}
