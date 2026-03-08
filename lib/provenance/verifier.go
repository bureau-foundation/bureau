// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package provenance

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/bureau-foundation/bureau/lib/schema"
)

// Status represents the outcome of provenance verification.
type Status int

const (
	// StatusVerified means the bundle was cryptographically valid and
	// matched a trusted identity from the fleet's provenance policy.
	StatusVerified Status = iota

	// StatusUnverified means no provenance bundle was available for
	// the artifact. The caller should consult the enforcement level
	// to decide whether to accept, warn, or reject.
	StatusUnverified

	// StatusRejected means verification was attempted and failed:
	// invalid signature, untrusted root, expired certificate, or
	// no matching identity. The Error field describes the failure.
	StatusRejected
)

// String returns a human-readable status label.
func (s Status) String() string {
	switch s {
	case StatusVerified:
		return "verified"
	case StatusUnverified:
		return "unverified"
	case StatusRejected:
		return "rejected"
	default:
		return fmt.Sprintf("Status(%d)", int(s))
	}
}

// Result is the outcome of verifying a Sigstore provenance bundle.
type Result struct {
	// Status is the verification outcome.
	Status Status

	// Identity is the name of the matched TrustedIdentity from the
	// provenance policy. Empty when Status is not StatusVerified.
	Identity string

	// Issuer is the OIDC issuer URL from the Fulcio certificate's
	// extensions. Populated when verification reaches the identity
	// matching phase (even if no identity matched).
	Issuer string

	// Subject is the Subject Alternative Name from the Fulcio
	// certificate. For GitHub Actions, this has the form
	// "repo:<owner>/<repo>:ref:<ref>".
	Subject string

	// IntegratedTime is the Rekor transparency log entry timestamp
	// as Unix seconds. Zero when the bundle has no tlog entry or
	// verification did not reach the timestamp extraction phase.
	IntegratedTime int64

	// Error describes the verification failure. Non-nil only when
	// Status is StatusRejected.
	Error error
}

// rootSet holds a parsed trust root and the trusted identities that
// reference it.
type rootSet struct {
	root       *parsedRoot
	identities []schema.TrustedIdentity
}

// Verifier verifies Sigstore provenance bundles against fleet trust
// roots and policy. Constructed from the fleet's provenance_roots and
// provenance_policy Matrix state events via NewVerifier.
//
// All verification is offline — no network calls. The bundle carries
// the tlog entry (with inclusion proof and integrated timestamp), the
// Fulcio leaf certificate, and optionally RFC3161 timestamps. The
// trust roots carry the Fulcio root CA and Rekor public key.
type Verifier struct {
	roots  map[string]*rootSet
	policy schema.ProvenancePolicyContent
}

// NewVerifier constructs a Verifier from the fleet's trust roots and
// policy. Returns an error if any root set's PEM material is invalid
// or if a trusted identity references a nonexistent root set.
func NewVerifier(roots schema.ProvenanceRootsContent, policy schema.ProvenancePolicyContent) (*Verifier, error) {
	if len(roots.Roots) == 0 {
		return nil, errors.New("provenance roots: no root sets defined")
	}

	// Parse each named root set from PEM into verified certificates
	// and public keys.
	rootSets := make(map[string]*rootSet, len(roots.Roots))
	for name, trustRoot := range roots.Roots {
		parsed, err := parseTrustRoot(trustRoot)
		if err != nil {
			return nil, fmt.Errorf("root set %q: %w", name, err)
		}
		rootSets[name] = &rootSet{root: parsed}
	}

	// Attach each trusted identity to its declared root set.
	for _, identity := range policy.TrustedIdentities {
		rs, ok := rootSets[identity.Roots]
		if !ok {
			return nil, fmt.Errorf("trusted identity %q references unknown root set %q", identity.Name, identity.Roots)
		}
		rs.identities = append(rs.identities, identity)
	}

	// Validate enforcement levels. Unknown values (typos, empty
	// strings, attacker-crafted strings) would silently default to
	// EnforcementLog at lookup time, effectively disabling blocking
	// verification. Reject them at construction time instead.
	for category, level := range policy.Enforcement {
		if !level.IsKnown() {
			return nil, fmt.Errorf("enforcement category %q has unknown level %q (valid: require, warn, log)", category, level)
		}
	}

	return &Verifier{
		roots:  rootSets,
		policy: policy,
	}, nil
}

// Enforcement returns the enforcement level for the given artifact
// category. Returns EnforcementLog for categories not present in
// the policy — the default is permissive to avoid breaking ingestion
// for newly introduced categories before the operator updates policy.
func (v *Verifier) Enforcement(category string) schema.EnforcementLevel {
	level, ok := v.policy.Enforcement[category]
	if !ok {
		return schema.EnforcementLog
	}
	return level
}

// Verify verifies a Sigstore bundle against the configured trust roots
// and policy. The digestAlgorithm and artifactDigest must match the
// digest that was signed (typically "sha256" for Nix store paths).
//
// Returns StatusVerified if the bundle is cryptographically valid and
// matches a trusted identity. Returns StatusRejected with a
// descriptive error if verification fails.
//
// Each root set is tried independently: a bundle must both verify
// cryptographically against a root set's trust material AND match at
// least one trusted identity referencing that root set. This prevents
// cross-root-set identity confusion.
func (v *Verifier) Verify(bundleBytes []byte, digestAlgorithm string, artifactDigest []byte) Result {
	bundle, err := parseBundle(bundleBytes)
	if err != nil {
		return Result{
			Status: StatusRejected,
			Error:  fmt.Errorf("parse bundle: %w", err),
		}
	}

	// Verify the artifact digest matches the bundle's declared digest.
	if bundle.isDSSE {
		// DSSE bundles carry artifact digests inside the in-toto
		// statement's subject list. At least one subject must have
		// a digest matching the expected algorithm and value.
		matched := false
		for _, subject := range bundle.dsseSubjects {
			if subject.matchesDigest(digestAlgorithm, artifactDigest) {
				matched = true
				break
			}
		}
		if !matched {
			return Result{
				Status: StatusRejected,
				Error: fmt.Errorf("no in-toto subject matches expected %s digest",
					digestAlgorithm),
			}
		}
	} else {
		// messageSignature bundles carry the digest directly.
		if bundle.digestAlgorithm != digestAlgorithm {
			return Result{
				Status: StatusRejected,
				Error: fmt.Errorf("bundle digest algorithm %q does not match expected %q",
					bundle.digestAlgorithm, digestAlgorithm),
			}
		}
		if !constantTimeEqual(bundle.digestValue, artifactDigest) {
			return Result{
				Status: StatusRejected,
				Error:  errors.New("bundle artifact digest does not match expected digest"),
			}
		}
	}

	// Try each root set independently. Collect errors for diagnostics.
	var lastError error
	for rootName, rs := range v.roots {
		if len(rs.identities) == 0 {
			continue
		}

		// Phase 1: verify certificate chain — leaf cert must chain
		// to one of this root set's Fulcio CA certificates.
		if err := verifyCertificateChain(bundle.leafCert, rs.root); err != nil {
			lastError = fmt.Errorf("root set %q: %w", rootName, err)
			continue
		}

		// Phase 2: verify the signature over the artifact.
		if err := verifySignature(bundle, artifactDigest); err != nil {
			lastError = fmt.Errorf("root set %q: %w", rootName, err)
			continue
		}

		// Phase 3: verify the transparency log entry.
		if err := verifyTlogEntry(&bundle.tlogEntry, rs.root.rekorKey, rs.root.rekorKeyID, bundle); err != nil {
			lastError = fmt.Errorf("root set %q: %w", rootName, err)
			continue
		}

		// Phase 4: extract OIDC claims from the verified certificate.
		claims := extractFulcioClaims(bundle.leafCert)
		integratedTime := bundle.tlogEntry.integratedTime

		// Phase 5: match against trusted identities for this root set.
		for _, identity := range rs.identities {
			if matchIdentity(identity, claims.Issuer, claims.SubjectAlternativeName, claims.BuildConfigURI) {
				return Result{
					Status:         StatusVerified,
					Identity:       identity.Name,
					Issuer:         claims.Issuer,
					Subject:        claims.SubjectAlternativeName,
					IntegratedTime: integratedTime,
				}
			}
		}

		lastError = fmt.Errorf("root set %q: verified but no identity matched (issuer=%q, subject=%q)",
			rootName, claims.Issuer, claims.SubjectAlternativeName)
	}

	if lastError == nil {
		lastError = errors.New("no root sets with trusted identities configured")
	}
	return Result{
		Status: StatusRejected,
		Error:  lastError,
	}
}

// matchIdentity checks whether OIDC claims from a verified Fulcio
// certificate match a trusted identity's patterns. The issuer must
// match exactly. SubjectPattern and WorkflowPattern use glob matching
// where '*' matches any sequence of characters (including '/') and
// '?' matches any single character.
func matchIdentity(identity schema.TrustedIdentity, issuer, subject, buildConfigURI string) bool {
	if identity.Issuer != issuer {
		return false
	}

	if !matchGlob(identity.SubjectPattern, subject) {
		return false
	}

	if identity.WorkflowPattern != "" {
		if !matchGlob(identity.WorkflowPattern, buildConfigURI) {
			return false
		}
	}

	return true
}

// matchGlob performs glob matching where '*' matches any sequence of
// characters (including path separators) and '?' matches any single
// character. This differs from path.Match which restricts '*' to
// non-separator characters — our patterns need to match URIs and
// colon-separated identifiers where '/' and ':' are common.
func matchGlob(pattern, value string) bool {
	re, err := globToRegexp(pattern)
	if err != nil {
		// Invalid pattern never matches. The operator will see
		// "no identity matched" errors that surface the problem.
		return false
	}
	return re.MatchString(value)
}

// globToRegexp converts a glob pattern to a regexp. '*' becomes a
// match for any sequence of printable characters, '?' becomes a match
// for any single printable character, and all other regex
// metacharacters are escaped.
//
// The character class excludes all control characters and Unicode line
// separators: ASCII C0 (0x00-0x1F), DEL (0x7F), NEL (U+0085), LS
// (U+2028), and PS (U+2029). This prevents injection attacks where
// control characters or Unicode line breaks in a subject could match
// wildcards, confusing log analysis or terminal output.
func globToRegexp(pattern string) (*regexp.Regexp, error) {
	var builder strings.Builder
	builder.WriteString("^")
	for _, character := range pattern {
		switch character {
		case '*':
			builder.WriteString("[^\\x00-\\x1f\\x7f\\x{0085}\\x{2028}\\x{2029}]*")
		case '?':
			builder.WriteString("[^\\x00-\\x1f\\x7f\\x{0085}\\x{2028}\\x{2029}]")
		case '.', '(', ')', '+', '|', '^', '$', '[', ']', '{', '}', '\\':
			builder.WriteByte('\\')
			builder.WriteRune(character)
		default:
			builder.WriteRune(character)
		}
	}
	builder.WriteString("$")
	return regexp.Compile(builder.String())
}

// parseTrustRoot converts a schema.ProvenanceTrustRoot (PEM strings)
// into a parsedRoot for offline verification.
//
// The Fulcio root PEM may contain multiple concatenated CERTIFICATE
// blocks (e.g., when a trust root includes multiple CA certificates
// from different validity periods or key rotations). All blocks are
// parsed and added to the root pool.
func parseTrustRoot(trustRoot schema.ProvenanceTrustRoot) (*parsedRoot, error) {
	// Parse Fulcio CA certificates from the PEM bundle. The PEM may
	// contain the complete chain (intermediate + root) as provided by
	// Sigstore's trusted_root.json. Separate self-signed root CAs
	// (trust anchors) from intermediates. Non-CA certificates are
	// rejected — only CA certificates belong in the trust root.
	var fulcioRoots []*x509.Certificate
	var fulcioIntermediates []*x509.Certificate
	remaining := []byte(trustRoot.FulcioRootPEM)
	certIndex := 0
	for {
		var block *pem.Block
		block, remaining = pem.Decode(remaining)
		if block == nil {
			break
		}
		certIndex++
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("unexpected PEM block type %q in Fulcio root PEM (expected CERTIFICATE)", block.Type)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse Fulcio certificate #%d: %w", certIndex, err)
		}
		if !cert.IsCA {
			return nil, fmt.Errorf("Fulcio certificate #%d (%q) is not a CA", certIndex, cert.Subject)
		}
		if isSelfSigned(cert) {
			fulcioRoots = append(fulcioRoots, cert)
		} else {
			fulcioIntermediates = append(fulcioIntermediates, cert)
		}
	}
	if len(fulcioRoots) == 0 {
		return nil, errors.New("failed to decode Fulcio root PEM: no self-signed root CA found")
	}

	// Parse Rekor transparency log public key.
	rekorBlock, _ := pem.Decode([]byte(trustRoot.RekorPublicKeyPEM))
	if rekorBlock == nil {
		return nil, errors.New("failed to decode Rekor public key PEM: no PEM block found")
	}
	rekorPubKey, err := x509.ParsePKIXPublicKey(rekorBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse Rekor public key: %w", err)
	}

	// Compute log ID: SHA-256 of the DER-encoded public key bytes.
	logIDHash := sha256.Sum256(rekorBlock.Bytes)

	return &parsedRoot{
		fulcioRoots:         fulcioRoots,
		fulcioIntermediates: fulcioIntermediates,
		rekorKey:            rekorPubKey,
		rekorKeyID:          logIDHash[:],
	}, nil
}

// parseTrustedRootFile parses a Sigstore trusted root JSON file (the
// format used by sigstore-go test fixtures and the public good
// instance) into a parsedRoot. This is used for testing with real
// Sigstore fixture files.
func parseTrustedRootFile(data []byte) (*parsedRoot, error) {
	return parseTrustedRootJSON(data)
}
