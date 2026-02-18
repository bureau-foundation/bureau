// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package servicetoken

import (
	"crypto/ed25519"
	"errors"
	"testing"
)

func TestSignVerifyPushTargetsConfig(t *testing.T) {
	public, private, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	config := &PushTargetsConfig{
		Targets: map[string]PushTarget{
			"machine/gpu-server-1": {
				SocketPath: "/run/bureau/tunnel/push-machine-gpu-server-1.sock",
			},
			"machine/workstation": {
				SocketPath: "/run/bureau/service/artifact/main.sock",
				Token:      []byte("test-token-bytes"),
			},
		},
		IssuedAt: 1735689500,
	}

	signed, err := SignPushTargetsConfig(private, config)
	if err != nil {
		t.Fatalf("SignPushTargetsConfig: %v", err)
	}

	decoded, err := VerifyPushTargetsConfig(public, signed)
	if err != nil {
		t.Fatalf("VerifyPushTargetsConfig: %v", err)
	}

	if len(decoded.Targets) != 2 {
		t.Fatalf("decoded.Targets has %d entries, want 2", len(decoded.Targets))
	}

	gpu := decoded.Targets["machine/gpu-server-1"]
	if gpu.SocketPath != "/run/bureau/tunnel/push-machine-gpu-server-1.sock" {
		t.Errorf("gpu SocketPath = %q, want %q", gpu.SocketPath, "/run/bureau/tunnel/push-machine-gpu-server-1.sock")
	}
	if gpu.Token != nil {
		t.Errorf("gpu Token = %v, want nil", gpu.Token)
	}

	workstation := decoded.Targets["machine/workstation"]
	if workstation.SocketPath != "/run/bureau/service/artifact/main.sock" {
		t.Errorf("workstation SocketPath = %q, want %q", workstation.SocketPath, "/run/bureau/service/artifact/main.sock")
	}
	if string(workstation.Token) != "test-token-bytes" {
		t.Errorf("workstation Token = %q, want %q", string(workstation.Token), "test-token-bytes")
	}

	if decoded.IssuedAt != 1735689500 {
		t.Errorf("IssuedAt = %d, want 1735689500", decoded.IssuedAt)
	}
}

func TestSignVerifyPushTargetsConfig_EmptyTargets(t *testing.T) {
	public, private, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	config := &PushTargetsConfig{
		Targets:  map[string]PushTarget{},
		IssuedAt: 1735689500,
	}

	signed, err := SignPushTargetsConfig(private, config)
	if err != nil {
		t.Fatalf("SignPushTargetsConfig: %v", err)
	}

	decoded, err := VerifyPushTargetsConfig(public, signed)
	if err != nil {
		t.Fatalf("VerifyPushTargetsConfig: %v", err)
	}

	if len(decoded.Targets) != 0 {
		t.Errorf("decoded.Targets has %d entries, want 0", len(decoded.Targets))
	}
}

func TestVerifyPushTargetsConfig_BadSignature(t *testing.T) {
	_, signingKey, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	wrongPublic, _, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	config := &PushTargetsConfig{
		Targets: map[string]PushTarget{
			"machine/test": {SocketPath: "/some/path"},
		},
		IssuedAt: 1735689500,
	}

	signed, err := SignPushTargetsConfig(signingKey, config)
	if err != nil {
		t.Fatalf("SignPushTargetsConfig: %v", err)
	}

	_, err = VerifyPushTargetsConfig(wrongPublic, signed)
	if !errors.Is(err, ErrPushTargetsConfigBadSig) {
		t.Errorf("VerifyPushTargetsConfig with wrong key: got %v, want %v", err, ErrPushTargetsConfigBadSig)
	}
}

func TestVerifyPushTargetsConfig_TooShort(t *testing.T) {
	_, err := VerifyPushTargetsConfig(make(ed25519.PublicKey, ed25519.PublicKeySize), []byte("short"))
	if !errors.Is(err, ErrPushTargetsConfigTooShort) {
		t.Errorf("VerifyPushTargetsConfig with truncated data: got %v, want %v", err, ErrPushTargetsConfigTooShort)
	}
}

func TestVerifyPushTargetsConfig_TamperedPayload(t *testing.T) {
	public, private, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	config := &PushTargetsConfig{
		Targets: map[string]PushTarget{
			"machine/test": {SocketPath: "/some/path"},
		},
		IssuedAt: 1735689500,
	}

	signed, err := SignPushTargetsConfig(private, config)
	if err != nil {
		t.Fatalf("SignPushTargetsConfig: %v", err)
	}

	// Flip a byte in the payload region.
	signed[0] ^= 0xff

	_, err = VerifyPushTargetsConfig(public, signed)
	if !errors.Is(err, ErrPushTargetsConfigBadSig) {
		t.Errorf("VerifyPushTargetsConfig with tampered payload: got %v, want %v", err, ErrPushTargetsConfigBadSig)
	}
}
