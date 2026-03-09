// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"encoding/hex"
	"fmt"
)

// GetAttestations retrieves Sigstore attestation bundles for an
// artifact identified by its content digest. The subjectDigest
// parameter must be algorithm-prefixed (e.g., "sha256:abcdef01...").
//
// GitHub's attestation API returns all bundles associated with the
// given artifact digest in the specified repository. Each bundle is
// a complete Sigstore bundle (JSON) that can be passed directly to
// provenance.Verifier.Verify.
//
// Returns an empty response (not an error) when no attestations exist
// for the digest. Returns an *APIError on HTTP failures.
func (client *Client) GetAttestations(ctx context.Context, owner, repo, subjectDigest string) (*AttestationResponse, error) {
	var response AttestationResponse
	path := fmt.Sprintf("/repos/%s/%s/attestations/%s", owner, repo, subjectDigest)
	if err := client.get(ctx, path, &response); err != nil {
		return nil, fmt.Errorf("getting attestations for %s in %s/%s: %w", subjectDigest, owner, repo, err)
	}
	return &response, nil
}

// FormatSubjectDigest constructs the algorithm-prefixed digest string
// expected by GitHub's attestation API from a digest algorithm name
// and raw digest bytes. Example: FormatSubjectDigest("sha256", digest)
// returns "sha256:abcdef01...".
func FormatSubjectDigest(algorithm string, digest []byte) string {
	return fmt.Sprintf("%s:%s", algorithm, hex.EncodeToString(digest))
}
