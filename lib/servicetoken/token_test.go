// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package servicetoken

import (
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/fleet"
	"github.com/bureau-foundation/bureau/lib/schema/observation"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
)

func testKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	return public, private
}

func mustParseUserID(t *testing.T, raw string) ref.UserID {
	t.Helper()
	userID, err := ref.ParseUserID(raw)
	if err != nil {
		t.Fatalf("ParseUserID(%q): %v", raw, err)
	}
	return userID
}

func mustParseMachine(t *testing.T, raw string) ref.Machine {
	t.Helper()
	machine, err := ref.ParseMachineUserID(raw)
	if err != nil {
		t.Fatalf("ParseMachineUserID(%q): %v", raw, err)
	}
	return machine
}

func TestMintAndVerify(t *testing.T) {
	public, private := testKeypair(t)

	subject := mustParseUserID(t, "@bureau/fleet/prod/agent/pm:bureau.local")
	machine := mustParseMachine(t, "@bureau/fleet/prod/machine/workstation:bureau.local")

	const issuedAt int64 = 1735689600  // 2025-01-01 00:00:00 UTC
	const expiresAt int64 = 1735693200 // 2025-01-01 01:00:00 UTC
	token := &Token{
		Subject:  subject,
		Machine:  machine,
		Audience: "ticket",
		Grants: []Grant{
			{Actions: []string{ticket.ActionCreate, "ticket/assign"}},
			{Actions: []string{ticket.ActionClose}, Targets: []string{"iree/**:bureau.local"}},
		},
		ID:        "a1b2c3d4e5f6",
		IssuedAt:  issuedAt,
		ExpiresAt: expiresAt,
	}

	tokenBytes, err := Mint(private, token)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// Token should be CBOR payload + 64-byte signature.
	if len(tokenBytes) <= signatureSize {
		t.Fatalf("token too short: %d bytes", len(tokenBytes))
	}

	verifyTime := time.Unix(issuedAt+1800, 0) // midpoint between issued and expiry
	verified, err := VerifyAt(public, tokenBytes, verifyTime)
	if err != nil {
		t.Fatalf("VerifyAt: %v", err)
	}

	if verified.Subject != subject {
		t.Errorf("Subject = %v, want %v", verified.Subject, subject)
	}
	if verified.Machine != machine {
		t.Errorf("Machine = %v, want %v", verified.Machine, machine)
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
	if verified.Grants[0].Actions[0] != ticket.ActionCreate {
		t.Errorf("Grants[0].Actions[0] = %q, want %s", verified.Grants[0].Actions[0], ticket.ActionCreate)
	}
}

func TestVerify_TamperedPayload(t *testing.T) {
	public, private := testKeypair(t)

	const issuedAt int64 = 1735689600  // 2025-01-01 00:00:00 UTC
	const expiresAt int64 = 1735693200 // 2025-01-01 01:00:00 UTC
	token := &Token{
		Subject:   mustParseUserID(t, "@bureau/fleet/prod/agent/tester:bureau.local"),
		Machine:   mustParseMachine(t, "@bureau/fleet/prod/machine/box:bureau.local"),
		Audience:  "ticket",
		ID:        "id1",
		IssuedAt:  issuedAt,
		ExpiresAt: expiresAt,
	}

	tokenBytes, err := Mint(private, token)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// Tamper with a payload byte.
	tokenBytes[0] ^= 0xFF

	verifyTime := time.Unix(issuedAt+1800, 0)
	_, err = VerifyAt(public, tokenBytes, verifyTime)
	if !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("VerifyAt tampered token: got %v, want ErrInvalidSignature", err)
	}
}

func TestVerify_WrongKey(t *testing.T) {
	_, private := testKeypair(t)
	otherPublic, _ := testKeypair(t)

	const issuedAt int64 = 1735689600  // 2025-01-01 00:00:00 UTC
	const expiresAt int64 = 1735693200 // 2025-01-01 01:00:00 UTC
	token := &Token{
		Subject:   mustParseUserID(t, "@bureau/fleet/prod/agent/tester:bureau.local"),
		Machine:   mustParseMachine(t, "@bureau/fleet/prod/machine/box:bureau.local"),
		Audience:  "ticket",
		ID:        "id1",
		IssuedAt:  issuedAt,
		ExpiresAt: expiresAt,
	}

	tokenBytes, err := Mint(private, token)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	verifyTime := time.Unix(issuedAt+1800, 0)
	_, err = VerifyAt(otherPublic, tokenBytes, verifyTime)
	if !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("VerifyAt with wrong key: got %v, want ErrInvalidSignature", err)
	}
}

func TestVerify_ExpiredToken(t *testing.T) {
	public, private := testKeypair(t)

	const issuedAt int64 = 1735689600  // 2025-01-01 00:00:00 UTC
	const expiresAt int64 = 1735693200 // 2025-01-01 01:00:00 UTC
	token := &Token{
		Subject:   mustParseUserID(t, "@bureau/fleet/prod/agent/tester:bureau.local"),
		Machine:   mustParseMachine(t, "@bureau/fleet/prod/machine/box:bureau.local"),
		Audience:  "ticket",
		ID:        "id1",
		IssuedAt:  issuedAt,
		ExpiresAt: expiresAt,
	}

	tokenBytes, err := Mint(private, token)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// Verify at a time after expiry.
	verifyTime := time.Unix(expiresAt+3600, 0)
	_, err = VerifyAt(public, tokenBytes, verifyTime)
	if !errors.Is(err, ErrTokenExpired) {
		t.Errorf("VerifyAt expired token: got %v, want ErrTokenExpired", err)
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
		Subject:   mustParseUserID(t, "@bureau/fleet/prod/agent/tester:bureau.local"),
		Machine:   mustParseMachine(t, "@bureau/fleet/prod/machine/box:bureau.local"),
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

	subject := mustParseUserID(t, "@bureau/fleet/prod/agent/tester:bureau.local")

	const issuedAt int64 = 1735689600  // 2025-01-01 00:00:00 UTC
	const expiresAt int64 = 1735693200 // 2025-01-01 01:00:00 UTC
	token := &Token{
		Subject:   subject,
		Machine:   mustParseMachine(t, "@bureau/fleet/prod/machine/box:bureau.local"),
		Audience:  "ticket",
		ID:        "id1",
		IssuedAt:  issuedAt,
		ExpiresAt: expiresAt,
	}

	tokenBytes, err := Mint(private, token)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	verifyTime := time.Unix(issuedAt+1800, 0)

	// Correct audience.
	verified, err := VerifyForServiceAt(public, tokenBytes, "ticket", verifyTime)
	if err != nil {
		t.Fatalf("VerifyForServiceAt correct audience: %v", err)
	}
	if verified.Subject != subject {
		t.Errorf("Subject = %v, want %v", verified.Subject, subject)
	}

	// Wrong audience.
	_, err = VerifyForServiceAt(public, tokenBytes, "artifact", verifyTime)
	if !errors.Is(err, ErrAudienceMismatch) {
		t.Errorf("VerifyForServiceAt wrong audience: got %v, want ErrAudienceMismatch", err)
	}
}

func TestGrantsAllow(t *testing.T) {
	grants := []Grant{
		{Actions: []string{ticket.ActionCreate, "ticket/assign"}},
		{Actions: []string{ticket.ActionClose}, Targets: []string{"iree/**"}},
	}

	tests := []struct {
		action string
		target string
		want   bool
	}{
		{ticket.ActionCreate, "", true},
		{"ticket/assign", "", true},
		{ticket.ActionClose, "iree/amdgpu/pm", true},
		{ticket.ActionClose, "bureau/dev/coder", false},
		{ticket.ActionClose, "", true}, // self-service check on targeted grant
		{fleet.ActionAssign, "", false},
		{observation.ActionObserve, "", false},
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
		{Actions: []string{schema.ActionCommandAll}},
		{Actions: []string{observation.ActionAll}, Targets: []string{"**"}},
	}

	tests := []struct {
		action string
		target string
		want   bool
	}{
		{"command/ticket/create", "", true},
		{"command/artifact/fetch", "", true},
		{observation.ActionReadWrite, "any/principal", true},
		{observation.ActionObserve, "any/principal", true},
		{schema.ActionInterrupt, "", false},
	}

	for _, tt := range tests {
		got := GrantsAllow(grants, tt.action, tt.target)
		if got != tt.want {
			t.Errorf("GrantsAllow(%q, %q) = %v, want %v", tt.action, tt.target, got, tt.want)
		}
	}
}

func TestGrantsAllow_EmptyGrants(t *testing.T) {
	if GrantsAllow(nil, ticket.ActionCreate, "") {
		t.Error("nil grants should deny")
	}
	if GrantsAllow([]Grant{}, ticket.ActionCreate, "") {
		t.Error("empty grants should deny")
	}
}

func TestMintVerify_NoGrants(t *testing.T) {
	public, private := testKeypair(t)

	const issuedAt int64 = 1735689600  // 2025-01-01 00:00:00 UTC
	const expiresAt int64 = 1735693200 // 2025-01-01 01:00:00 UTC
	token := &Token{
		Subject:   mustParseUserID(t, "@bureau/fleet/prod/agent/tester:bureau.local"),
		Machine:   mustParseMachine(t, "@bureau/fleet/prod/machine/box:bureau.local"),
		Audience:  "ticket",
		ID:        "id1",
		IssuedAt:  issuedAt,
		ExpiresAt: expiresAt,
	}

	tokenBytes, err := Mint(private, token)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	verifyTime := time.Unix(issuedAt+1800, 0)
	verified, err := VerifyAt(public, tokenBytes, verifyTime)
	if err != nil {
		t.Fatalf("VerifyAt: %v", err)
	}

	if len(verified.Grants) != 0 {
		t.Errorf("Grants = %v, want empty", verified.Grants)
	}
}

func TestTokenWireSize(t *testing.T) {
	_, private := testKeypair(t)

	// A typical token with a few grants. Full user IDs are longer
	// than the old localparts, so the wire size is slightly larger.
	token := &Token{
		Subject:  mustParseUserID(t, "@bureau/fleet/prod/agent/coder:bureau.local"),
		Machine:  mustParseMachine(t, "@bureau/fleet/prod/machine/workstation:bureau.local"),
		Audience: "ticket",
		Grants: []Grant{
			{Actions: []string{ticket.ActionCreate, "ticket/assign"}},
			{Actions: []string{ticket.ActionClose}, Targets: []string{"bureau/dev/workspace/**:bureau.local"}},
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
