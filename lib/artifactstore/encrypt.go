// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package artifactstore

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"

	"github.com/zeebo/blake3"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"

	"github.com/bureau-foundation/bureau/lib/secret"
)

// KeySize is the size in bytes of all symmetric keys in the artifact
// encryption system: the artifact encryption key (received from the
// launcher), per-artifact keys, and per-container encryption keys.
const KeySize = 32

// EncryptedBlobVersion is the version byte prepended to all encrypted
// blobs. Included as additional authenticated data (AAD) in the AEAD
// Seal/Open call, so tampering with the version byte causes
// authentication failure.
const EncryptedBlobVersion byte = 0x01

// EncryptedBlobOverhead is the total byte overhead per encrypted blob:
// 1 (version) + 24 (XChaCha20-Poly1305 nonce) + 16 (Poly1305 tag).
// For a ~64 MiB container this is negligible; for a ~200 byte
// reconstruction record it is roughly 20%.
const EncryptedBlobOverhead = 1 + chacha20poly1305.NonceSizeX + chacha20poly1305.Overhead

// HKDF info strings. These are the "info" parameter to HKDF-SHA256,
// providing domain separation between different key derivation paths.
// Changing any of these invalidates all ciphertext encrypted under
// that derivation path.
var (
	hkdfInfoPerArtifact         = []byte("bureau.artifact.v1")
	hkdfInfoReconEncryption     = []byte("bureau.artifact.recon.enc.v1")
	hkdfInfoContainerEncryption = []byte("bureau.artifact.container.enc.v1")
)

// BLAKE3-keyed reference obscuring domain tags. These are the data
// prefix for BLAKE3 keyed hashing when computing obscured references
// for external storage. Domain tags prevent reconstruction record
// refs from ever colliding with container refs.
var (
	referenceDomainRecon     = []byte("bureau.artifact.ref.recon.v1")
	referenceDomainContainer = []byte("bureau.artifact.ref.container.v1")
)

// DerivePerArtifactKey derives the per-artifact key from the artifact
// encryption key and a file hash. The per-artifact key is the root
// of a two-key tree: one key encrypts the reconstruction record,
// and the other obscures the reconstruction record's external storage
// reference.
//
// The masterKey is borrowed (read via .Bytes()) and is NOT closed by
// this function. The returned Buffer must be closed by the caller.
func DerivePerArtifactKey(masterKey *secret.Buffer, fileHash Hash) (*secret.Buffer, error) {
	info := make([]byte, len(hkdfInfoPerArtifact)+len(fileHash))
	copy(info, hkdfInfoPerArtifact)
	copy(info[len(hkdfInfoPerArtifact):], fileHash[:])
	return deriveKey(masterKey.Bytes(), info)
}

// DeriveReconEncryptionKey derives the encryption key for a
// reconstruction record from the per-artifact key. Each artifact's
// reconstruction record is encrypted with a unique key.
//
// The perArtifactKey is borrowed and NOT closed. The returned Buffer
// must be closed by the caller.
func DeriveReconEncryptionKey(perArtifactKey *secret.Buffer) (*secret.Buffer, error) {
	return deriveKey(perArtifactKey.Bytes(), hkdfInfoReconEncryption)
}

// DeriveContainerEncryptionKey derives the encryption key for a
// container from the artifact encryption key and the container hash.
// The same container always derives the same key regardless of which
// artifact references it, preserving deduplication in external
// storage.
//
// The masterKey is borrowed and NOT closed. The returned Buffer must
// be closed by the caller.
func DeriveContainerEncryptionKey(masterKey *secret.Buffer, containerHash Hash) (*secret.Buffer, error) {
	info := make([]byte, len(hkdfInfoContainerEncryption)+len(containerHash))
	copy(info, hkdfInfoContainerEncryption)
	copy(info[len(hkdfInfoContainerEncryption):], containerHash[:])
	return deriveKey(masterKey.Bytes(), info)
}

// ObscureReconReference computes the opaque external storage key for
// an encrypted reconstruction record. The reference is deterministic
// (same key + hash = same ref), opaque without the key, and
// deployment-specific.
//
// The perArtifactKey is borrowed and NOT closed.
func ObscureReconReference(perArtifactKey *secret.Buffer, fileHash Hash) Hash {
	return obscureReference(perArtifactKey.Bytes(), referenceDomainRecon, fileHash)
}

// ObscureContainerReference computes the opaque external storage key
// for an encrypted container. Deterministic for dedup checking in
// external storage: the same container always produces the same
// obscured reference when encrypted under the same deployment key.
//
// The masterKey is borrowed and NOT closed.
func ObscureContainerReference(masterKey *secret.Buffer, containerHash Hash) Hash {
	return obscureReference(masterKey.Bytes(), referenceDomainContainer, containerHash)
}

// EncryptBlob encrypts plaintext using XChaCha20-Poly1305 and returns
// the encrypted blob in the standard format:
//
//	[Version: 1 byte (0x01)] [Nonce: 24 bytes (random)] [Ciphertext+Tag: N+16 bytes]
//
// The version byte and identityHash are included as additional
// authenticated data (AAD). The version byte authenticates the format
// version. The identityHash binds the ciphertext to the artifact or
// container it belongs to, preventing blob swapping in external
// storage.
//
// The encryptionKey is borrowed and NOT closed. It must be exactly 32
// bytes (the output of any Derive*Key function).
func EncryptBlob(plaintext []byte, encryptionKey *secret.Buffer, identityHash Hash) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(encryptionKey.Bytes())
	if err != nil {
		return nil, fmt.Errorf("creating XChaCha20-Poly1305 cipher: %w", err)
	}

	// Generate a random 24-byte nonce.
	var nonce [chacha20poly1305.NonceSizeX]byte
	if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
		return nil, fmt.Errorf("generating random nonce: %w", err)
	}

	aad := buildAAD(EncryptedBlobVersion, identityHash)

	// Allocate output: version + nonce + ciphertext + tag.
	output := make([]byte, 1+chacha20poly1305.NonceSizeX, 1+chacha20poly1305.NonceSizeX+len(plaintext)+aead.Overhead())
	output[0] = EncryptedBlobVersion
	copy(output[1:], nonce[:])

	// Seal appends the ciphertext+tag to output.
	output = aead.Seal(output, nonce[:], plaintext, aad)
	return output, nil
}

// DecryptBlob decrypts an encrypted blob produced by EncryptBlob.
// It verifies the version byte, extracts the nonce, and authenticates
// the ciphertext against the AAD (version byte + identityHash).
//
// Returns an error if:
//   - The blob is too short to contain version + nonce + tag
//   - The version byte is not EncryptedBlobVersion
//   - AEAD authentication fails (wrong key, tampered ciphertext,
//     wrong identity hash)
//
// The encryptionKey is borrowed and NOT closed.
func DecryptBlob(encryptedBlob []byte, encryptionKey *secret.Buffer, identityHash Hash) ([]byte, error) {
	if len(encryptedBlob) < EncryptedBlobOverhead {
		return nil, fmt.Errorf("encrypted blob is %d bytes, minimum is %d (version + nonce + tag)",
			len(encryptedBlob), EncryptedBlobOverhead)
	}

	version := encryptedBlob[0]
	if version != EncryptedBlobVersion {
		return nil, fmt.Errorf("encrypted blob version %d is not supported (expected %d)",
			version, EncryptedBlobVersion)
	}

	nonce := encryptedBlob[1 : 1+chacha20poly1305.NonceSizeX]
	ciphertext := encryptedBlob[1+chacha20poly1305.NonceSizeX:]

	aead, err := chacha20poly1305.NewX(encryptionKey.Bytes())
	if err != nil {
		return nil, fmt.Errorf("creating XChaCha20-Poly1305 cipher: %w", err)
	}

	aad := buildAAD(version, identityHash)

	plaintext, err := aead.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("AEAD decryption failed (wrong key, tampered data, or mismatched identity): %w", err)
	}

	return plaintext, nil
}

// EncryptionKeySet holds the artifact encryption key in guarded
// memory and provides methods to derive per-artifact and per-container
// keys for encryption and reference obscuring.
//
// The artifact encryption key is the root of all key derivation in
// the artifact encryption system. It is derived by the launcher from
// the deployment master key via HKDF, then delivered to the artifact
// service via stdin. The service stores it in a secret.Buffer
// (mmap-backed, mlock'd, excluded from core dumps, zeroed on close).
//
// EncryptionKeySet does not cache derived keys. Each method call
// performs a fresh HKDF derivation. HKDF-SHA256 derivation takes
// roughly 1 microsecond — negligible compared to the AEAD encryption
// and network I/O that follows.
//
// Close zeroes and releases the master key. After Close, all methods
// panic (via secret.Buffer's closed check).
type EncryptionKeySet struct {
	masterKey *secret.Buffer
}

// NewEncryptionKeySet creates a key set from an artifact encryption
// key. The masterKey buffer is owned by the EncryptionKeySet and will
// be closed when Close is called. The caller must not use masterKey
// after passing it to this function.
//
// Returns an error if masterKey is not exactly KeySize (32) bytes.
func NewEncryptionKeySet(masterKey *secret.Buffer) (*EncryptionKeySet, error) {
	if masterKey.Len() != KeySize {
		return nil, fmt.Errorf("artifact encryption key must be %d bytes, got %d", KeySize, masterKey.Len())
	}
	return &EncryptionKeySet{masterKey: masterKey}, nil
}

// Close zeroes and releases the master key. After Close, all
// derivation methods will panic. Idempotent.
func (keySet *EncryptionKeySet) Close() error {
	return keySet.masterKey.Close()
}

// EncryptContainer encrypts container bytes for external storage.
// Derives the container encryption key from the master key and
// container hash, encrypts using XChaCha20-Poly1305 with the
// container hash as AAD, and returns the encrypted blob.
func (keySet *EncryptionKeySet) EncryptContainer(containerBytes []byte, containerHash Hash) ([]byte, error) {
	encryptionKey, err := DeriveContainerEncryptionKey(keySet.masterKey, containerHash)
	if err != nil {
		return nil, fmt.Errorf("deriving container encryption key: %w", err)
	}
	defer encryptionKey.Close()

	return EncryptBlob(containerBytes, encryptionKey, containerHash)
}

// DecryptContainer decrypts a container blob from external storage.
func (keySet *EncryptionKeySet) DecryptContainer(encryptedBlob []byte, containerHash Hash) ([]byte, error) {
	encryptionKey, err := DeriveContainerEncryptionKey(keySet.masterKey, containerHash)
	if err != nil {
		return nil, fmt.Errorf("deriving container encryption key: %w", err)
	}
	defer encryptionKey.Close()

	return DecryptBlob(encryptedBlob, encryptionKey, containerHash)
}

// EncryptReconstruction encrypts a reconstruction record for external
// storage. Derives the per-artifact key, then the reconstruction
// encryption key, encrypts with the file hash as AAD.
func (keySet *EncryptionKeySet) EncryptReconstruction(recordBytes []byte, fileHash Hash) ([]byte, error) {
	perArtifactKey, err := DerivePerArtifactKey(keySet.masterKey, fileHash)
	if err != nil {
		return nil, fmt.Errorf("deriving per-artifact key: %w", err)
	}
	defer perArtifactKey.Close()

	reconKey, err := DeriveReconEncryptionKey(perArtifactKey)
	if err != nil {
		return nil, fmt.Errorf("deriving reconstruction encryption key: %w", err)
	}
	defer reconKey.Close()

	return EncryptBlob(recordBytes, reconKey, fileHash)
}

// DecryptReconstruction decrypts a reconstruction record from
// external storage.
func (keySet *EncryptionKeySet) DecryptReconstruction(encryptedBlob []byte, fileHash Hash) ([]byte, error) {
	perArtifactKey, err := DerivePerArtifactKey(keySet.masterKey, fileHash)
	if err != nil {
		return nil, fmt.Errorf("deriving per-artifact key: %w", err)
	}
	defer perArtifactKey.Close()

	reconKey, err := DeriveReconEncryptionKey(perArtifactKey)
	if err != nil {
		return nil, fmt.Errorf("deriving reconstruction encryption key: %w", err)
	}
	defer reconKey.Close()

	return DecryptBlob(encryptedBlob, reconKey, fileHash)
}

// ObscuredContainerRef returns the opaque external storage key for
// a container.
func (keySet *EncryptionKeySet) ObscuredContainerRef(containerHash Hash) Hash {
	return ObscureContainerReference(keySet.masterKey, containerHash)
}

// ObscuredReconRef returns the opaque external storage key for a
// reconstruction record. Derives the per-artifact key internally
// and closes it before returning.
func (keySet *EncryptionKeySet) ObscuredReconRef(fileHash Hash) Hash {
	perArtifactKey, err := DerivePerArtifactKey(keySet.masterKey, fileHash)
	if err != nil {
		// DerivePerArtifactKey only fails if HKDF fails, which requires
		// a broken SHA-256 implementation. Panic rather than returning
		// an error for a function that computes a deterministic hash.
		panic("artifact: deriving per-artifact key for reference obscuring: " + err.Error())
	}
	defer perArtifactKey.Close()

	return ObscureReconReference(perArtifactKey, fileHash)
}

// deriveKey is the shared HKDF-SHA256 implementation used by all key
// derivation functions. It derives a 32-byte key from
// inputKeyMaterial using the given info parameter. The salt is nil:
// the IKM is already uniformly random (HKDF-derived from the
// deployment master key by the launcher), so HKDF's extract phase
// with nil salt (HMAC-SHA256 with zero key) is appropriate per
// RFC 5869.
func deriveKey(inputKeyMaterial []byte, info []byte) (*secret.Buffer, error) {
	reader := hkdf.New(sha256.New, inputKeyMaterial, nil, info)
	derived := make([]byte, KeySize)
	if _, err := io.ReadFull(reader, derived); err != nil {
		secret.Zero(derived)
		return nil, fmt.Errorf("HKDF key derivation failed: %w", err)
	}
	// NewFromBytes copies into mmap and zeros the heap slice.
	return secret.NewFromBytes(derived)
}

// obscureReference computes a BLAKE3 keyed hash for reference
// obscuring. The key must be exactly 32 bytes (guaranteed by all
// callers since keys come from HKDF output or the master key).
func obscureReference(key []byte, domainTag []byte, hashValue Hash) Hash {
	hasher, err := blake3.NewKeyed(key)
	if err != nil {
		panic("artifact: BLAKE3 keyed hash initialization failed (key must be 32 bytes): " + err.Error())
	}
	hasher.Write(domainTag)
	hasher.Write(hashValue[:])
	var result Hash
	copy(result[:], hasher.Sum(nil))
	return result
}

// buildAAD constructs the additional authenticated data for AEAD
// operations: the version byte followed by the identity hash. The
// identity hash binds the ciphertext to the specific artifact or
// container, preventing an attacker from swapping encrypted blobs
// between different artifacts in external storage.
func buildAAD(version byte, identityHash Hash) []byte {
	aad := make([]byte, 1+len(identityHash))
	aad[0] = version
	copy(aad[1:], identityHash[:])
	return aad
}
