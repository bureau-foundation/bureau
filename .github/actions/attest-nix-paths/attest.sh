#!/usr/bin/env bash
# Copyright 2026 The Bureau Authors
# SPDX-License-Identifier: Apache-2.0
#
# Generate Sigstore provenance attestations for Nix store paths.
#
# This script is designed for the attest-nix-paths composite action but
# works standalone in any CI environment where cosign, Nix, jq, and
# sha256sum are available.
#
# For GitHub Actions: set FLAKE_OUTPUTS and the cache variables. The
# GITHUB_* variables are provided automatically by the runner.
#
# For Forgejo/GitLab: set FLAKE_OUTPUTS plus the CI metadata variables
# (CI_SERVER_URL, CI_REPOSITORY, CI_COMMIT_SHA, CI_RUN_ID) and ensure
# cosign can obtain an OIDC token from your provider. If using a
# private Sigstore instance, set COSIGN_FULCIO_URL and COSIGN_REKOR_URL.
#
# Required environment:
#   FLAKE_OUTPUTS             Space-separated flake output attributes
#
# CI metadata (auto-detected on GitHub Actions):
#   GITHUB_REPOSITORY         e.g., "bureau-foundation/bureau"
#   GITHUB_SHA                Commit hash
#   GITHUB_SERVER_URL         e.g., "https://github.com"
#   GITHUB_RUN_ID             Workflow run ID
#
# Optional (for S3-compatible cache upload):
#   CACHE_ENDPOINT            S3-compatible endpoint URL
#   CACHE_BUCKET              S3 bucket name
#   AWS_ACCESS_KEY_ID         S3 credentials
#   AWS_SECRET_ACCESS_KEY     S3 credentials

set -euo pipefail

if [ -z "${FLAKE_OUTPUTS:-}" ]; then
    echo "Error: FLAKE_OUTPUTS is required" >&2
    exit 1
fi

# Resolve CI metadata. GitHub Actions variables are the primary source;
# Forgejo/GitLab CI can override with their equivalents.
server_url="${GITHUB_SERVER_URL:-${CI_SERVER_URL:-https://github.com}}"
repository="${GITHUB_REPOSITORY:-${CI_REPOSITORY:-unknown}}"
commit_sha="${GITHUB_SHA:-${CI_COMMIT_SHA:-unknown}}"
run_id="${GITHUB_RUN_ID:-${CI_RUN_ID:-}}"

build_type="bureau.foundation/provenance/nix-derivation/v1"

mkdir -p attestation

attested=0
failed=0

for output in $FLAKE_OUTPUTS; do
    echo "--- Attesting: $output ---"

    # Resolve the Nix store path for this flake output.
    if ! store_path=$(nix path-info ".#$output" 2>/dev/null); then
        echo "  Warning: could not resolve store path for .#$output, skipping" >&2
        failed=$((failed + 1))
        continue
    fi
    store_basename=$(basename "$store_path")
    bundle_path="attestation/$store_basename.bundle.json"

    echo "  Store path: $store_path"

    # Serialize the store path as a NAR to a temp file. cosign needs a
    # seekable file to compute the subject digest. The tee + sha256sum
    # pipeline computes the digest in the same pass as the write, so
    # we hash the NAR exactly once (cosign re-hashes it internally
    # when constructing the in-toto statement subject, but that second
    # pass over a ~40MB file takes <10ms and isn't worth eliminating).
    nar_file=$(mktemp)
    predicate_file=$(mktemp)

    echo "  Serializing NAR..."
    if ! digest=$(nix-store --dump "$store_path" | tee "$nar_file" | sha256sum | cut -d' ' -f1); then
        echo "  Error: NAR serialization failed for $store_path" >&2
        rm -f "$nar_file" "$predicate_file"
        failed=$((failed + 1))
        continue
    fi
    echo "  NAR digest: sha256:$digest"

    # Generate SLSA v1 provenance predicate. This is only the predicate
    # portion — cosign constructs the in-toto statement wrapper with
    # the subject (from the blob's hash) and predicateType (from --type).
    invocation_url=""
    if [ -n "$run_id" ]; then
        invocation_url="$server_url/$repository/actions/runs/$run_id"
    fi

    jq -n \
        --arg build_type "$build_type" \
        --arg flake_ref "github:$repository" \
        --arg flake_output "packages.x86_64-linux.$output" \
        --arg resolved_rev "$commit_sha" \
        --arg builder_id "$server_url/actions/runner" \
        --arg invocation_url "$invocation_url" \
        '{
            buildDefinition: {
                buildType: $build_type,
                externalParameters: {
                    flake_ref: $flake_ref,
                    flake_output: $flake_output,
                    resolved_rev: $resolved_rev
                }
            },
            runDetails: {
                builder: {
                    id: $builder_id
                },
                metadata: (
                    if $invocation_url != ""
                    then { invocationId: $invocation_url }
                    else {}
                    end
                )
            }
        }' > "$predicate_file"

    # Sign with cosign. In GitHub Actions, cosign auto-detects the
    # environment and requests an OIDC token for keyless signing via
    # Fulcio. The resulting bundle contains the DSSE envelope (with
    # the in-toto statement), the Fulcio certificate chain, and the
    # Rekor transparency log entry with inclusion proof.
    echo "  Signing with cosign..."
    if ! cosign attest-blob \
        --bundle "$bundle_path" \
        --predicate "$predicate_file" \
        --type "https://slsa.dev/provenance/v1" \
        --yes \
        "$nar_file"; then
        echo "  Error: cosign signing failed for $output" >&2
        rm -f "$nar_file" "$predicate_file"
        failed=$((failed + 1))
        continue
    fi

    rm -f "$nar_file" "$predicate_file"

    echo "  Bundle written: $bundle_path"
    attested=$((attested + 1))
done

echo ""
echo "Attestation summary: $attested succeeded, $failed failed"

# Upload bundles to S3-compatible cache.
if [ -n "${CACHE_ENDPOINT:-}" ] && [ -n "${CACHE_BUCKET:-}" ]; then
    bundle_count=$(find attestation -name '*.bundle.json' | wc -l)
    if [ "$bundle_count" -gt 0 ]; then
        echo "Uploading $bundle_count bundle(s) to s3://$CACHE_BUCKET/attestation/"
        aws s3 sync attestation/ "s3://$CACHE_BUCKET/attestation/" \
            --endpoint-url "$CACHE_ENDPOINT" \
            --region auto
    else
        echo "No bundles to upload"
    fi
fi

if [ "$failed" -gt 0 ]; then
    echo "Warning: $failed output(s) failed attestation" >&2
    exit 1
fi
