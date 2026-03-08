// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/provenance"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

type verifyParams struct {
	cli.SessionConfig
	cli.JSONOutput
	Digest          string `json:"digest"           flag:"digest"           desc:"hex-encoded digest of the artifact (SHA-256 of the NAR for Nix store paths)" required:"true"`
	DigestAlgorithm string `json:"digest_algorithm" flag:"digest-algorithm" desc:"digest algorithm (default: sha256)" default:"sha256"`
	CacheURL        string `json:"cache_url"        flag:"cache-url"        desc:"override the fleet's binary cache URL for bundle fetching"`
}

type verifyResult struct {
	Fleet          ref.Fleet `json:"fleet"                    desc:"fleet reference"`
	StorePath      string    `json:"store_path"               desc:"store path basename"`
	Status         string    `json:"status"                   desc:"verification status: verified, unverified, or rejected"`
	Identity       string    `json:"identity,omitempty"       desc:"matched trusted identity name (when verified)"`
	Issuer         string    `json:"issuer,omitempty"         desc:"OIDC issuer from certificate"`
	Subject        string    `json:"subject,omitempty"        desc:"OIDC subject from certificate"`
	IntegratedTime int64     `json:"integrated_time,omitempty" desc:"Rekor transparency log timestamp (Unix seconds)"`
	Enforcement    string    `json:"enforcement"              desc:"enforcement level for nix_store_paths category"`
	Error          string    `json:"error,omitempty"          desc:"verification error message (when rejected)"`
}

func verifyCommand() *cli.Command {
	var params verifyParams

	return &cli.Command{
		Name:    "verify",
		Summary: "Verify provenance of a store path against fleet trust roots and policy",
		Description: `One-shot provenance verification of a Nix store path.

Fetches the Sigstore bundle from the fleet's binary cache, verifies
it against the fleet's trust roots and policy, and prints the result.
Useful for debugging provenance verification and smoke-testing the
trust root and policy configuration.

The store path argument can be either a full path
(/nix/store/xyz-bureau-daemon) or just the basename
(xyz-bureau-daemon). The --digest flag is required and should contain
the hex-encoded SHA-256 of the store path's NAR serialization
(nix-store --dump <path> | sha256sum).

Exit code is non-zero when verification is rejected (StatusRejected).
Unverified status (no bundle found) exits zero — the enforcement
level determines whether that is acceptable.`,
		Usage: "bureau fleet provenance verify <namespace/fleet/name> <store-path> [flags]",
		Examples: []cli.Example{
			{
				Description: "Verify a store path",
				Command:     "bureau fleet provenance verify bureau/fleet/prod xyz-bureau-daemon --digest abc123... --credential-file ./creds",
			},
			{
				Description: "Verify with a custom cache URL",
				Command:     "bureau fleet provenance verify bureau/fleet/prod xyz-bureau-daemon --digest abc123... --cache-url https://cache.example.com --credential-file ./creds",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &verifyResult{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/fleet/provenance/verify"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) < 2 {
				return cli.Validation("fleet localpart and store path are required\n\nusage: bureau fleet provenance verify <fleet> <store-path> --digest <hex>")
			}
			if len(args) > 2 {
				return cli.Validation("expected two arguments (fleet localpart, store path), got %d", len(args))
			}
			return runVerify(ctx, logger, args[0], args[1], &params)
		},
	}
}

func runVerify(ctx context.Context, logger *slog.Logger, fleetLocalpart string, storePathArg string, params *verifyParams) error {
	if params.Digest == "" {
		return cli.Validation("--digest is required (hex-encoded SHA-256 of the NAR)").
			WithHint("Compute it with: nix-store --dump /nix/store/<basename> | sha256sum")
	}

	// Decode the digest.
	artifactDigest, err := hex.DecodeString(params.Digest)
	if err != nil {
		return cli.Validation("invalid --digest: %v (expected hex-encoded hash)", err)
	}

	// Normalize store path: strip /nix/store/ prefix if present.
	basename := storePathArg
	if strings.HasPrefix(basename, "/nix/store/") {
		basename = basename[len("/nix/store/"):]
	}
	if basename == "" {
		return cli.Validation("store path basename is empty")
	}

	// Connect and resolve fleet room.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	session, err := params.SessionConfig.Connect(ctx)
	if err != nil {
		return err
	}
	defer session.Close()

	fleet, fleetRoomID, err := resolveFleetRoom(ctx, session, fleetLocalpart)
	if err != nil {
		return err
	}

	// Read trust roots and policy.
	roots, err := messaging.GetState[schema.ProvenanceRootsContent](ctx, session, fleetRoomID, schema.EventTypeProvenanceRoots, "")
	if err != nil && !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
		return cli.Transient("reading provenance roots from fleet room: %w", err)
	}
	if len(roots.Roots) == 0 {
		return cli.NotFound("no provenance trust roots configured for fleet %s", fleet.Localpart()).
			WithHint("Run 'bureau fleet provenance roots set' to configure trust roots.")
	}

	policy, err := messaging.GetState[schema.ProvenancePolicyContent](ctx, session, fleetRoomID, schema.EventTypeProvenancePolicy, "")
	if err != nil && !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
		return cli.Transient("reading provenance policy from fleet room: %w", err)
	}
	if len(policy.TrustedIdentities) == 0 {
		return cli.NotFound("no provenance policy configured for fleet %s", fleet.Localpart()).
			WithHint("Run 'bureau fleet provenance policy set' to configure policy.")
	}

	// Construct verifier.
	verifier, err := provenance.NewVerifier(roots, policy)
	if err != nil {
		return cli.Internal("constructing verifier: %v", err)
	}

	// Determine the cache URL.
	cacheURL := params.CacheURL
	if cacheURL == "" {
		cacheConfig, err := messaging.GetState[schema.FleetCacheContent](ctx, session, fleetRoomID, schema.EventTypeFleetCache, "")
		if err != nil && !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			return cli.Transient("reading fleet cache config: %w", err)
		}
		if cacheConfig.URL == "" {
			return cli.NotFound("no binary cache configured for fleet %s and no --cache-url provided", fleet.Localpart()).
				WithHint("Run 'bureau fleet cache' to configure a binary cache, or pass --cache-url.")
		}
		cacheURL = cacheConfig.URL
	}

	// Fetch the bundle.
	enforcement := verifier.Enforcement("nix_store_paths")

	bundleBytes, err := provenance.FetchBundle(http.DefaultClient, cacheURL, basename)
	if err != nil {
		if errors.Is(err, provenance.ErrNoBundleFound) {
			result := verifyResult{
				Fleet:       fleet,
				StorePath:   basename,
				Status:      "unverified",
				Enforcement: string(enforcement),
			}
			if done, emitError := params.EmitJSON(result); done {
				return emitError
			}
			fmt.Fprintf(os.Stdout, "Status: unverified (no bundle found)\n")
			fmt.Fprintf(os.Stdout, "Store Path: %s\n", basename)
			fmt.Fprintf(os.Stdout, "Enforcement: %s\n", enforcement)
			return nil
		}
		return cli.Transient("fetching provenance bundle: %v", err)
	}

	// Verify.
	verifyResult := verifier.Verify(bundleBytes, params.DigestAlgorithm, artifactDigest)

	result := makeVerifyResult(fleet, basename, verifyResult, enforcement)

	if done, emitError := params.EmitJSON(result); done {
		if emitError != nil {
			return emitError
		}
		if verifyResult.Status == provenance.StatusRejected {
			// Non-zero exit for rejected bundles, even in JSON mode.
			return cli.Validation("verification rejected: %v", verifyResult.Error)
		}
		return nil
	}

	// Text output.
	fmt.Fprintf(os.Stdout, "Status: %s\n", result.Status)
	fmt.Fprintf(os.Stdout, "Store Path: %s\n", basename)
	if result.Identity != "" {
		fmt.Fprintf(os.Stdout, "Identity: %s\n", result.Identity)
	}
	if result.Issuer != "" {
		fmt.Fprintf(os.Stdout, "Issuer: %s\n", result.Issuer)
	}
	if result.Subject != "" {
		fmt.Fprintf(os.Stdout, "Subject: %s\n", result.Subject)
	}
	if result.IntegratedTime != 0 {
		fmt.Fprintf(os.Stdout, "Integrated Time: %s (%d)\n",
			time.Unix(result.IntegratedTime, 0).UTC().Format(time.RFC3339), result.IntegratedTime)
	}
	fmt.Fprintf(os.Stdout, "Enforcement: %s\n", result.Enforcement)
	if result.Error != "" {
		fmt.Fprintf(os.Stdout, "Error: %s\n", result.Error)
	}

	if verifyResult.Status == provenance.StatusRejected {
		return cli.Validation("verification rejected: %v", verifyResult.Error)
	}
	return nil
}

func makeVerifyResult(fleet ref.Fleet, basename string, result provenance.Result, enforcement schema.EnforcementLevel) verifyResult {
	output := verifyResult{
		Fleet:          fleet,
		StorePath:      basename,
		Status:         result.Status.String(),
		Identity:       result.Identity,
		Issuer:         result.Issuer,
		Subject:        result.Subject,
		IntegratedTime: result.IntegratedTime,
		Enforcement:    string(enforcement),
	}
	if result.Error != nil {
		output.Error = result.Error.Error()
	}
	return output
}
