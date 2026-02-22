// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package artifactstore

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"testing"

	"github.com/bureau-foundation/bureau/lib/secret"
)

// testMasterKey creates a deterministic 32-byte master key for tests.
// The key is derived from a fixed seed so tests are reproducible.
func testMasterKey(t *testing.T) *secret.Buffer {
	t.Helper()
	key := [KeySize]byte{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
		0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
		0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20,
	}
	buffer, err := secret.NewFromBytes(key[:])
	if err != nil {
		t.Fatal(err)
	}
	return buffer
}

// testMasterKeyAlternate creates a different deterministic master key
// for testing that different keys produce different outputs.
func testMasterKeyAlternate(t *testing.T) *secret.Buffer {
	t.Helper()
	key := [KeySize]byte{
		0xf0, 0xe1, 0xd2, 0xc3, 0xb4, 0xa5, 0x96, 0x87,
		0x78, 0x69, 0x5a, 0x4b, 0x3c, 0x2d, 0x1e, 0x0f,
		0x0f, 0x1e, 0x2d, 0x3c, 0x4b, 0x5a, 0x69, 0x78,
		0x87, 0x96, 0xa5, 0xb4, 0xc3, 0xd2, 0xe1, 0xf0,
	}
	buffer, err := secret.NewFromBytes(key[:])
	if err != nil {
		t.Fatal(err)
	}
	return buffer
}

// testFileHash returns a deterministic file hash for tests.
func testFileHash() Hash {
	return HashFile(HashChunk([]byte("test artifact content")))
}

// testFileHashAlternate returns a different file hash for tests.
func testFileHashAlternate() Hash {
	return HashFile(HashChunk([]byte("different artifact content")))
}

// testContainerHash returns a deterministic container hash for tests.
func testContainerHash() Hash {
	return HashContainer(HashChunk([]byte("test container content")))
}

// testContainerHashAlternate returns a different container hash.
func testContainerHashAlternate() Hash {
	return HashContainer(HashChunk([]byte("different container content")))
}

// --- Key derivation tests ---

func TestDerivePerArtifactKeyDeterministic(t *testing.T) {
	masterKey := testMasterKey(t)
	defer masterKey.Close()
	fileHash := testFileHash()

	key1, err := DerivePerArtifactKey(masterKey, fileHash)
	if err != nil {
		t.Fatal(err)
	}
	defer key1.Close()

	key2, err := DerivePerArtifactKey(masterKey, fileHash)
	if err != nil {
		t.Fatal(err)
	}
	defer key2.Close()

	if !key1.Equal(key2) {
		t.Error("same master key + same file hash should produce identical per-artifact keys")
	}
}

func TestDerivePerArtifactKeyVariesWithFileHash(t *testing.T) {
	masterKey := testMasterKey(t)
	defer masterKey.Close()

	key1, err := DerivePerArtifactKey(masterKey, testFileHash())
	if err != nil {
		t.Fatal(err)
	}
	defer key1.Close()

	key2, err := DerivePerArtifactKey(masterKey, testFileHashAlternate())
	if err != nil {
		t.Fatal(err)
	}
	defer key2.Close()

	if key1.Equal(key2) {
		t.Error("different file hashes should produce different per-artifact keys")
	}
}

func TestDerivePerArtifactKeyVariesWithMasterKey(t *testing.T) {
	masterKey1 := testMasterKey(t)
	defer masterKey1.Close()
	masterKey2 := testMasterKeyAlternate(t)
	defer masterKey2.Close()
	fileHash := testFileHash()

	key1, err := DerivePerArtifactKey(masterKey1, fileHash)
	if err != nil {
		t.Fatal(err)
	}
	defer key1.Close()

	key2, err := DerivePerArtifactKey(masterKey2, fileHash)
	if err != nil {
		t.Fatal(err)
	}
	defer key2.Close()

	if key1.Equal(key2) {
		t.Error("different master keys should produce different per-artifact keys")
	}
}

func TestDeriveReconEncryptionKeyDeterministic(t *testing.T) {
	masterKey := testMasterKey(t)
	defer masterKey.Close()

	perArtifactKey, err := DerivePerArtifactKey(masterKey, testFileHash())
	if err != nil {
		t.Fatal(err)
	}
	defer perArtifactKey.Close()

	key1, err := DeriveReconEncryptionKey(perArtifactKey)
	if err != nil {
		t.Fatal(err)
	}
	defer key1.Close()

	key2, err := DeriveReconEncryptionKey(perArtifactKey)
	if err != nil {
		t.Fatal(err)
	}
	defer key2.Close()

	if !key1.Equal(key2) {
		t.Error("same per-artifact key should produce identical recon encryption keys")
	}
}

func TestDeriveContainerEncryptionKeyDeterministic(t *testing.T) {
	masterKey := testMasterKey(t)
	defer masterKey.Close()
	containerHash := testContainerHash()

	key1, err := DeriveContainerEncryptionKey(masterKey, containerHash)
	if err != nil {
		t.Fatal(err)
	}
	defer key1.Close()

	key2, err := DeriveContainerEncryptionKey(masterKey, containerHash)
	if err != nil {
		t.Fatal(err)
	}
	defer key2.Close()

	if !key1.Equal(key2) {
		t.Error("same master key + same container hash should produce identical container encryption keys")
	}
}

func TestDeriveContainerEncryptionKeyVariesWithContainerHash(t *testing.T) {
	masterKey := testMasterKey(t)
	defer masterKey.Close()

	key1, err := DeriveContainerEncryptionKey(masterKey, testContainerHash())
	if err != nil {
		t.Fatal(err)
	}
	defer key1.Close()

	key2, err := DeriveContainerEncryptionKey(masterKey, testContainerHashAlternate())
	if err != nil {
		t.Fatal(err)
	}
	defer key2.Close()

	if key1.Equal(key2) {
		t.Error("different container hashes should produce different encryption keys")
	}
}

func TestKeyIsolation(t *testing.T) {
	masterKey := testMasterKey(t)
	defer masterKey.Close()

	fileHashA := testFileHash()
	fileHashB := testFileHashAlternate()
	plaintext := []byte("sensitive reconstruction record data")

	// Derive key for artifact A and encrypt.
	perArtifactKeyA, err := DerivePerArtifactKey(masterKey, fileHashA)
	if err != nil {
		t.Fatal(err)
	}
	defer perArtifactKeyA.Close()

	reconKeyA, err := DeriveReconEncryptionKey(perArtifactKeyA)
	if err != nil {
		t.Fatal(err)
	}
	defer reconKeyA.Close()

	encrypted, err := EncryptBlob(plaintext, reconKeyA, fileHashA)
	if err != nil {
		t.Fatal(err)
	}

	// Derive key for artifact B and attempt to decrypt A's ciphertext.
	perArtifactKeyB, err := DerivePerArtifactKey(masterKey, fileHashB)
	if err != nil {
		t.Fatal(err)
	}
	defer perArtifactKeyB.Close()

	reconKeyB, err := DeriveReconEncryptionKey(perArtifactKeyB)
	if err != nil {
		t.Fatal(err)
	}
	defer reconKeyB.Close()

	_, err = DecryptBlob(encrypted, reconKeyB, fileHashA)
	if err == nil {
		t.Error("decrypting with a key derived from a different file hash should fail")
	}
}

// --- Reference obscuring tests ---

func TestObscureReconReferenceDeterministic(t *testing.T) {
	masterKey := testMasterKey(t)
	defer masterKey.Close()
	fileHash := testFileHash()

	perArtifactKey, err := DerivePerArtifactKey(masterKey, fileHash)
	if err != nil {
		t.Fatal(err)
	}
	defer perArtifactKey.Close()

	ref1 := ObscureReconReference(perArtifactKey, fileHash)
	ref2 := ObscureReconReference(perArtifactKey, fileHash)

	if ref1 != ref2 {
		t.Error("same key + same hash should produce identical obscured recon refs")
	}
}

func TestObscureContainerReferenceDeterministic(t *testing.T) {
	masterKey := testMasterKey(t)
	defer masterKey.Close()
	containerHash := testContainerHash()

	ref1 := ObscureContainerReference(masterKey, containerHash)
	ref2 := ObscureContainerReference(masterKey, containerHash)

	if ref1 != ref2 {
		t.Error("same key + same hash should produce identical obscured container refs")
	}
}

func TestObscureReferenceVariesWithKey(t *testing.T) {
	masterKey1 := testMasterKey(t)
	defer masterKey1.Close()
	masterKey2 := testMasterKeyAlternate(t)
	defer masterKey2.Close()
	containerHash := testContainerHash()

	ref1 := ObscureContainerReference(masterKey1, containerHash)
	ref2 := ObscureContainerReference(masterKey2, containerHash)

	if ref1 == ref2 {
		t.Error("different keys should produce different obscured refs (deployment-specific)")
	}
}

func TestObscureReferenceNoCrossDomainCollision(t *testing.T) {
	masterKey := testMasterKey(t)
	defer masterKey.Close()

	// Use the same underlying hash value for both domains to test
	// that domain tags prevent collisions.
	sharedHash := testFileHash()

	perArtifactKey, err := DerivePerArtifactKey(masterKey, sharedHash)
	if err != nil {
		t.Fatal(err)
	}
	defer perArtifactKey.Close()

	reconRef := ObscureReconReference(perArtifactKey, sharedHash)
	containerRef := ObscureContainerReference(masterKey, sharedHash)

	if reconRef == containerRef {
		t.Error("recon ref and container ref should never collide, even for the same hash value")
	}
}

// --- AEAD encrypt/decrypt tests ---

func TestEncryptDecryptRoundTrip(t *testing.T) {
	masterKey := testMasterKey(t)
	defer masterKey.Close()

	encryptionKey, err := DeriveContainerEncryptionKey(masterKey, testContainerHash())
	if err != nil {
		t.Fatal(err)
	}
	defer encryptionKey.Close()

	identityHash := testContainerHash()

	sizes := []int{0, 1, 200, 64 * 1024, 1024 * 1024}
	for _, size := range sizes {
		t.Run(formatSize(size), func(t *testing.T) {
			plaintext := make([]byte, size)
			if size > 0 {
				if _, err := rand.Read(plaintext); err != nil {
					t.Fatal(err)
				}
			}

			encrypted, err := EncryptBlob(plaintext, encryptionKey, identityHash)
			if err != nil {
				t.Fatalf("EncryptBlob: %v", err)
			}

			decrypted, err := DecryptBlob(encrypted, encryptionKey, identityHash)
			if err != nil {
				t.Fatalf("DecryptBlob: %v", err)
			}

			if !bytes.Equal(decrypted, plaintext) {
				t.Errorf("decrypted content does not match original (size %d)", size)
			}
		})
	}
}

func TestEncryptBlobNonDeterministic(t *testing.T) {
	masterKey := testMasterKey(t)
	defer masterKey.Close()

	encryptionKey, err := DeriveContainerEncryptionKey(masterKey, testContainerHash())
	if err != nil {
		t.Fatal(err)
	}
	defer encryptionKey.Close()

	plaintext := []byte("identical content for both encryptions")
	identityHash := testContainerHash()

	encrypted1, err := EncryptBlob(plaintext, encryptionKey, identityHash)
	if err != nil {
		t.Fatal(err)
	}

	encrypted2, err := EncryptBlob(plaintext, encryptionKey, identityHash)
	if err != nil {
		t.Fatal(err)
	}

	if bytes.Equal(encrypted1, encrypted2) {
		t.Error("two encryptions of identical content should produce different output (random nonce)")
	}
}

func TestDecryptBlobWrongIdentityHash(t *testing.T) {
	masterKey := testMasterKey(t)
	defer masterKey.Close()

	encryptionKey, err := DeriveContainerEncryptionKey(masterKey, testContainerHash())
	if err != nil {
		t.Fatal(err)
	}
	defer encryptionKey.Close()

	plaintext := []byte("test data")
	hashA := testContainerHash()
	hashB := testContainerHashAlternate()

	encrypted, err := EncryptBlob(plaintext, encryptionKey, hashA)
	if err != nil {
		t.Fatal(err)
	}

	_, err = DecryptBlob(encrypted, encryptionKey, hashB)
	if err == nil {
		t.Error("decrypting with wrong identity hash should fail AEAD authentication")
	}
}

func TestDecryptBlobWrongKey(t *testing.T) {
	masterKey1 := testMasterKey(t)
	defer masterKey1.Close()
	masterKey2 := testMasterKeyAlternate(t)
	defer masterKey2.Close()

	key1, err := DeriveContainerEncryptionKey(masterKey1, testContainerHash())
	if err != nil {
		t.Fatal(err)
	}
	defer key1.Close()

	key2, err := DeriveContainerEncryptionKey(masterKey2, testContainerHash())
	if err != nil {
		t.Fatal(err)
	}
	defer key2.Close()

	plaintext := []byte("secret data")
	identityHash := testContainerHash()

	encrypted, err := EncryptBlob(plaintext, key1, identityHash)
	if err != nil {
		t.Fatal(err)
	}

	_, err = DecryptBlob(encrypted, key2, identityHash)
	if err == nil {
		t.Error("decrypting with wrong key should fail AEAD authentication")
	}
}

func TestDecryptBlobTruncated(t *testing.T) {
	masterKey := testMasterKey(t)
	defer masterKey.Close()

	encryptionKey, err := DeriveContainerEncryptionKey(masterKey, testContainerHash())
	if err != nil {
		t.Fatal(err)
	}
	defer encryptionKey.Close()

	identityHash := testContainerHash()

	// Try blobs shorter than the minimum size (41 bytes).
	for _, length := range []int{0, 1, 10, 40} {
		_, err := DecryptBlob(make([]byte, length), encryptionKey, identityHash)
		if err == nil {
			t.Errorf("blob of length %d should be rejected as too short", length)
		}
	}
}

func TestDecryptBlobWrongVersion(t *testing.T) {
	masterKey := testMasterKey(t)
	defer masterKey.Close()

	encryptionKey, err := DeriveContainerEncryptionKey(masterKey, testContainerHash())
	if err != nil {
		t.Fatal(err)
	}
	defer encryptionKey.Close()

	plaintext := []byte("test data")
	identityHash := testContainerHash()

	encrypted, err := EncryptBlob(plaintext, encryptionKey, identityHash)
	if err != nil {
		t.Fatal(err)
	}

	// Tamper with the version byte.
	tampered := make([]byte, len(encrypted))
	copy(tampered, encrypted)
	tampered[0] = 0x02

	_, err = DecryptBlob(tampered, encryptionKey, identityHash)
	if err == nil {
		t.Error("tampered version byte should cause decryption failure")
	}
}

func TestDecryptBlobTamperedCiphertext(t *testing.T) {
	masterKey := testMasterKey(t)
	defer masterKey.Close()

	encryptionKey, err := DeriveContainerEncryptionKey(masterKey, testContainerHash())
	if err != nil {
		t.Fatal(err)
	}
	defer encryptionKey.Close()

	plaintext := []byte("test data for tamper detection")
	identityHash := testContainerHash()

	encrypted, err := EncryptBlob(plaintext, encryptionKey, identityHash)
	if err != nil {
		t.Fatal(err)
	}

	// Flip a bit in the ciphertext portion (after version + nonce).
	tampered := make([]byte, len(encrypted))
	copy(tampered, encrypted)
	ciphertextOffset := 1 + 24 // version + nonce
	tampered[ciphertextOffset] ^= 0x01

	_, err = DecryptBlob(tampered, encryptionKey, identityHash)
	if err == nil {
		t.Error("tampered ciphertext should cause AEAD authentication failure")
	}
}

func TestEncryptBlobFormat(t *testing.T) {
	masterKey := testMasterKey(t)
	defer masterKey.Close()

	encryptionKey, err := DeriveContainerEncryptionKey(masterKey, testContainerHash())
	if err != nil {
		t.Fatal(err)
	}
	defer encryptionKey.Close()

	plaintext := []byte("format verification test data")
	identityHash := testContainerHash()

	encrypted, err := EncryptBlob(plaintext, encryptionKey, identityHash)
	if err != nil {
		t.Fatal(err)
	}

	// Version byte.
	if encrypted[0] != EncryptedBlobVersion {
		t.Errorf("first byte = 0x%02x, want 0x%02x", encrypted[0], EncryptedBlobVersion)
	}

	// Nonce (24 bytes after version). With overwhelming probability,
	// a random nonce is not all zeros.
	nonce := encrypted[1:25]
	allZero := true
	for _, b := range nonce {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Error("nonce is all zeros, which is astronomically unlikely for a random nonce")
	}

	// Total length: 1 + 24 + len(plaintext) + 16.
	expectedLength := EncryptedBlobOverhead + len(plaintext)
	if len(encrypted) != expectedLength {
		t.Errorf("encrypted blob length = %d, want %d (1 version + 24 nonce + %d plaintext + 16 tag)",
			len(encrypted), expectedLength, len(plaintext))
	}
}

// --- EncryptionKeySet integration tests ---

func TestEncryptionKeySetContainerRoundTrip(t *testing.T) {
	masterKey := testMasterKey(t)
	keySet, err := NewEncryptionKeySet(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer keySet.Close()

	containerHash := testContainerHash()
	containerBytes := []byte("simulated container data with chunks and index headers")

	encrypted, err := keySet.EncryptContainer(containerBytes, containerHash)
	if err != nil {
		t.Fatalf("EncryptContainer: %v", err)
	}

	decrypted, err := keySet.DecryptContainer(encrypted, containerHash)
	if err != nil {
		t.Fatalf("DecryptContainer: %v", err)
	}

	if !bytes.Equal(decrypted, containerBytes) {
		t.Error("decrypted container does not match original")
	}
}

func TestEncryptionKeySetReconstructionRoundTrip(t *testing.T) {
	masterKey := testMasterKey(t)
	keySet, err := NewEncryptionKeySet(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer keySet.Close()

	fileHash := testFileHash()

	// Build a realistic reconstruction record.
	record := &ReconstructionRecord{
		Version:    ReconstructionRecordVersion,
		FileHash:   fileHash,
		Size:       65536,
		ChunkCount: 2,
		Segments: []Segment{
			{Container: testContainerHash(), StartIndex: 0, ChunkCount: 1},
			{Container: testContainerHashAlternate(), StartIndex: 0, ChunkCount: 1},
		},
	}
	recordBytes, err := MarshalReconstruction(record)
	if err != nil {
		t.Fatal(err)
	}

	encrypted, err := keySet.EncryptReconstruction(recordBytes, fileHash)
	if err != nil {
		t.Fatalf("EncryptReconstruction: %v", err)
	}

	decrypted, err := keySet.DecryptReconstruction(encrypted, fileHash)
	if err != nil {
		t.Fatalf("DecryptReconstruction: %v", err)
	}

	// Unmarshal and verify the record survived the round trip.
	loaded, err := UnmarshalReconstruction(decrypted)
	if err != nil {
		t.Fatalf("UnmarshalReconstruction: %v", err)
	}

	if loaded.FileHash != record.FileHash {
		t.Errorf("FileHash mismatch after round trip")
	}
	if loaded.Size != record.Size {
		t.Errorf("Size = %d, want %d", loaded.Size, record.Size)
	}
	if loaded.ChunkCount != record.ChunkCount {
		t.Errorf("ChunkCount = %d, want %d", loaded.ChunkCount, record.ChunkCount)
	}
	if len(loaded.Segments) != len(record.Segments) {
		t.Fatalf("Segments length = %d, want %d", len(loaded.Segments), len(record.Segments))
	}
	for i, segment := range loaded.Segments {
		if segment.Container != record.Segments[i].Container {
			t.Errorf("Segments[%d].Container mismatch", i)
		}
	}
}

func TestEncryptionKeySetObscuredRefs(t *testing.T) {
	masterKey := testMasterKey(t)
	keySet, err := NewEncryptionKeySet(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer keySet.Close()

	// Container refs are deterministic.
	containerHash := testContainerHash()
	containerRef1 := keySet.ObscuredContainerRef(containerHash)
	containerRef2 := keySet.ObscuredContainerRef(containerHash)
	if containerRef1 != containerRef2 {
		t.Error("ObscuredContainerRef should be deterministic")
	}

	// Recon refs are deterministic.
	fileHash := testFileHash()
	reconRef1 := keySet.ObscuredReconRef(fileHash)
	reconRef2 := keySet.ObscuredReconRef(fileHash)
	if reconRef1 != reconRef2 {
		t.Error("ObscuredReconRef should be deterministic")
	}

	// Recon and container refs should differ even for the same hash
	// input (different domain tags and different key derivation paths).
	if reconRef1 == containerRef1 {
		t.Error("recon ref and container ref should differ due to domain separation")
	}
}

func TestEncryptionKeySetRejectsWrongKeySize(t *testing.T) {
	shortKey := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	buffer, err := secret.NewFromBytes(shortKey[:])
	if err != nil {
		t.Fatal(err)
	}

	_, err = NewEncryptionKeySet(buffer)
	if err == nil {
		t.Error("NewEncryptionKeySet should reject a key that is not 32 bytes")
	}
	// buffer was not taken by NewEncryptionKeySet, so close it.
	buffer.Close()
}

// --- Benchmarks ---

func BenchmarkEncryptBlob(b *testing.B) {
	keyBytes := [KeySize]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16,
		17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32}
	key, err := secret.NewFromBytes(keyBytes[:])
	if err != nil {
		b.Fatal(err)
	}
	defer key.Close()

	var identityHash Hash
	identityHash[0] = 0xAA

	sizes := []int{1024, 64 * 1024, 1024 * 1024, 64 * 1024 * 1024}
	for _, size := range sizes {
		plaintext := make([]byte, size)
		b.Run(formatSize(size), func(b *testing.B) {
			b.SetBytes(int64(size))
			for i := 0; i < b.N; i++ {
				_, err := EncryptBlob(plaintext, key, identityHash)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkDerivePerArtifactKey(b *testing.B) {
	keyBytes := [KeySize]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16,
		17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32}
	masterKey, err := secret.NewFromBytes(keyBytes[:])
	if err != nil {
		b.Fatal(err)
	}
	defer masterKey.Close()

	fileHash := testFileHash()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		derived, err := DerivePerArtifactKey(masterKey, fileHash)
		if err != nil {
			b.Fatal(err)
		}
		derived.Close()
	}
}

func BenchmarkObscureReference(b *testing.B) {
	keyBytes := [KeySize]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16,
		17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32}
	masterKey, err := secret.NewFromBytes(keyBytes[:])
	if err != nil {
		b.Fatal(err)
	}
	defer masterKey.Close()

	containerHash := testContainerHash()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ObscureContainerReference(masterKey, containerHash)
	}
}

// formatSize returns a human-readable size string for test/benchmark names.
func formatSize(size int) string {
	switch {
	case size == 0:
		return "0B"
	case size < 1024:
		return fmt.Sprintf("%dB", size)
	case size < 1024*1024:
		return fmt.Sprintf("%dKB", size/1024)
	default:
		return fmt.Sprintf("%dMB", size/(1024*1024))
	}
}
