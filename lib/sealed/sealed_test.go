// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package sealed

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/secret"
)

func TestGenerateKeypair(t *testing.T) {
	keypair, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair() error: %v", err)
	}
	defer keypair.Close()

	privateKeyString := keypair.PrivateKey.String()
	if !strings.HasPrefix(privateKeyString, "AGE-SECRET-KEY-1") {
		t.Errorf("PrivateKey = %q, want prefix AGE-SECRET-KEY-1", privateKeyString)
	}
	if !strings.HasPrefix(keypair.PublicKey, "age1") {
		t.Errorf("PublicKey = %q, want prefix age1", keypair.PublicKey)
	}

	if keypair.PrivateKey.Len() < 20 {
		t.Errorf("PrivateKey too short: %d chars", keypair.PrivateKey.Len())
	}
	if len(keypair.PublicKey) < 20 {
		t.Errorf("PublicKey too short: %d chars", len(keypair.PublicKey))
	}
}

func TestGenerateKeypair_Unique(t *testing.T) {
	keypair1, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair() error: %v", err)
	}
	defer keypair1.Close()

	keypair2, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair() error: %v", err)
	}
	defer keypair2.Close()

	if keypair1.PrivateKey.String() == keypair2.PrivateKey.String() {
		t.Error("two generated keypairs have identical private keys")
	}
	if keypair1.PublicKey == keypair2.PublicKey {
		t.Error("two generated keypairs have identical public keys")
	}
}

func TestEncryptDecrypt_SingleRecipient(t *testing.T) {
	keypair, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair() error: %v", err)
	}
	defer keypair.Close()

	plaintext := []byte("hello, bureau credentials")
	ciphertext, err := Encrypt(plaintext, []string{keypair.PublicKey})
	if err != nil {
		t.Fatalf("Encrypt() error: %v", err)
	}

	// Ciphertext should be valid base64.
	if _, err := base64.StdEncoding.DecodeString(ciphertext); err != nil {
		t.Errorf("Encrypt() returned invalid base64: %v", err)
	}

	// Ciphertext should be different from plaintext.
	if ciphertext == string(plaintext) {
		t.Error("ciphertext equals plaintext")
	}

	// Decrypt should recover the original plaintext.
	decrypted, err := Decrypt(ciphertext, keypair.PrivateKey)
	if err != nil {
		t.Fatalf("Decrypt() error: %v", err)
	}
	defer decrypted.Close()

	if decrypted.String() != string(plaintext) {
		t.Errorf("Decrypt() = %q, want %q", decrypted.String(), plaintext)
	}
}

func TestEncryptDecrypt_MultipleRecipients(t *testing.T) {
	// Generate two keypairs (simulating machine key + operator escrow).
	machine, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair() error: %v", err)
	}
	defer machine.Close()

	operator, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair() error: %v", err)
	}
	defer operator.Close()

	plaintext := []byte(`{"OPENAI_API_KEY":"sk-test","ANTHROPIC_API_KEY":"sk-ant-test"}`)
	ciphertext, err := Encrypt(plaintext, []string{machine.PublicKey, operator.PublicKey})
	if err != nil {
		t.Fatalf("Encrypt() error: %v", err)
	}

	// Both recipients should be able to decrypt independently.
	decryptedByMachine, err := Decrypt(ciphertext, machine.PrivateKey)
	if err != nil {
		t.Fatalf("Decrypt(machine) error: %v", err)
	}
	defer decryptedByMachine.Close()

	if decryptedByMachine.String() != string(plaintext) {
		t.Errorf("Decrypt(machine) = %q, want %q", decryptedByMachine.String(), plaintext)
	}

	decryptedByOperator, err := Decrypt(ciphertext, operator.PrivateKey)
	if err != nil {
		t.Fatalf("Decrypt(operator) error: %v", err)
	}
	defer decryptedByOperator.Close()

	if decryptedByOperator.String() != string(plaintext) {
		t.Errorf("Decrypt(operator) = %q, want %q", decryptedByOperator.String(), plaintext)
	}
}

func TestDecrypt_WrongKey(t *testing.T) {
	keypair, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair() error: %v", err)
	}
	defer keypair.Close()

	wrongKeypair, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair() error: %v", err)
	}
	defer wrongKeypair.Close()

	plaintext := []byte("secret data")
	ciphertext, err := Encrypt(plaintext, []string{keypair.PublicKey})
	if err != nil {
		t.Fatalf("Encrypt() error: %v", err)
	}

	// Decrypting with the wrong key should fail.
	_, err = Decrypt(ciphertext, wrongKeypair.PrivateKey)
	if err == nil {
		t.Error("Decrypt() with wrong key should return error")
	}
}

func TestEncrypt_NoRecipients(t *testing.T) {
	_, err := Encrypt([]byte("data"), nil)
	if err == nil {
		t.Error("Encrypt() with no recipients should return error")
	}
	if !strings.Contains(err.Error(), "at least one recipient") {
		t.Errorf("error = %v, want 'at least one recipient'", err)
	}

	_, err = Encrypt([]byte("data"), []string{})
	if err == nil {
		t.Error("Encrypt() with empty recipients should return error")
	}
}

func TestEncrypt_InvalidRecipientKey(t *testing.T) {
	_, err := Encrypt([]byte("data"), []string{"not-a-valid-key"})
	if err == nil {
		t.Error("Encrypt() with invalid recipient key should return error")
	}
	if !strings.Contains(err.Error(), "parsing recipient key") {
		t.Errorf("error = %v, want 'parsing recipient key'", err)
	}
}

func TestDecrypt_InvalidPrivateKey(t *testing.T) {
	// Generate valid ciphertext first.
	keypair, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair() error: %v", err)
	}
	defer keypair.Close()

	ciphertext, err := Encrypt([]byte("data"), []string{keypair.PublicKey})
	if err != nil {
		t.Fatalf("Encrypt() error: %v", err)
	}

	invalidKey, err := secret.NewFromBytes([]byte("not-a-valid-private-key"))
	if err != nil {
		t.Fatalf("NewFromBytes failed: %v", err)
	}
	defer invalidKey.Close()

	_, err = Decrypt(ciphertext, invalidKey)
	if err == nil {
		t.Error("Decrypt() with invalid private key should return error")
	}
	if !strings.Contains(err.Error(), "parsing private key") {
		t.Errorf("error = %v, want 'parsing private key'", err)
	}
}

func TestDecrypt_InvalidBase64(t *testing.T) {
	keypair, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair() error: %v", err)
	}
	defer keypair.Close()

	_, err = Decrypt("not-valid-base64!!!", keypair.PrivateKey)
	if err == nil {
		t.Error("Decrypt() with invalid base64 should return error")
	}
	if !strings.Contains(err.Error(), "decoding base64") {
		t.Errorf("error = %v, want 'decoding base64'", err)
	}
}

func TestDecrypt_CorruptedCiphertext(t *testing.T) {
	keypair, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair() error: %v", err)
	}
	defer keypair.Close()

	// Valid base64 but not valid age ciphertext.
	corruptedBase64 := base64.StdEncoding.EncodeToString([]byte("this is not age ciphertext"))

	_, err = Decrypt(corruptedBase64, keypair.PrivateKey)
	if err == nil {
		t.Error("Decrypt() with corrupted ciphertext should return error")
	}
}

func TestEncryptDecrypt_EmptyPlaintext(t *testing.T) {
	keypair, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair() error: %v", err)
	}
	defer keypair.Close()

	ciphertext, err := Encrypt([]byte{}, []string{keypair.PublicKey})
	if err != nil {
		t.Fatalf("Encrypt(empty) error: %v", err)
	}

	decrypted, err := Decrypt(ciphertext, keypair.PrivateKey)
	if err != nil {
		t.Fatalf("Decrypt(empty) error: %v", err)
	}
	defer decrypted.Close()

	// Empty plaintext gets a 1-byte zero-filled buffer.
	if decrypted.Len() != 1 {
		t.Errorf("Decrypt(empty) length = %d, want 1", decrypted.Len())
	}
}

func TestEncryptDecrypt_LargePlaintext(t *testing.T) {
	keypair, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair() error: %v", err)
	}
	defer keypair.Close()

	// Simulate a large credential bundle (many API keys).
	largePlaintext := make([]byte, 64*1024)
	for i := range largePlaintext {
		largePlaintext[i] = byte(i % 256)
	}

	ciphertext, err := Encrypt(largePlaintext, []string{keypair.PublicKey})
	if err != nil {
		t.Fatalf("Encrypt(large) error: %v", err)
	}

	decrypted, err := Decrypt(ciphertext, keypair.PrivateKey)
	if err != nil {
		t.Fatalf("Decrypt(large) error: %v", err)
	}
	defer decrypted.Close()

	decryptedBytes := decrypted.Bytes()
	if len(decryptedBytes) != len(largePlaintext) {
		t.Fatalf("Decrypt(large) length = %d, want %d", len(decryptedBytes), len(largePlaintext))
	}
	for i := range largePlaintext {
		if decryptedBytes[i] != largePlaintext[i] {
			t.Errorf("Decrypt(large) byte %d = %d, want %d", i, decryptedBytes[i], largePlaintext[i])
			break
		}
	}
}

func TestEncryptJSON_DecryptJSON_RoundTrip(t *testing.T) {
	// Simulate the full credential bundle lifecycle: marshal JSON,
	// encrypt to machine + operator, decrypt on machine, unmarshal.
	machine, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair() error: %v", err)
	}
	defer machine.Close()

	operator, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair() error: %v", err)
	}
	defer operator.Close()

	credentials := map[string]string{
		"OPENAI_API_KEY":    "sk-test-key-12345",
		"ANTHROPIC_API_KEY": "sk-ant-test-key-67890",
		"GITHUB_PAT":        "ghp_test_token",
	}

	jsonPayload, err := json.Marshal(credentials)
	if err != nil {
		t.Fatalf("json.Marshal() error: %v", err)
	}

	ciphertext, err := EncryptJSON(jsonPayload, []string{machine.PublicKey, operator.PublicKey})
	if err != nil {
		t.Fatalf("EncryptJSON() error: %v", err)
	}

	// Machine decrypts.
	decryptedJSON, err := DecryptJSON(ciphertext, machine.PrivateKey)
	if err != nil {
		t.Fatalf("DecryptJSON() error: %v", err)
	}
	defer decryptedJSON.Close()

	var decryptedCredentials map[string]string
	if err := json.Unmarshal(decryptedJSON.Bytes(), &decryptedCredentials); err != nil {
		t.Fatalf("json.Unmarshal() error: %v", err)
	}

	for key, wantValue := range credentials {
		gotValue, exists := decryptedCredentials[key]
		if !exists {
			t.Errorf("decrypted credentials missing key %q", key)
			continue
		}
		if gotValue != wantValue {
			t.Errorf("decrypted credentials[%q] = %q, want %q", key, gotValue, wantValue)
		}
	}
	if len(decryptedCredentials) != len(credentials) {
		t.Errorf("decrypted credentials has %d keys, want %d", len(decryptedCredentials), len(credentials))
	}
}

func TestParsePublicKey(t *testing.T) {
	keypair, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair() error: %v", err)
	}
	defer keypair.Close()

	if err := ParsePublicKey(keypair.PublicKey); err != nil {
		t.Errorf("ParsePublicKey(valid) error: %v", err)
	}

	if err := ParsePublicKey("not-a-valid-key"); err == nil {
		t.Error("ParsePublicKey(invalid) should return error")
	}

	if err := ParsePublicKey(""); err == nil {
		t.Error("ParsePublicKey(empty) should return error")
	}
}

func TestParsePrivateKey(t *testing.T) {
	keypair, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair() error: %v", err)
	}
	defer keypair.Close()

	if err := ParsePrivateKey(keypair.PrivateKey); err != nil {
		t.Errorf("ParsePrivateKey(valid) error: %v", err)
	}

	invalidKey, err := secret.NewFromBytes([]byte("not-a-valid-key"))
	if err != nil {
		t.Fatalf("NewFromBytes failed: %v", err)
	}
	defer invalidKey.Close()

	if err := ParsePrivateKey(invalidKey); err == nil {
		t.Error("ParsePrivateKey(invalid) should return error")
	}

	emptyKey, err := secret.NewFromBytes([]byte(" "))
	if err != nil {
		t.Fatalf("NewFromBytes failed: %v", err)
	}
	defer emptyKey.Close()

	if err := ParsePrivateKey(emptyKey); err == nil {
		t.Error("ParsePrivateKey(empty) should return error")
	}
}

func TestEncryptDecrypt_DeterministicRecovery(t *testing.T) {
	// Verify that a serialized keypair can be used to decrypt later.
	// This simulates the launcher storing the private key in the keyring
	// and using it across process restarts.
	keypair, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair() error: %v", err)
	}
	defer keypair.Close()

	plaintext := []byte("persistent secret")
	ciphertext, err := Encrypt(plaintext, []string{keypair.PublicKey})
	if err != nil {
		t.Fatalf("Encrypt() error: %v", err)
	}

	// Simulate serializing and deserializing the private key (e.g., stored
	// in kernel keyring, retrieved later).
	savedPrivateKey, err := secret.NewFromBytes([]byte(keypair.PrivateKey.String()))
	if err != nil {
		t.Fatalf("NewFromBytes failed: %v", err)
	}
	defer savedPrivateKey.Close()

	if err := ParsePrivateKey(savedPrivateKey); err != nil {
		t.Fatalf("saved private key is invalid: %v", err)
	}

	decrypted, err := Decrypt(ciphertext, savedPrivateKey)
	if err != nil {
		t.Fatalf("Decrypt() with saved key error: %v", err)
	}
	defer decrypted.Close()

	if decrypted.String() != string(plaintext) {
		t.Errorf("Decrypt() = %q, want %q", decrypted.String(), plaintext)
	}
}

func TestKeypair_Close_Idempotent(t *testing.T) {
	keypair, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair() error: %v", err)
	}

	if err := keypair.Close(); err != nil {
		t.Fatalf("first Close failed: %v", err)
	}
	if err := keypair.Close(); err != nil {
		t.Fatalf("second Close failed: %v", err)
	}
}
