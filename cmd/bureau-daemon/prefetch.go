// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/bureau-foundation/bureau/lib/nix"
	"github.com/bureau-foundation/bureau/lib/provenance"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// prefetchEnvironment ensures a Nix store path and its full transitive
// closure exist in the local Nix store. Uses an os.Stat fast path to
// avoid forking nix-store on every reconcile cycle when the path is
// already present. Nix store paths are content-addressed and immutable,
// so existence of the top-level path guarantees closure integrity (Nix
// GC operates on closure boundaries).
//
// When the path is missing, delegates to d.prefetchFunc (defaulting to
// prefetchNixStore) which invokes nix-store --realise to fetch from
// configured substituters.
func (d *Daemon) prefetchEnvironment(ctx context.Context, storePath string) error {
	// Fast path: the store path already exists locally. Nix store paths
	// are written atomically (rename into /nix/store/) so there is no
	// race with concurrent fetches — we see either the full path or
	// nothing.
	if _, err := os.Stat(storePath); err == nil {
		return nil
	}

	d.logger.Info("prefetching nix environment", "store_path", storePath)

	prefetch := d.prefetchFunc
	if prefetch == nil {
		prefetch = prefetchNixStore
	}
	if err := prefetch(ctx, storePath); err != nil {
		return err
	}

	d.logger.Info("nix environment prefetched", "store_path", storePath)
	return nil
}

// prefetchBureauVersion ensures all non-empty store paths in a BureauVersion
// exist in the local Nix store. Each path is prefetched independently so that
// a failure on one component (e.g., launcher) does not prevent the others
// (e.g., daemon, proxy) from being prefetched. The caller can then compare
// whatever store paths are available.
//
// After each newly fetched store path, provenance verification is performed
// against the fleet's trust roots and policy. When enforcement is "require"
// and verification fails, the prefetch returns an error — blocking the
// entire version update. Paths that already exist locally (os.Stat fast
// path) are not re-verified: the daemon trusts store paths that were
// present before this reconcile cycle, and Nix's own Ed25519 cache
// signatures have already validated them.
//
// Returns the first error encountered, if any. On error, some store paths
// may have been fetched while others were not — the caller should check
// individual path existence before hashing.
func (d *Daemon) prefetchBureauVersion(ctx context.Context, version *schema.BureauVersion) error {
	paths := []struct {
		label string
		path  string
	}{
		{"daemon", version.DaemonStorePath},
		{"launcher", version.LauncherStorePath},
		{"proxy", version.ProxyStorePath},
		{"log-relay", version.LogRelayStorePath},
		{"host-environment", version.HostEnvironmentPath},
	}

	for _, entry := range paths {
		if entry.path == "" {
			continue
		}

		// BureauVersion contains full file paths (e.g.,
		// /nix/store/abc-bureau-daemon/bin/bureau-daemon). Check if
		// the file already exists locally (fast path — skips
		// nix-store invocation on the common case where the binary
		// was built on this machine).
		if _, statErr := os.Stat(entry.path); statErr == nil {
			continue
		}

		// File doesn't exist locally. Extract the Nix store directory
		// (e.g., /nix/store/abc-bureau-daemon) and prefetch it.
		// nix-store --realise expects store directory paths, not file
		// paths within them.
		storeDirectory, err := nix.StoreDirectory(entry.path)
		if err != nil {
			return fmt.Errorf("invalid %s store path %s: %w", entry.label, entry.path, err)
		}
		if err := d.prefetchEnvironment(ctx, storeDirectory); err != nil {
			return fmt.Errorf("prefetching %s store path %s: %w", entry.label, storeDirectory, err)
		}

		// Verify provenance of the newly fetched store path. This
		// is an additional gate beyond Nix's Ed25519 cache
		// signatures: the bundle attests that the NAR was built by
		// a trusted CI identity (Fulcio cert + Rekor tlog entry).
		if err := d.verifyStorePathProvenance(ctx, entry.label, storeDirectory); err != nil {
			return err
		}
	}
	return nil
}

// verifyStorePathProvenance verifies a Sigstore provenance bundle for a
// newly fetched Nix store path. The bundle is fetched from the fleet's
// binary cache at attestation/<basename>.bundle.json and verified against
// the fleet's trust roots and policy.
//
// The verification flow:
//  1. Check if a provenance verifier is configured (roots + policy
//     published to the fleet room). If not, skip — the fleet has no
//     provenance policy.
//  2. Check the enforcement level for the "nix_store_paths" category.
//  3. Compute the NAR digest (SHA-256 of `nix-store --dump`) for the
//     store path. This is the artifact digest that was signed by CI.
//  4. Fetch the bundle from the binary cache.
//  5. Verify: certificate chain, ECDSA signature, Merkle inclusion
//     proof, Rekor checkpoint+SET, OIDC identity matching.
//
// Returns an error only when enforcement is "require" and verification
// fails or no bundle exists. For "warn", failures are logged and posted
// as notifications to the config room but do not block the version
// update. For "log", failures are logged only.
func (d *Daemon) verifyStorePathProvenance(ctx context.Context, label, storeDirectory string) error {
	verifier := d.provenanceVerifier
	if verifier == nil {
		return nil
	}

	enforcement := verifier.Enforcement("nix_store_paths")

	// Resolve the binary cache URL from fleet config. Without a cache
	// URL, bundles cannot be fetched.
	cacheURL := ""
	if d.fleetCacheConfig != nil {
		cacheURL = d.fleetCacheConfig.URL
	}
	if cacheURL == "" {
		if enforcement == schema.EnforcementRequire {
			return fmt.Errorf("provenance: no binary cache URL configured for %s verification", label)
		}
		d.logger.Warn("provenance: no binary cache URL, skipping verification",
			"store_path", storeDirectory)
		d.postProvenanceWarning(ctx, label, storeDirectory, enforcement,
			"no_cache_url", "no binary cache URL configured")
		return nil
	}

	basename := filepath.Base(storeDirectory)

	// Fetch the provenance bundle from the cache's attestation directory.
	// Use a bounded timeout to prevent a malicious or slow cache server
	// from stalling the reconcile loop indefinitely.
	bundleBytes, err := provenance.FetchBundle(&http.Client{Timeout: 30 * time.Second}, cacheURL, basename)
	if err != nil {
		if errors.Is(err, provenance.ErrNoBundleFound) {
			d.logger.Warn("no provenance bundle for store path",
				"label", label,
				"store_path", storeDirectory,
				"enforcement", enforcement,
			)
			if enforcement == schema.EnforcementRequire {
				return fmt.Errorf("provenance: %s: %w", label, provenance.ErrNoBundleFound)
			}
			d.postProvenanceWarning(ctx, label, storeDirectory, enforcement,
				"no_bundle", "no attestation bundle found in cache")
			return nil
		}
		d.logger.Error("fetching provenance bundle",
			"label", label,
			"store_path", storeDirectory,
			"error", err,
		)
		if enforcement == schema.EnforcementRequire {
			return fmt.Errorf("provenance: fetching %s bundle: %w", label, err)
		}
		d.postProvenanceWarning(ctx, label, storeDirectory, enforcement,
			"fetch_error", err.Error())
		return nil
	}

	// Compute the NAR digest. This is the SHA-256 of the NAR
	// serialization (nix-store --dump), which is the artifact digest
	// that cosign signed when generating the attestation in CI.
	narDigestFn := d.narDigestFunc
	if narDigestFn == nil {
		narDigestFn = nix.NARDigest
	}
	narDigest, err := narDigestFn(ctx, storeDirectory)
	if err != nil {
		d.logger.Error("computing NAR digest for provenance verification",
			"label", label,
			"store_path", storeDirectory,
			"error", err,
		)
		if enforcement == schema.EnforcementRequire {
			return fmt.Errorf("provenance: computing %s NAR digest: %w", label, err)
		}
		d.postProvenanceWarning(ctx, label, storeDirectory, enforcement,
			"digest_error", err.Error())
		return nil
	}

	// Verify the bundle against trust roots and policy.
	result := verifier.Verify(bundleBytes, "sha256", narDigest)

	switch result.Status {
	case provenance.StatusVerified:
		d.logger.Info("provenance verified",
			"label", label,
			"store_path", storeDirectory,
			"identity", result.Identity,
			"issuer", result.Issuer,
			"subject", result.Subject,
			"integrated_time", result.IntegratedTime,
		)
		return nil

	case provenance.StatusRejected:
		d.logger.Error("provenance verification failed",
			"label", label,
			"store_path", storeDirectory,
			"enforcement", enforcement,
			"error", result.Error,
		)
		if enforcement == schema.EnforcementRequire {
			return fmt.Errorf("provenance: %s rejected: %w", label, result.Error)
		}
		d.postProvenanceWarning(ctx, label, storeDirectory, enforcement,
			"rejected", result.Error.Error())
		return nil

	default:
		d.logger.Warn("unexpected provenance verification status",
			"label", label,
			"store_path", storeDirectory,
			"status", result.Status,
		)
		return nil
	}
}

// postProvenanceWarning posts a provenance warning notification to the
// config room when enforcement is "warn". For "log" enforcement (or any
// other level), only slog output is produced — no Matrix message. This
// keeps "log" lightweight while making "warn" visible to operators
// monitoring the config room.
func (d *Daemon) postProvenanceWarning(ctx context.Context, label, storePath string, enforcement schema.EnforcementLevel, reason, errorMessage string) {
	if enforcement != schema.EnforcementWarn {
		return
	}
	if d.configRoomID.IsZero() {
		return
	}
	if _, err := d.sendEventRetry(ctx, d.configRoomID, schema.MatrixEventTypeMessage,
		schema.NewProvenanceWarningMessage(label, storePath, enforcement, reason, errorMessage)); err != nil {
		d.logger.Error("failed to post provenance warning notification",
			"label", label, "store_path", storePath, "error", err)
	}
}

// prefetchNixStore invokes nix-store --realise to fetch a store path
// and its full transitive closure from configured substituters (binary
// caches in nix.conf). This is a no-op when the path is already valid
// in the local store.
func prefetchNixStore(ctx context.Context, storePath string) error {
	_, err := nix.RunStore(ctx, "--realise", storePath)
	return err
}
