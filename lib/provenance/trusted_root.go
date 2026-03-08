// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package provenance

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

// ParseTrustedRootPEMs extracts PEM-encoded trust material from a
// Sigstore trusted_root.json file (the format distributed via TUF by
// the Sigstore public good instance and used by sigstore-go test
// fixtures).
//
// Returns the Fulcio root CA certificate(s) as concatenated PEM blocks
// and the Rekor transparency log public key as a single PEM block.
// Only self-signed root CA certificates are included in the Fulcio
// output — intermediate CAs are excluded because they are not trust
// anchors.
//
// This is the bridge between Sigstore's TUF distribution format
// (base64-encoded DER in JSON) and Bureau's schema format (PEM strings
// in Matrix state events). The CLI uses this when an operator runs:
//
//	bureau fleet provenance roots set --sigstore-trusted-root trusted_root.json
func ParseTrustedRootPEMs(data []byte) (fulcioRootPEM string, rekorPublicKeyPEM string, err error) {
	var raw rawTrustedRoot
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", "", fmt.Errorf("unmarshal trusted root JSON: %w", err)
	}

	// Extract self-signed Fulcio root CA certificates. Intermediates
	// are excluded — they are not trust anchors and storing them as
	// roots would bypass path length and name constraints.
	var fulcioPEMBlocks strings.Builder
	rootCount := 0
	for certificateAuthorityIndex, certificateAuthority := range raw.CertificateAuthorities {
		for certificateIndex, certificateEntry := range certificateAuthority.CertChain.Certificates {
			certificateDER, decodeError := base64.StdEncoding.DecodeString(certificateEntry.RawBytes)
			if decodeError != nil {
				return "", "", fmt.Errorf("CA[%d] cert[%d]: decode base64: %w",
					certificateAuthorityIndex, certificateIndex, decodeError)
			}
			certificate, parseError := x509.ParseCertificate(certificateDER)
			if parseError != nil {
				return "", "", fmt.Errorf("CA[%d] cert[%d]: parse x509: %w",
					certificateAuthorityIndex, certificateIndex, parseError)
			}
			if !certificate.IsCA {
				continue
			}
			if !isSelfSigned(certificate) {
				continue
			}
			fulcioPEMBlocks.Write(pem.EncodeToMemory(&pem.Block{
				Type:  "CERTIFICATE",
				Bytes: certificateDER,
			}))
			rootCount++
		}
	}
	if rootCount == 0 {
		return "", "", errors.New("trusted root has no self-signed Fulcio root CA certificates")
	}

	// Extract Rekor transparency log public key.
	if len(raw.Tlogs) == 0 {
		return "", "", errors.New("trusted root has no transparency logs")
	}
	rekorKeyDER, decodeError := base64.StdEncoding.DecodeString(raw.Tlogs[0].PublicKey.RawBytes)
	if decodeError != nil {
		return "", "", fmt.Errorf("decode Rekor public key: %w", decodeError)
	}
	// Validate the key parses as PKIX before returning PEM.
	if _, parseError := x509.ParsePKIXPublicKey(rekorKeyDER); parseError != nil {
		return "", "", fmt.Errorf("parse Rekor public key: %w", parseError)
	}
	rekorPEM := string(pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: rekorKeyDER,
	}))

	return fulcioPEMBlocks.String(), rekorPEM, nil
}
