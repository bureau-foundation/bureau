// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package provenance

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"hash"
	"math/big"
	"math/bits"
	"strconv"
	"strings"
	"time"
)

// ---- Bundle JSON types ------------------------------------------------
//
// These mirror the Sigstore protobuf-specs bundle format, serialized
// as protobuf-JSON (camelCase field names, int64 as strings, bytes as
// base64). We support bundle media types v0.1 through v0.3.

// rawBundle is the top-level Sigstore bundle JSON structure.
type rawBundle struct {
	MediaType            string                  `json:"mediaType"`
	VerificationMaterial rawVerificationMaterial `json:"verificationMaterial"`
	MessageSignature     *rawMessageSignature    `json:"messageSignature,omitempty"`
	DSSEEnvelope         *rawDSSEEnvelope        `json:"dsseEnvelope,omitempty"`
}

type rawVerificationMaterial struct {
	// v0.3: single certificate.
	Certificate *rawCertificate `json:"certificate,omitempty"`
	// v0.1/v0.2: certificate chain.
	X509CertificateChain *rawCertificateChain `json:"x509CertificateChain,omitempty"`
	TlogEntries          []rawTlogEntry       `json:"tlogEntries"`
}

type rawCertificate struct {
	RawBytes string `json:"rawBytes"` // base64 DER
}

type rawCertificateChain struct {
	Certificates []rawCertificate `json:"certificates"`
}

type rawTlogEntry struct {
	LogIndex          string               `json:"logIndex"`
	LogID             rawLogID             `json:"logId"`
	KindVersion       rawKindVersion       `json:"kindVersion"`
	IntegratedTime    string               `json:"integratedTime"`
	InclusionPromise  *rawInclusionPromise `json:"inclusionPromise,omitempty"`
	InclusionProof    *rawInclusionProof   `json:"inclusionProof,omitempty"`
	CanonicalizedBody string               `json:"canonicalizedBody"` // base64
}

type rawLogID struct {
	KeyID string `json:"keyId"` // base64 of SHA-256(DER public key)
}

type rawKindVersion struct {
	Kind    string `json:"kind"`
	Version string `json:"version"`
}

type rawInclusionPromise struct {
	SignedEntryTimestamp string `json:"signedEntryTimestamp"` // base64
}

type rawInclusionProof struct {
	LogIndex   string        `json:"logIndex"`
	RootHash   string        `json:"rootHash"` // base64
	TreeSize   string        `json:"treeSize"`
	Hashes     []string      `json:"hashes"` // base64
	Checkpoint rawCheckpoint `json:"checkpoint"`
}

type rawCheckpoint struct {
	Envelope string `json:"envelope"`
}

type rawMessageSignature struct {
	MessageDigest rawMessageDigest `json:"messageDigest"`
	Signature     string           `json:"signature"` // base64
}

type rawMessageDigest struct {
	Algorithm string `json:"algorithm"` // e.g., "SHA2_256"
	Digest    string `json:"digest"`    // base64
}

type rawDSSEEnvelope struct {
	Payload     string             `json:"payload"` // base64
	PayloadType string             `json:"payloadType"`
	Signatures  []rawDSSESignature `json:"signatures"`
}

type rawDSSESignature struct {
	Sig   string `json:"sig"` // base64
	KeyID string `json:"keyid"`
}

// ---- Trusted root JSON types ------------------------------------------
//
// These mirror the Sigstore trusted root format. We parse them to
// extract the Fulcio root certificate and Rekor public key.

type rawTrustedRoot struct {
	MediaType              string                    `json:"mediaType"`
	Tlogs                  []rawTrustedLog           `json:"tlogs"`
	CertificateAuthorities []rawCertificateAuthority `json:"certificateAuthorities"`
}

type rawTrustedLog struct {
	BaseURL       string        `json:"baseUrl"`
	HashAlgorithm string        `json:"hashAlgorithm"`
	PublicKey     rawTrustedKey `json:"publicKey"`
	LogID         rawLogID      `json:"logId"`
}

type rawTrustedKey struct {
	RawBytes   string       `json:"rawBytes"`   // base64 DER
	KeyDetails string       `json:"keyDetails"` // e.g., "PKIX_ECDSA_P256_SHA_256"
	ValidFor   *rawValidFor `json:"validFor,omitempty"`
}

type rawCertificateAuthority struct {
	Subject   *rawDistinguishedName `json:"subject,omitempty"`
	URI       string                `json:"uri"`
	CertChain rawCertificateChain   `json:"certChain"`
	ValidFor  *rawValidFor          `json:"validFor,omitempty"`
}

type rawDistinguishedName struct {
	Organization string `json:"organization"`
	CommonName   string `json:"commonName"`
}

type rawValidFor struct {
	Start string `json:"start"`
	End   string `json:"end,omitempty"`
}

// ---- Parsed internal types --------------------------------------------

// parsedBundle holds the components extracted from a Sigstore bundle
// JSON file, ready for verification.
type parsedBundle struct {
	leafCert        *x509.Certificate
	signature       []byte
	isDSSE          bool
	dssePayload     []byte
	dssePayloadType string
	// For messageSignature bundles: the declared artifact digest.
	digestAlgorithm string // normalized: "sha256", "sha384", "sha512"
	digestValue     []byte
	// For DSSE bundles: subject digests from the in-toto statement.
	dsseSubjects []inTotoSubject
	tlogEntry    parsedTlogEntry
}

type parsedTlogEntry struct {
	logIndex          int64
	logKeyID          []byte // from bundle, base64-decoded
	integratedTime    int64
	canonicalizedBody []byte
	// canonicalizedBodyB64 is the raw base64 string from the bundle
	// JSON, needed for SET verification. The SET payload includes
	// this string verbatim (not re-encoded from decoded bytes).
	canonicalizedBodyB64 string
	// signedEntryTimestamp is the Rekor Signed Entry Timestamp
	// (SET): an ECDSA-SHA256 signature over the canonical JSON
	// payload {"body":..., "integratedTime":..., "logID":...,
	// "logIndex":...}. This is the only field that
	// cryptographically authenticates the integratedTime.
	signedEntryTimestamp []byte
	inclusionProof       *parsedInclusionProof
}

type parsedInclusionProof struct {
	logIndex   int64
	rootHash   []byte
	treeSize   int64
	hashes     [][]byte
	checkpoint string
}

// parsedRoot holds the cryptographic material from a trust root,
// ready for verification.
type parsedRoot struct {
	fulcioRoots         []*x509.Certificate
	fulcioIntermediates []*x509.Certificate
	rekorKey            crypto.PublicKey
	rekorKeyID          []byte // SHA-256 of DER-encoded public key
}

// ---- In-toto statement types ------------------------------------------

// inTotoStatement is the top-level in-toto statement structure, parsed
// from the DSSE payload.
type inTotoStatement struct {
	Type          string          `json:"_type"`
	Subject       []inTotoSubject `json:"subject"`
	PredicateType string          `json:"predicateType"`
}

// inTotoSubject represents a single subject in an in-toto statement.
type inTotoSubject struct {
	Name   string            `json:"name"`
	Digest map[string]string `json:"digest"` // algorithm → hex digest
}

// matchesDigest checks whether this subject has a digest matching the
// given algorithm and value. The comparison is case-insensitive
// because the in-toto spec does not mandate hex case, and different
// tools may produce uppercase or lowercase hex strings.
func (s inTotoSubject) matchesDigest(algorithm string, digest []byte) bool {
	hexDigest, ok := s.Digest[algorithm]
	if !ok {
		return false
	}
	expected := hex.EncodeToString(digest)
	return strings.EqualFold(hexDigest, expected)
}

// ---- Bundle parsing ---------------------------------------------------

// parseBundle parses a Sigstore bundle JSON into its components.
func parseBundle(data []byte) (*parsedBundle, error) {
	var raw rawBundle
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal bundle JSON: %w", err)
	}

	// Reject bundles that contain both signature types. A well-formed
	// bundle has exactly one of messageSignature or dsseEnvelope. If
	// both are present, the bundle is malformed or adversarially
	// constructed — accepting the first one silently would allow type
	// confusion attacks where the bundle verifies via messageSignature
	// but a downstream consumer interprets the dsseEnvelope. This
	// check runs before any cryptographic parsing to fail fast.
	if raw.MessageSignature != nil && raw.DSSEEnvelope != nil {
		return nil, errors.New("bundle contains both messageSignature and dsseEnvelope; exactly one is required")
	}

	// Extract the leaf certificate.
	certDER, err := extractLeafCertDER(&raw)
	if err != nil {
		return nil, err
	}
	leafCert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("parse leaf certificate: %w", err)
	}

	result := &parsedBundle{
		leafCert: leafCert,
	}

	// Extract signature and content type.
	if raw.MessageSignature != nil {
		sig, err := base64.StdEncoding.DecodeString(raw.MessageSignature.Signature)
		if err != nil {
			return nil, fmt.Errorf("decode message signature: %w", err)
		}
		result.signature = sig
		result.isDSSE = false

		result.digestAlgorithm, err = normalizeHashAlgorithm(raw.MessageSignature.MessageDigest.Algorithm)
		if err != nil {
			return nil, err
		}
		result.digestValue, err = base64.StdEncoding.DecodeString(raw.MessageSignature.MessageDigest.Digest)
		if err != nil {
			return nil, fmt.Errorf("decode message digest: %w", err)
		}
	} else if raw.DSSEEnvelope != nil {
		if len(raw.DSSEEnvelope.Signatures) != 1 {
			return nil, fmt.Errorf("DSSE envelope has %d signatures, want exactly 1", len(raw.DSSEEnvelope.Signatures))
		}
		sig, err := base64.StdEncoding.DecodeString(raw.DSSEEnvelope.Signatures[0].Sig)
		if err != nil {
			return nil, fmt.Errorf("decode DSSE signature: %w", err)
		}
		result.signature = sig
		result.isDSSE = true
		result.dssePayloadType = raw.DSSEEnvelope.PayloadType
		result.dssePayload, err = base64.StdEncoding.DecodeString(raw.DSSEEnvelope.Payload)
		if err != nil {
			return nil, fmt.Errorf("decode DSSE payload: %w", err)
		}

		// Parse the in-toto statement to extract subject digests.
		var statement inTotoStatement
		if err := json.Unmarshal(result.dssePayload, &statement); err != nil {
			return nil, fmt.Errorf("parse DSSE in-toto statement: %w", err)
		}
		if len(statement.Subject) == 0 {
			return nil, errors.New("DSSE in-toto statement has no subjects")
		}
		result.dsseSubjects = statement.Subject
	} else {
		return nil, errors.New("bundle has neither messageSignature nor dsseEnvelope")
	}

	// Parse the tlog entry.
	if len(raw.VerificationMaterial.TlogEntries) == 0 {
		return nil, errors.New("bundle has no tlog entries")
	}
	entry := raw.VerificationMaterial.TlogEntries[0]

	logIndex, err := strconv.ParseInt(entry.LogIndex, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse tlog logIndex: %w", err)
	}

	logKeyID, err := base64.StdEncoding.DecodeString(entry.LogID.KeyID)
	if err != nil {
		return nil, fmt.Errorf("decode tlog logId: %w", err)
	}

	integratedTime, err := strconv.ParseInt(entry.IntegratedTime, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse tlog integratedTime: %w", err)
	}

	body, err := base64.StdEncoding.DecodeString(entry.CanonicalizedBody)
	if err != nil {
		return nil, fmt.Errorf("decode tlog canonicalizedBody: %w", err)
	}

	result.tlogEntry = parsedTlogEntry{
		logIndex:             logIndex,
		logKeyID:             logKeyID,
		integratedTime:       integratedTime,
		canonicalizedBody:    body,
		canonicalizedBodyB64: entry.CanonicalizedBody,
	}

	// Extract the Signed Entry Timestamp (SET) if present.
	if entry.InclusionPromise != nil && entry.InclusionPromise.SignedEntryTimestamp != "" {
		setBytes, err := base64.StdEncoding.DecodeString(entry.InclusionPromise.SignedEntryTimestamp)
		if err != nil {
			return nil, fmt.Errorf("decode signed entry timestamp: %w", err)
		}
		result.tlogEntry.signedEntryTimestamp = setBytes
	}

	// Parse inclusion proof if present.
	if entry.InclusionProof != nil {
		proof, err := parseInclusionProof(entry.InclusionProof)
		if err != nil {
			return nil, fmt.Errorf("parse inclusion proof: %w", err)
		}
		result.tlogEntry.inclusionProof = proof
	}

	return result, nil
}

func extractLeafCertDER(raw *rawBundle) ([]byte, error) {
	if raw.VerificationMaterial.Certificate != nil {
		return base64.StdEncoding.DecodeString(raw.VerificationMaterial.Certificate.RawBytes)
	}
	if raw.VerificationMaterial.X509CertificateChain != nil {
		certs := raw.VerificationMaterial.X509CertificateChain.Certificates
		if len(certs) == 0 {
			return nil, errors.New("x509 certificate chain is empty")
		}
		// Leaf certificate is first in the chain.
		return base64.StdEncoding.DecodeString(certs[0].RawBytes)
	}
	return nil, errors.New("bundle has no certificate or certificate chain")
}

func parseInclusionProof(raw *rawInclusionProof) (*parsedInclusionProof, error) {
	logIndex, err := strconv.ParseInt(raw.LogIndex, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("logIndex: %w", err)
	}

	rootHash, err := base64.StdEncoding.DecodeString(raw.RootHash)
	if err != nil {
		return nil, fmt.Errorf("rootHash: %w", err)
	}

	treeSize, err := strconv.ParseInt(raw.TreeSize, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("treeSize: %w", err)
	}

	hashes := make([][]byte, len(raw.Hashes))
	for i, h := range raw.Hashes {
		hashes[i], err = base64.StdEncoding.DecodeString(h)
		if err != nil {
			return nil, fmt.Errorf("hash[%d]: %w", i, err)
		}
	}

	return &parsedInclusionProof{
		logIndex:   logIndex,
		rootHash:   rootHash,
		treeSize:   treeSize,
		hashes:     hashes,
		checkpoint: raw.Checkpoint.Envelope,
	}, nil
}

func normalizeHashAlgorithm(algorithm string) (string, error) {
	switch algorithm {
	case "SHA2_256", "sha256":
		return "sha256", nil
	case "SHA2_384", "sha384":
		return "sha384", nil
	case "SHA2_512", "sha512":
		return "sha512", nil
	default:
		return "", fmt.Errorf("unsupported hash algorithm %q: only sha256, sha384, sha512 are accepted", algorithm)
	}
}

// ---- Trusted root parsing ---------------------------------------------

// parseTrustedRootJSON parses a Sigstore trusted root JSON file and
// extracts the Fulcio CA certificates and Rekor public key.
func parseTrustedRootJSON(data []byte) (*parsedRoot, error) {
	var raw rawTrustedRoot
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal trusted root JSON: %w", err)
	}

	// Extract Fulcio CA certificates, separating trust anchors
	// (self-signed root CAs) from intermediates. This distinction
	// matters: intermediates must chain to a root and are subject
	// to the root's path length and name constraints. Treating
	// intermediates as trust anchors bypasses those constraints.
	var fulcioRoots []*x509.Certificate
	var fulcioIntermediates []*x509.Certificate
	for i, ca := range raw.CertificateAuthorities {
		for j, certEntry := range ca.CertChain.Certificates {
			certDER, err := base64.StdEncoding.DecodeString(certEntry.RawBytes)
			if err != nil {
				return nil, fmt.Errorf("CA[%d] cert[%d]: decode base64: %w", i, j, err)
			}
			cert, err := x509.ParseCertificate(certDER)
			if err != nil {
				return nil, fmt.Errorf("CA[%d] cert[%d]: parse x509: %w", i, j, err)
			}
			if !cert.IsCA {
				continue
			}
			if isSelfSigned(cert) {
				fulcioRoots = append(fulcioRoots, cert)
			} else {
				fulcioIntermediates = append(fulcioIntermediates, cert)
			}
		}
	}
	if len(fulcioRoots) == 0 {
		return nil, errors.New("trusted root has no Fulcio root CA certificates (self-signed)")
	}

	// Extract Rekor transparency log public key.
	if len(raw.Tlogs) == 0 {
		return nil, errors.New("trusted root has no transparency logs")
	}
	tlog := raw.Tlogs[0]

	keyDER, err := base64.StdEncoding.DecodeString(tlog.PublicKey.RawBytes)
	if err != nil {
		return nil, fmt.Errorf("decode Rekor public key: %w", err)
	}
	rekorKey, err := x509.ParsePKIXPublicKey(keyDER)
	if err != nil {
		return nil, fmt.Errorf("parse Rekor public key: %w", err)
	}

	logKeyID, err := base64.StdEncoding.DecodeString(tlog.LogID.KeyID)
	if err != nil {
		return nil, fmt.Errorf("decode Rekor log ID: %w", err)
	}

	return &parsedRoot{
		fulcioRoots:         fulcioRoots,
		fulcioIntermediates: fulcioIntermediates,
		rekorKey:            rekorKey,
		rekorKeyID:          logKeyID,
	}, nil
}

// isSelfSigned checks whether a certificate is self-signed. Uses a
// layered strategy: fast negative checks first, then cryptographic
// verification as the source of truth.
//
// The cryptographic check (CheckSignatureFrom) is necessary because
// heuristics like Issuer==Subject or AKI==SKI can produce false
// positives. An intermediate CA issued with the same DN as its root
// (e.g., cross-certification or key rollover) would pass both
// heuristic checks but fail the signature check, since its signature
// was made by the root's key, not its own. Misclassifying such a cert
// as a root would make it a trust anchor, bypassing path length and
// name constraints from the actual root.
//
// The cost of CheckSignatureFrom is negligible: it runs only during
// trusted root parsing (once at startup), not on the verification
// hot path.
func isSelfSigned(cert *x509.Certificate) bool {
	// Fast negative: if Issuer != Subject, it cannot be self-signed.
	if cert.Issuer.String() != cert.Subject.String() {
		return false
	}

	// Fast negative: if both AKI and SKI are present but differ,
	// the cert was issued by a different key.
	if len(cert.AuthorityKeyId) > 0 && len(cert.SubjectKeyId) > 0 {
		if !constantTimeEqual(cert.AuthorityKeyId, cert.SubjectKeyId) {
			return false
		}
	}

	// Cryptographic verification: verify that the cert's signature
	// was made by its own public key. This is the authoritative
	// check — it catches same-DN intermediates where heuristics
	// give false positives.
	return cert.CheckSignatureFrom(cert) == nil
}

// ---- Certificate chain verification -----------------------------------

// verifyCertificateChain verifies that the leaf certificate chains to
// one of the trusted Fulcio root CAs, optionally through intermediate
// CAs. Roots are trust anchors (self-signed); intermediates must
// themselves chain to a root and are subject to path length and name
// constraints. Returns nil on success.
func verifyCertificateChain(leaf *x509.Certificate, root *parsedRoot) error {
	rootPool := x509.NewCertPool()
	for _, rootCert := range root.fulcioRoots {
		rootPool.AddCert(rootCert)
	}

	intermediatePool := x509.NewCertPool()
	for _, intermediate := range root.fulcioIntermediates {
		intermediatePool.AddCert(intermediate)
	}

	// Fulcio leaf certificates are short-lived (typically 10 minutes)
	// and may be expired by the time we verify. The signing time is
	// established by the Rekor tlog entry's integrated timestamp, not
	// by the current wall clock. We set CurrentTime to the cert's
	// NotBefore to bypass expiration checks — the tlog timestamp
	// proves the signature was made while the cert was valid.
	//
	// Fulcio certificates contain private-use extensions under
	// 1.3.6.1.4.1.57264.1.* that Go's x509 verifier doesn't
	// understand. These are marked as critical, so Go rejects
	// them as "unhandled critical extensions." We process these
	// extensions ourselves in extractFulcioClaims, so remove
	// only the specific OIDs we handle. Any remaining unknown
	// critical extension causes verification to fail — this is
	// intentional defense-in-depth: a CA-added critical
	// constraint must not be silently ignored.
	leaf.UnhandledCriticalExtensions = filterHandledCriticalExtensions(
		leaf.UnhandledCriticalExtensions,
	)

	opts := x509.VerifyOptions{
		Roots:         rootPool,
		Intermediates: intermediatePool,
		CurrentTime:   leaf.NotBefore,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
	}

	if _, err := leaf.Verify(opts); err != nil {
		return fmt.Errorf("certificate chain verification failed: %w", err)
	}
	return nil
}

// ---- Signature verification -------------------------------------------

// verifySignature verifies the bundle's signature against the expected
// artifact digest using the leaf certificate's public key.
func verifySignature(bundle *parsedBundle, expectedDigest []byte) error {
	pubKey, ok := bundle.leafCert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return fmt.Errorf("unsupported public key type %T, want *ecdsa.PublicKey", bundle.leafCert.PublicKey)
	}

	var digest []byte
	if bundle.isDSSE {
		// DSSE: signature is over PAE(payloadType, payload).
		pae := dssePreAuthEncoding(bundle.dssePayloadType, bundle.dssePayload)
		digest = hashForKey(pubKey, pae)
	} else {
		// MessageSignature: signature is over the artifact digest directly.
		// The caller-provided digest IS the hash — verify against it.
		digest = expectedDigest
	}

	if !ecdsa.VerifyASN1(pubKey, digest, bundle.signature) {
		return errors.New("ECDSA signature verification failed")
	}
	return nil
}

// dssePreAuthEncoding computes the DSSE Pre-Authentication Encoding:
// "DSSEv1" SP LEN(type) SP type SP LEN(body) SP body
func dssePreAuthEncoding(payloadType string, payload []byte) []byte {
	return []byte(fmt.Sprintf("DSSEv1 %d %s %d %s",
		len(payloadType), payloadType, len(payload), payload))
}

// hashForKey computes the appropriate hash of data for the given
// ECDSA public key's curve.
func hashForKey(key *ecdsa.PublicKey, data []byte) []byte {
	switch key.Curve {
	case elliptic.P384():
		h := sha512.Sum384(data)
		return h[:]
	case elliptic.P521():
		h := sha512.Sum512(data)
		return h[:]
	default: // P-256 and anything else
		h := sha256.Sum256(data)
		return h[:]
	}
}

// ---- Transparency log verification -----------------------------------

// verifyTlogEntry verifies the Rekor transparency log entry: the
// Merkle inclusion proof, checkpoint signature, and binding to the
// bundle's signature and certificate.
//
// The binding check is critical: without it, an attacker could reuse
// a valid tlog entry from a previous signing event with a different
// signature, defeating the "every artifact must be tlog-recorded"
// guarantee.
func verifyTlogEntry(entry *parsedTlogEntry, rekorKey crypto.PublicKey, rekorKeyID []byte, bundle *parsedBundle) error {
	if entry.inclusionProof == nil {
		return errors.New("tlog entry has no inclusion proof")
	}

	// Verify the tlog entry's LogID matches the trusted root's key
	// ID. While the checkpoint signature prevents accepting proofs
	// from untrusted logs, matching the LogID ensures metadata
	// consistency — a mismatched LogID would indicate the entry
	// claims to be from a different log than the one whose key we
	// are verifying against.
	if !constantTimeEqual(entry.logKeyID, rekorKeyID) {
		return errors.New("tlog entry LogID does not match trusted Rekor key ID")
	}

	// Verify the Merkle inclusion proof.
	leafHash := rfc6962LeafHash(entry.canonicalizedBody)
	if err := verifyInclusionProof(
		entry.inclusionProof.logIndex,
		entry.inclusionProof.treeSize,
		entry.inclusionProof.rootHash,
		leafHash,
		entry.inclusionProof.hashes,
	); err != nil {
		return fmt.Errorf("Merkle inclusion proof: %w", err)
	}

	// Verify the checkpoint signature.
	if err := verifyCheckpoint(
		entry.inclusionProof.checkpoint,
		entry.inclusionProof.rootHash,
		entry.inclusionProof.treeSize,
		rekorKey,
		rekorKeyID,
	); err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}

	// Verify that the logged entry binds to this bundle's signature
	// and certificate. Without this check, an attacker could sign
	// artifact B but attach a tlog entry from signing artifact A.
	if err := verifyTlogBinding(entry.canonicalizedBody, bundle); err != nil {
		return fmt.Errorf("tlog binding: %w", err)
	}

	// Verify the Signed Entry Timestamp (SET). The SET is the only
	// field that cryptographically authenticates integratedTime.
	// Without this check, an attacker could modify integratedTime in
	// the bundle JSON to make an expired-cert signature appear to
	// fall within the certificate's validity window.
	if err := verifySignedEntryTimestamp(entry, rekorKey); err != nil {
		return fmt.Errorf("signed entry timestamp: %w", err)
	}

	// Verify that the tlog integrated timestamp falls within the
	// leaf certificate's validity period. Fulcio certificates are
	// short-lived (typically 10 minutes). The integrated timestamp
	// proves the signature was made while the cert was valid. Without
	// this check, an attacker with a compromised private key could
	// continue signing artifacts after the certificate expires.
	signingTime := time.Unix(entry.integratedTime, 0)
	if signingTime.Before(bundle.leafCert.NotBefore) {
		return fmt.Errorf("tlog integrated time %v is before certificate validity start %v",
			signingTime, bundle.leafCert.NotBefore)
	}
	if signingTime.After(bundle.leafCert.NotAfter) {
		return fmt.Errorf("tlog integrated time %v is after certificate validity end %v",
			signingTime, bundle.leafCert.NotAfter)
	}

	return nil
}

// ---- Signed Entry Timestamp (SET) verification -------------------------
//
// The SET is an ECDSA-SHA256 signature by Rekor over a canonical JSON
// payload containing the entry's body, integratedTime, logID, and
// logIndex. It is the sole mechanism that cryptographically binds the
// integratedTime to the Rekor signing key. Without SET verification,
// integratedTime is an unauthenticated field that an attacker can
// modify freely.

// setPayload is the canonical JSON structure signed by Rekor's SET.
// Fields are in alphabetical order — Go's json.Marshal emits struct
// fields in declaration order, and alphabetical order matches the
// canonical form that Rekor uses (JSON with sorted keys, compact
// separators).
type setPayload struct {
	Body           string `json:"body"`
	IntegratedTime int64  `json:"integratedTime"`
	LogID          string `json:"logID"`
	LogIndex       int64  `json:"logIndex"`
}

// verifySignedEntryTimestamp verifies the Rekor Signed Entry Timestamp
// (SET). The SET is an ECDSA signature over the canonical JSON payload
// {"body":"<base64>","integratedTime":<int>,"logID":"<hex>","logIndex":<int>}.
//
// This is essential: without SET verification, an attacker can
// arbitrarily modify integratedTime (and logIndex) in the bundle JSON
// because those fields are not covered by the Merkle inclusion proof
// or checkpoint signature.
func verifySignedEntryTimestamp(entry *parsedTlogEntry, rekorKey crypto.PublicKey) error {
	if len(entry.signedEntryTimestamp) == 0 {
		return errors.New("no signed entry timestamp (SET) in tlog entry")
	}

	payload := setPayload{
		Body:           entry.canonicalizedBodyB64,
		IntegratedTime: entry.integratedTime,
		LogID:          hex.EncodeToString(entry.logKeyID),
		LogIndex:       entry.logIndex,
	}

	canonicalJSON, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal SET payload: %w", err)
	}

	digest := sha256.Sum256(canonicalJSON)

	ecdsaKey, ok := rekorKey.(*ecdsa.PublicKey)
	if !ok {
		return fmt.Errorf("SET verification requires ECDSA key, got %T", rekorKey)
	}

	if !ecdsa.VerifyASN1(ecdsaKey, digest[:], entry.signedEntryTimestamp) {
		return errors.New("SET signature verification failed: integratedTime may have been tampered with")
	}

	return nil
}

// ---- Rekor entry body types -------------------------------------------
//
// The canonicalizedBody in a tlog entry is a base64-encoded JSON
// Rekor log entry. Two kinds are relevant:
//   - hashedrekord: for messageSignature bundles
//   - intoto: for DSSE bundles

type rekorEntryBody struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Spec       json.RawMessage `json:"spec"`
}

// hashedrekord spec
type rekorHashedRekordSpec struct {
	Signature rekorSignature `json:"signature"`
}

type rekorSignature struct {
	Content   string         `json:"content"` // base64 signature
	PublicKey rekorPublicKey `json:"publicKey"`
}

type rekorPublicKey struct {
	Content string `json:"content"` // base64 PEM certificate
}

// intoto spec
type rekorIntotoSpec struct {
	Content rekorIntotoContent `json:"content"`
}

type rekorIntotoContent struct {
	Envelope rekorIntotoEnvelope `json:"envelope"`
}

type rekorIntotoEnvelope struct {
	Signatures []rekorIntotoSig `json:"signatures"`
}

type rekorIntotoSig struct {
	PublicKey string `json:"publicKey"` // base64 PEM certificate
	Sig       string `json:"sig"`       // base64 signature
}

// verifyTlogBinding verifies that the Rekor entry body contains the
// same certificate and signature as the bundle being verified.
func verifyTlogBinding(canonicalizedBody []byte, bundle *parsedBundle) error {
	var entry rekorEntryBody
	if err := json.Unmarshal(canonicalizedBody, &entry); err != nil {
		return fmt.Errorf("unmarshal rekor entry body: %w", err)
	}

	switch entry.Kind {
	case "hashedrekord":
		return verifyHashedRekordBinding(entry.Spec, bundle)
	case "intoto":
		return verifyIntotoBinding(entry.Spec, bundle)
	default:
		return fmt.Errorf("unsupported rekor entry kind %q", entry.Kind)
	}
}

func verifyHashedRekordBinding(specBytes json.RawMessage, bundle *parsedBundle) error {
	var spec rekorHashedRekordSpec
	if err := json.Unmarshal(specBytes, &spec); err != nil {
		return fmt.Errorf("unmarshal hashedrekord spec: %w", err)
	}

	// Verify the signature matches.
	entrySig, err := base64.StdEncoding.DecodeString(spec.Signature.Content)
	if err != nil {
		return fmt.Errorf("decode entry signature: %w", err)
	}
	if !constantTimeEqual(entrySig, bundle.signature) {
		return errors.New("entry signature does not match bundle signature")
	}

	// Verify the certificate matches.
	return verifyEntryCertBinding(spec.Signature.PublicKey.Content, bundle.leafCert)
}

func verifyIntotoBinding(specBytes json.RawMessage, bundle *parsedBundle) error {
	var spec rekorIntotoSpec
	if err := json.Unmarshal(specBytes, &spec); err != nil {
		return fmt.Errorf("unmarshal intoto spec: %w", err)
	}

	if len(spec.Content.Envelope.Signatures) == 0 {
		return errors.New("intoto entry has no signatures")
	}

	sig := spec.Content.Envelope.Signatures[0]

	// Verify the signature matches. The intoto entry's
	// canonicalizedBody is base64-encoded JSON. Within that JSON,
	// sig.Sig is ALSO base64-encoded (the DSSE envelope format
	// stores signatures as base64 strings). So sig.Sig here is
	// double-base64: decoding it once yields the base64 string of
	// the raw signature, which we compare against the base64
	// encoding of the bundle's raw signature bytes.
	sigB64Bytes, err := base64.StdEncoding.DecodeString(sig.Sig)
	if err != nil {
		return fmt.Errorf("decode intoto entry signature: %w", err)
	}
	bundleSigB64 := base64.StdEncoding.EncodeToString(bundle.signature)
	if string(sigB64Bytes) != bundleSigB64 {
		return errors.New("entry signature does not match bundle signature")
	}

	// Verify the certificate matches.
	return verifyEntryCertBinding(sig.PublicKey, bundle.leafCert)
}

// verifyEntryCertBinding verifies that the PEM-encoded certificate
// in the Rekor entry matches the bundle's leaf certificate.
func verifyEntryCertBinding(pemB64 string, leafCert *x509.Certificate) error {
	pemBytes, err := base64.StdEncoding.DecodeString(pemB64)
	if err != nil {
		return fmt.Errorf("decode entry certificate base64: %w", err)
	}

	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return errors.New("entry certificate is not valid PEM")
	}

	if !constantTimeEqual(block.Bytes, leafCert.Raw) {
		return errors.New("entry certificate does not match bundle leaf certificate")
	}

	return nil
}

// ---- RFC 6962 Merkle tree ---------------------------------------------

// rfc6962LeafHash computes the Merkle tree leaf hash per RFC 6962 s2.1:
// SHA-256(0x00 || data).
func rfc6962LeafHash(data []byte) []byte {
	hasher := sha256.New()
	hasher.Write([]byte{0x00})
	hasher.Write(data)
	return hasher.Sum(nil)
}

// rfc6962NodeHash computes the internal node hash per RFC 6962 s2.1:
// SHA-256(0x01 || left || right).
func rfc6962NodeHash(left, right []byte) []byte {
	hasher := sha256.New()
	hasher.Write([]byte{0x01})
	hasher.Write(left)
	hasher.Write(right)
	return hasher.Sum(nil)
}

// verifyInclusionProof verifies a Merkle tree inclusion proof.
// Algorithm from transparency-dev/merkle/proof/verify.go.
func verifyInclusionProof(leafIndex, treeSize int64, expectedRoot, leafHash []byte, proof [][]byte) error {
	if leafIndex < 0 || treeSize <= 0 || leafIndex >= treeSize {
		return fmt.Errorf("invalid index/size: index=%d size=%d", leafIndex, treeSize)
	}

	// Decompose the proof into inner and border nodes.
	inner := innerProofSize(leafIndex, treeSize)
	border := bits.OnesCount64(uint64(leafIndex) >> uint(inner))

	if len(proof) != inner+border {
		return fmt.Errorf("proof length %d does not match expected %d (inner=%d border=%d)",
			len(proof), inner+border, inner, border)
	}

	for i, hash := range proof {
		if len(hash) != sha256.Size {
			return fmt.Errorf("proof[%d] length = %d, want %d (SHA-256)", i, len(hash), sha256.Size)
		}
	}

	// Chain through inner proof nodes.
	currentHash := leafHash
	for i := 0; i < inner; i++ {
		if (uint64(leafIndex)>>uint(i))&1 == 0 {
			currentHash = rfc6962NodeHash(currentHash, proof[i])
		} else {
			currentHash = rfc6962NodeHash(proof[i], currentHash)
		}
	}

	// Chain through border proof nodes (always left-side hashes).
	for i := inner; i < len(proof); i++ {
		currentHash = rfc6962NodeHash(proof[i], currentHash)
	}

	if !constantTimeEqual(currentHash, expectedRoot) {
		return errors.New("computed root hash does not match expected root")
	}

	return nil
}

// innerProofSize returns the number of inner proof nodes for a leaf
// at the given index in a tree of the given size.
func innerProofSize(index, size int64) int {
	return bits.Len64(uint64(index) ^ uint64(size-1))
}

// constantTimeEqual compares two byte slices in constant time to
// prevent timing side-channel attacks. All comparisons of
// cryptographic material (signatures, hashes, Merkle proofs, key IDs)
// must use this function, never short-circuit byte comparison.
func constantTimeEqual(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

// ---- Checkpoint (signed note) verification ----------------------------

// verifyCheckpoint parses and verifies a Rekor checkpoint (signed note).
// The checkpoint must contain the expected root hash and tree size,
// and its signature must verify against the Rekor public key.
func verifyCheckpoint(envelope string, expectedRoot []byte, expectedSize int64, rekorKey crypto.PublicKey, rekorKeyID []byte) error {
	noteText, signerName, sigBytes, keyID, err := parseSignedNote(envelope)
	if err != nil {
		return fmt.Errorf("parse signed note: %w", err)
	}

	// Verify the key ID matches the Rekor key.
	if !constantTimeEqual(keyID, rekorKeyID[:4]) {
		// Key ID is the first 4 bytes of SHA-256(DER public key).
		// Compute from our known key.
		computedKeyID := computeCheckpointKeyID(rekorKey)
		if !constantTimeEqual(keyID, computedKeyID) {
			return fmt.Errorf("checkpoint key ID %x does not match Rekor key ID %x", keyID, computedKeyID)
		}
	}

	// Parse the checkpoint body to extract origin, tree size, and
	// root hash.
	origin, cpSize, cpRoot, err := parseCheckpointBody(noteText)
	if err != nil {
		return fmt.Errorf("parse checkpoint body: %w", err)
	}

	// Cross-check the checkpoint origin against the signature's
	// signer name. The origin line in the checkpoint body says
	// "this is log X's state" while the signature line says "signed
	// by X." If they disagree, the checkpoint content and signature
	// are from different logs — which shouldn't be possible if the
	// signature is valid, but checking costs nothing.
	originName := origin
	if dashIndex := strings.Index(origin, " - "); dashIndex >= 0 {
		originName = origin[:dashIndex]
	}
	if signerName != originName {
		return fmt.Errorf("checkpoint origin %q does not match signer %q", originName, signerName)
	}

	if cpSize != expectedSize {
		return fmt.Errorf("checkpoint tree size %d does not match proof tree size %d", cpSize, expectedSize)
	}
	if !constantTimeEqual(cpRoot, expectedRoot) {
		return errors.New("checkpoint root hash does not match proof root hash")
	}

	// Verify the signature over the note text.
	if err := verifyCheckpointSignature(noteText, sigBytes, rekorKey); err != nil {
		return err
	}

	return nil
}

// parseSignedNote parses a C2SP signed note format string. Returns
// the note text (including trailing newline), the signer name, the
// signature bytes, and the 4-byte key ID.
func parseSignedNote(envelope string) (noteText string, signerName string, sigBytes []byte, keyID []byte, err error) {
	// The note text and signature are separated by a blank line ("\n\n").
	separatorIndex := strings.Index(envelope, "\n\n")
	if separatorIndex < 0 {
		return "", "", nil, nil, errors.New("no blank line separator found")
	}

	// Note text includes the trailing newline before the separator.
	noteText = envelope[:separatorIndex+1]

	// Signature lines follow the blank line.
	sigSection := envelope[separatorIndex+2:]
	lines := strings.Split(strings.TrimRight(sigSection, "\n"), "\n")
	if len(lines) == 0 {
		return "", "", nil, nil, errors.New("no signature lines found")
	}

	// Parse the first signature line: "— <name> <base64(keyID || sig)>"
	sigLine := lines[0]
	// The em dash is U+2014 (3 bytes in UTF-8).
	if !strings.HasPrefix(sigLine, "\u2014 ") {
		return "", "", nil, nil, fmt.Errorf("signature line does not start with em dash: %q", sigLine)
	}

	// Split after "— " to get "<name> <base64>".
	afterDash := sigLine[len("\u2014 "):]
	spaceIndex := strings.LastIndex(afterDash, " ")
	if spaceIndex < 0 {
		return "", "", nil, nil, errors.New("signature line missing space between name and signature")
	}

	signerName = afterDash[:spaceIndex]
	sigB64 := afterDash[spaceIndex+1:]
	decoded, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return "", "", nil, nil, fmt.Errorf("decode signature base64: %w", err)
	}

	if len(decoded) < 5 {
		return "", "", nil, nil, fmt.Errorf("decoded signature too short: %d bytes", len(decoded))
	}

	keyID = decoded[:4]
	sigBytes = decoded[4:]

	return noteText, signerName, sigBytes, keyID, nil
}

// parseCheckpointBody extracts the tree size and root hash from the
// checkpoint note text. The format is:
//
//	<origin>\n
//	<tree size>\n
//	<base64 root hash>\n
func parseCheckpointBody(noteText string) (origin string, treeSize int64, rootHash []byte, err error) {
	lines := strings.Split(noteText, "\n")
	if len(lines) < 3 {
		return "", 0, nil, fmt.Errorf("checkpoint has %d lines, need at least 3", len(lines))
	}

	origin = lines[0]
	if origin == "" {
		return "", 0, nil, errors.New("checkpoint origin line is empty")
	}

	treeSize, err = strconv.ParseInt(lines[1], 10, 64)
	if err != nil {
		return "", 0, nil, fmt.Errorf("parse tree size %q: %w", lines[1], err)
	}

	rootHash, err = base64.StdEncoding.DecodeString(lines[2])
	if err != nil {
		return "", 0, nil, fmt.Errorf("decode root hash: %w", err)
	}

	return origin, treeSize, rootHash, nil
}

// computeCheckpointKeyID computes the 4-byte key ID for a public key
// as used in Rekor checkpoints: first 4 bytes of SHA-256(DER public key).
func computeCheckpointKeyID(key crypto.PublicKey) []byte {
	derBytes, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return nil
	}
	h := sha256.Sum256(derBytes)
	return h[:4]
}

// verifyCheckpointSignature verifies the ECDSA signature over the
// checkpoint note text.
func verifyCheckpointSignature(noteText string, sigBytes []byte, key crypto.PublicKey) error {
	ecdsaKey, ok := key.(*ecdsa.PublicKey)
	if !ok {
		return fmt.Errorf("unsupported checkpoint key type %T, want *ecdsa.PublicKey", key)
	}

	digest := sha256.Sum256([]byte(noteText))

	// Rekor checkpoint signatures use ECDSA. Real Rekor instances
	// produce ASN.1 DER-encoded signatures. The C2SP signed note
	// specification describes a fixed-size r||s concatenation format.
	// Accept both: try fixed-size first, fall back to DER.
	byteLength := (ecdsaKey.Curve.Params().BitSize + 7) / 8
	if len(sigBytes) != 2*byteLength {
		if ecdsa.VerifyASN1(ecdsaKey, digest[:], sigBytes) {
			return nil
		}
		return fmt.Errorf("checkpoint signature verification failed (tried fixed-size %d bytes and ASN.1 DER %d bytes)",
			2*byteLength, len(sigBytes))
	}

	r := new(big.Int).SetBytes(sigBytes[:byteLength])
	s := new(big.Int).SetBytes(sigBytes[byteLength:])
	if !ecdsa.Verify(ecdsaKey, digest[:], r, s) {
		return errors.New("checkpoint ECDSA signature verification failed")
	}

	return nil
}

// ---- OIDC claim extraction from X.509 extensions ----------------------

// fulcioOIDBase is the Sigstore private enterprise OID prefix.
var fulcioOIDBase = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1}

// oidFulcioOtherName is the OID for Fulcio's otherName SAN type,
// used for username-based identities.
var oidFulcioOtherName = append(append(asn1.ObjectIdentifier{}, fulcioOIDBase...), 7)

// Well-known Fulcio extension OIDs.
var (
	oidIssuerV1            = append(append(asn1.ObjectIdentifier{}, fulcioOIDBase...), 1)
	oidIssuerV2            = append(append(asn1.ObjectIdentifier{}, fulcioOIDBase...), 8)
	oidBuildSignerURI      = append(append(asn1.ObjectIdentifier{}, fulcioOIDBase...), 9)
	oidSourceRepositoryURI = append(append(asn1.ObjectIdentifier{}, fulcioOIDBase...), 12)
	oidSourceRepositoryRef = append(append(asn1.ObjectIdentifier{}, fulcioOIDBase...), 14)
	oidBuildConfigURI      = append(append(asn1.ObjectIdentifier{}, fulcioOIDBase...), 18)
	oidBuildTrigger        = append(append(asn1.ObjectIdentifier{}, fulcioOIDBase...), 20)
	oidRunInvocationURI    = append(append(asn1.ObjectIdentifier{}, fulcioOIDBase...), 21)
)

// handledCriticalOIDs lists the Fulcio extension OIDs that this
// package explicitly processes. Only these are removed from Go's
// UnhandledCriticalExtensions list — any other critical extension
// will cause certificate verification to fail, which is the correct
// safe default.
var handledCriticalOIDs = []asn1.ObjectIdentifier{
	oidIssuerV1,
	oidIssuerV2,
	oidBuildSignerURI,
	oidSourceRepositoryURI,
	oidSourceRepositoryRef,
	oidBuildConfigURI,
	oidBuildTrigger,
	oidRunInvocationURI,
	oidFulcioOtherName,
	// The SAN extension (2.5.29.17) ends up in UnhandledCriticalExtensions
	// when it contains an otherName entry that Go's x509 package doesn't
	// parse. We handle otherName SANs ourselves in extractOtherNameSAN.
	oidSubjectAltName,
}

// filterHandledCriticalExtensions removes the Fulcio-specific OIDs
// that we process from the unhandled critical extensions list,
// leaving any unknown critical extensions so that Go's x509.Verify
// will reject the certificate.
func filterHandledCriticalExtensions(unhandled []asn1.ObjectIdentifier) []asn1.ObjectIdentifier {
	var remaining []asn1.ObjectIdentifier
	for _, oid := range unhandled {
		handled := false
		for _, known := range handledCriticalOIDs {
			if oid.Equal(known) {
				handled = true
				break
			}
		}
		// No blanket prefix match: if Fulcio introduces a new
		// critical extension with verification-relevant semantics,
		// we must fail closed (reject the certificate) until we
		// explicitly add support for it. Silently accepting unknown
		// OIDs under the Fulcio prefix would bypass constraints.
		if !handled {
			remaining = append(remaining, oid)
		}
	}
	return remaining
}

// fulcioClaims holds OIDC claims extracted from a Fulcio certificate's
// X.509 extensions.
type fulcioClaims struct {
	Issuer                 string
	SubjectAlternativeName string
	BuildConfigURI         string
	SourceRepositoryURI    string
	SourceRepositoryRef    string
	BuildSignerURI         string
	BuildTrigger           string
	RunInvocationURI       string
}

// extractFulcioClaims extracts OIDC claims from a Fulcio-issued X.509
// certificate's extensions.
func extractFulcioClaims(cert *x509.Certificate) fulcioClaims {
	claims := fulcioClaims{}

	// Subject Alternative Name: URIs, emails, or otherName entries.
	// Go's x509 package parses URIs and emails but not otherName SANs
	// (Fulcio OID 1.3.6.1.4.1.57264.1.7), so we fall back to raw
	// SAN extension parsing for username-based identities. All paths
	// are sanitized for control characters to prevent log injection
	// via Result.Subject and to ensure identity matching can't be
	// confused by non-printable characters.
	var san string
	if len(cert.URIs) > 0 {
		san = cert.URIs[0].String()
	} else if len(cert.EmailAddresses) > 0 {
		san = cert.EmailAddresses[0]
	} else {
		san = extractOtherNameSAN(cert)
	}
	if !containsControlCharacters(san) {
		claims.SubjectAlternativeName = san
	}

	// Extract Fulcio-specific extensions.
	for _, ext := range cert.Extensions {
		value := extractExtensionString(ext)
		if value == "" {
			continue
		}

		switch {
		case ext.Id.Equal(oidIssuerV2):
			claims.Issuer = value
		case ext.Id.Equal(oidBuildConfigURI):
			claims.BuildConfigURI = value
		case ext.Id.Equal(oidSourceRepositoryURI):
			claims.SourceRepositoryURI = value
		case ext.Id.Equal(oidSourceRepositoryRef):
			claims.SourceRepositoryRef = value
		case ext.Id.Equal(oidBuildSignerURI):
			claims.BuildSignerURI = value
		case ext.Id.Equal(oidBuildTrigger):
			claims.BuildTrigger = value
		case ext.Id.Equal(oidRunInvocationURI):
			claims.RunInvocationURI = value
		}
	}

	// V1 issuer fallback: raw bytes interpreted as string (not
	// DER-encoded UTF8String like V2). Reject if it contains
	// control characters — V1 issuers are URLs and must be
	// printable. A control character here indicates either a
	// DER-encoded value misidentified as V1 or an injection.
	if claims.Issuer == "" {
		for _, ext := range cert.Extensions {
			if ext.Id.Equal(oidIssuerV1) {
				raw := string(ext.Value)
				if !containsControlCharacters(raw) {
					claims.Issuer = raw
				}
				break
			}
		}
	}

	return claims
}

// oidSubjectAltName is the OID for the Subject Alternative Name
// X.509 extension (2.5.29.17).
var oidSubjectAltName = asn1.ObjectIdentifier{2, 5, 29, 17}

// extractOtherNameSAN parses the raw SAN extension to extract an
// otherName entry. This handles the Fulcio otherName SAN type used
// for username-based identities (OID 1.3.6.1.4.1.57264.1.7), which
// Go's x509 package does not parse.
//
// The SAN extension contains a SEQUENCE of GeneralName values.
// otherName is tagged [0] and contains:
//
//	otherName [0] {
//	    type-id OBJECT IDENTIFIER,
//	    value   [0] EXPLICIT ANY
//	}
//
// Returns the first otherName value as a UTF8String, or empty string.
func extractOtherNameSAN(cert *x509.Certificate) string {
	for _, ext := range cert.Extensions {
		if !ext.Id.Equal(oidSubjectAltName) {
			continue
		}

		// Parse the outer SEQUENCE of GeneralName entries.
		var rawSANs asn1.RawValue
		rest, err := asn1.Unmarshal(ext.Value, &rawSANs)
		if err != nil || len(rest) > 0 {
			return ""
		}
		// rawSANs.FullBytes is the SEQUENCE; iterate over its elements.
		remaining := rawSANs.Bytes
		for len(remaining) > 0 {
			var generalName asn1.RawValue
			remaining, err = asn1.Unmarshal(remaining, &generalName)
			if err != nil {
				break
			}
			// otherName is context-specific tag 0, constructed.
			if generalName.Tag != 0 || generalName.Class != asn1.ClassContextSpecific || !generalName.IsCompound {
				continue
			}
			// Parse the otherName: type-id OID + value [0] EXPLICIT.
			var typeID asn1.ObjectIdentifier
			valueBytes, err := asn1.Unmarshal(generalName.Bytes, &typeID)
			if err != nil || len(valueBytes) == 0 {
				continue
			}
			// Only accept Fulcio's specific otherName OID for
			// username-based identities. Accepting arbitrary
			// otherName types would let a cert with an unrelated
			// otherName (e.g., hardware ID) impersonate identities.
			if !typeID.Equal(oidFulcioOtherName) {
				continue
			}
			// Extract the explicit [0] wrapper around the value.
			var explicitValue asn1.RawValue
			_, err = asn1.Unmarshal(valueBytes, &explicitValue)
			if err != nil {
				continue
			}
			// Parse the inner UTF8String.
			var value string
			_, err = asn1.Unmarshal(explicitValue.Bytes, &value)
			if err != nil {
				continue
			}
			return value
		}
	}
	return ""
}

// extractExtensionString extracts a UTF8String from a DER-encoded
// X.509 extension value (used for Fulcio v2+ extensions, OIDs 1.8+).
// Returns empty string if the value contains control characters —
// OIDC claims are printable text and control characters indicate
// either corruption or an injection attempt.
func extractExtensionString(ext pkix.Extension) string {
	var value string
	rest, err := asn1.Unmarshal(ext.Value, &value)
	if err != nil || len(rest) > 0 {
		return ""
	}
	if containsControlCharacters(value) {
		return ""
	}
	return value
}

// containsControlCharacters returns true if the string contains any
// control character or Unicode line separator. OIDC claims extracted
// from X.509 extensions must be printable text — control characters
// indicate corruption, DER/PEM encoding confusion, or an injection
// attempt (e.g., \r in a subject to confuse glob matching, or null
// bytes to truncate string comparisons in other languages).
//
// Rejected ranges:
//   - ASCII C0 controls (0x00-0x1F): NULL, TAB, LF, CR, ESC, etc.
//   - ASCII DEL (0x7F)
//   - NEL (U+0085): "Next Line" — behaves as newline in terminals
//   - LS (U+2028): Unicode "Line Separator"
//   - PS (U+2029): Unicode "Paragraph Separator"
//
// The Unicode line separators are particularly relevant because they
// act as line breaks in many terminals, log processors, and JSON
// parsers, enabling the same injection attacks as \n but bypassing
// ASCII-only filters.
func containsControlCharacters(value string) bool {
	for _, character := range value {
		if character <= 0x1F || character == 0x7F {
			return true
		}
		if character == 0x85 || character == 0x2028 || character == 0x2029 {
			return true
		}
	}
	return false
}

// ---- PEM parsing helpers ----------------------------------------------

// parsePEMCertificate parses a PEM-encoded X.509 certificate.
func parsePEMCertificate(pemData string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	return x509.ParseCertificate(block.Bytes)
}

// parsePEMPublicKey parses a PEM-encoded public key.
func parsePEMPublicKey(pemData string) (crypto.PublicKey, error) {
	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	return x509.ParsePKIXPublicKey(block.Bytes)
}

// ---- Hash function lookup ---------------------------------------------

// newHasher returns a hash.Hash for the given algorithm name.
func newHasher(algorithm string) (hash.Hash, error) {
	switch algorithm {
	case "sha256":
		return sha256.New(), nil
	case "sha384":
		return sha512.New384(), nil
	case "sha512":
		return sha512.New(), nil
	default:
		return nil, fmt.Errorf("unsupported hash algorithm %q", algorithm)
	}
}
