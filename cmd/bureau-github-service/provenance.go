// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/github"
	"github.com/bureau-foundation/bureau/lib/provenance"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// provenanceEnforcementCategory is the enforcement category for forge
// artifacts downloaded via the GitHub service. Operators configure
// this in the fleet's m.bureau.provenance_policy enforcement map.
const provenanceEnforcementCategory = "forge_artifacts"

// syncProvenance reads provenance trust roots and policy from the fleet
// room and constructs a Verifier. Called during initial startup (before
// the sync loop) and when fleet room state changes are detected.
//
// The verifier is only cleared when BOTH state events are absent (404
// from the homeserver) — the legitimate "no provenance configured"
// state. All other error paths (transient Matrix errors, malformed
// state events, NewVerifier failures) retain the previous verifier.
// This prevents an attacker from disabling verification by inducing
// transient errors or publishing malformed state events.
func (gs *GitHubService) syncProvenance(ctx context.Context) error {
	roots, rootsError := messaging.GetState[schema.ProvenanceRootsContent](
		ctx, gs.session, gs.fleetRoomID, schema.EventTypeProvenanceRoots, "")
	rootsNotFound := rootsError != nil && messaging.IsMatrixError(rootsError, messaging.ErrCodeNotFound)

	policy, policyError := messaging.GetState[schema.ProvenancePolicyContent](
		ctx, gs.session, gs.fleetRoomID, schema.EventTypeProvenancePolicy, "")
	policyNotFound := policyError != nil && messaging.IsMatrixError(policyError, messaging.ErrCodeNotFound)

	// No provenance configured: both roots and policy are absent from
	// the fleet room. This is the only path that clears the verifier.
	if rootsNotFound && policyNotFound {
		if gs.provenanceVerifier.Load() != nil {
			gs.logger.Info("provenance configuration removed — verifier cleared")
		}
		gs.provenanceVerifier.Store(nil)
		return nil
	}

	// Transient errors reading roots or policy (network partition,
	// homeserver restart, etc.). Retain the previous verifier so
	// verification cannot be disabled by transient faults. Returns
	// the error so the caller can decide how to handle it — at
	// startup, this prevents the service from starting with an
	// unknown security posture; at runtime, the sync loop retries
	// on the next /sync response.
	if rootsError != nil && !rootsNotFound {
		gs.logger.Error("reading provenance roots from fleet room — retaining previous verifier",
			"error", rootsError,
			"has_previous_verifier", gs.provenanceVerifier.Load() != nil,
		)
		return fmt.Errorf("reading provenance roots: %w", rootsError)
	}
	if policyError != nil && !policyNotFound {
		gs.logger.Error("reading provenance policy from fleet room — retaining previous verifier",
			"error", policyError,
			"has_previous_verifier", gs.provenanceVerifier.Load() != nil,
		)
		return fmt.Errorf("reading provenance policy: %w", policyError)
	}

	// Partial configuration: one of roots or policy exists but not
	// the other. This can happen during initial setup (admin publishes
	// roots before policy) or if an attacker deletes one event.
	// Retain the previous verifier rather than silently disabling
	// verification. Not a transient error — the homeserver responded
	// successfully, the state is just incomplete.
	if rootsNotFound {
		gs.logger.Warn("provenance policy exists but roots are missing — retaining previous verifier")
		return nil
	}
	if policyNotFound {
		gs.logger.Warn("provenance roots exist but policy is missing — retaining previous verifier")
		return nil
	}

	verifier, err := provenance.NewVerifier(roots, policy)
	if err != nil {
		// Configuration is invalid (empty roots, dangling root set
		// reference, unknown enforcement level). Retain the previous
		// verifier rather than silently disabling verification. Not
		// a transient error — the config is genuinely malformed.
		gs.logger.Error("creating provenance verifier from fleet state — retaining previous verifier",
			"error", err,
			"has_previous_verifier", gs.provenanceVerifier.Load() != nil,
		)
		return nil
	}

	gs.provenanceVerifier.Store(verifier)
	gs.logger.Info("provenance verifier configured",
		"root_sets", len(roots.Roots),
		"trusted_identities", len(policy.TrustedIdentities),
		"forge_artifacts_enforcement", verifier.Enforcement(provenanceEnforcementCategory),
	)
	return nil
}

// provenanceResult is the provenance verification outcome included in
// download responses. Gives agents visibility into verification status
// without exposing the raw Sigstore bundle.
type provenanceResult struct {
	// Status is the verification outcome: "verified", "unverified",
	// or "rejected".
	Status string `json:"status"`

	// Identity is the name of the matched TrustedIdentity from the
	// provenance policy. Empty when status is not "verified".
	Identity string `json:"identity,omitempty"`

	// Issuer is the OIDC issuer URL from the Fulcio certificate.
	Issuer string `json:"issuer,omitempty"`

	// Subject is the Subject Alternative Name from the Fulcio
	// certificate (e.g., "repo:owner/repo:ref:refs/heads/main").
	Subject string `json:"subject,omitempty"`

	// IntegratedTime is the Rekor transparency log entry timestamp
	// as Unix seconds. Zero when not available.
	IntegratedTime int64 `json:"integrated_time,omitempty"`

	// Enforcement is the configured enforcement level for this
	// artifact category.
	Enforcement string `json:"enforcement"`

	// Error describes the verification failure. Empty on success.
	Error string `json:"error,omitempty"`
}

// verifyArtifactProvenance checks attestation bundles for a downloaded
// artifact against the fleet's provenance policy. Returns the
// verification result and whether the download should proceed (based
// on enforcement level).
//
// When the verifier is nil (no provenance policy configured), returns
// (nil, true) — the download proceeds without provenance metadata.
//
// The label parameter is used for log messages and identifies the
// artifact being verified (e.g., "owner/repo@v1.2.3/binary.tar.gz").
func (gs *GitHubService) verifyArtifactProvenance(
	ctx context.Context,
	client *github.Client,
	owner, repo string,
	artifactDigest []byte,
	label string,
) (*provenanceResult, bool) {
	verifier := gs.provenanceVerifier.Load()
	if verifier == nil {
		return nil, true
	}

	enforcement := verifier.Enforcement(provenanceEnforcementCategory)

	// Fetch attestation bundles from GitHub's attestation API.
	subjectDigest := github.FormatSubjectDigest("sha256", artifactDigest)
	attestations, err := client.GetAttestations(ctx, owner, repo, subjectDigest)
	if err != nil {
		gs.logger.Error("fetching attestation bundles from GitHub",
			"label", label,
			"error", err,
			"enforcement", enforcement,
		)
		result := &provenanceResult{
			Status:      "unverified",
			Enforcement: string(enforcement),
			Error:       fmt.Sprintf("fetching attestations: %v", err),
		}
		if enforcement == schema.EnforcementRequire {
			return result, false
		}
		return result, true
	}

	// No attestation bundles available for this artifact.
	if len(attestations.Attestations) == 0 {
		gs.logger.Warn("no attestation bundles for artifact",
			"label", label,
			"digest", subjectDigest,
			"enforcement", enforcement,
		)
		result := &provenanceResult{
			Status:      "unverified",
			Enforcement: string(enforcement),
			Error:       "no attestation bundles found",
		}
		if enforcement == schema.EnforcementRequire {
			return result, false
		}
		return result, true
	}

	// Try each attestation bundle. A single verified bundle is
	// sufficient — the artifact may have multiple attestations from
	// different signers or re-releases.
	var lastResult provenance.Result
	for _, entry := range attestations.Attestations {
		lastResult = verifier.Verify(entry.Bundle, "sha256", artifactDigest)
		if lastResult.Status == provenance.StatusVerified {
			gs.logger.Info("provenance verified",
				"label", label,
				"identity", lastResult.Identity,
				"issuer", lastResult.Issuer,
				"subject", lastResult.Subject,
				"integrated_time", lastResult.IntegratedTime,
			)
			return &provenanceResult{
				Status:         "verified",
				Identity:       lastResult.Identity,
				Issuer:         lastResult.Issuer,
				Subject:        lastResult.Subject,
				IntegratedTime: lastResult.IntegratedTime,
				Enforcement:    string(enforcement),
			}, true
		}
	}

	// All bundles were rejected. Use the last rejection result for
	// the error message.
	errorMessage := "verification failed"
	if lastResult.Error != nil {
		errorMessage = lastResult.Error.Error()
	}

	gs.logger.Error("provenance verification failed for all attestation bundles",
		"label", label,
		"bundles_tried", len(attestations.Attestations),
		"last_error", errorMessage,
		"enforcement", enforcement,
	)

	result := &provenanceResult{
		Status:      "rejected",
		Issuer:      lastResult.Issuer,
		Subject:     lastResult.Subject,
		Enforcement: string(enforcement),
		Error:       errorMessage,
	}
	if enforcement == schema.EnforcementRequire {
		return result, false
	}
	return result, true
}

// computeSHA256 returns the SHA-256 digest of the given data.
func computeSHA256(data []byte) []byte {
	digest := sha256.Sum256(data)
	return digest[:]
}
