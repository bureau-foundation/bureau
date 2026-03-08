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

# Parse the space-separated output list into an array.
read -ra outputs_array <<< "$FLAKE_OUTPUTS"

# Resolve all store paths in a single flake evaluation. This avoids
# 19 separate evaluations that each access the SQLite eval-cache,
# causing contention ("database is busy" errors). All outputs should
# already be built by previous CI steps.
echo "Resolving store paths for ${#outputs_array[@]} outputs..."
path_info_refs=()
for output in "${outputs_array[@]}"; do
    path_info_refs+=(".#$output")
done

if ! path_info_output=$(nix path-info "${path_info_refs[@]}"); then
    echo "Error: failed to resolve store paths" >&2
    echo "  Ensure all flake outputs are built before running attestation." >&2
    exit 1
fi
mapfile -t store_paths <<< "$path_info_output"

if [ ${#store_paths[@]} -ne ${#outputs_array[@]} ]; then
    echo "Error: resolved ${#store_paths[@]} paths, expected ${#outputs_array[@]}" >&2
    exit 1
fi

echo "  Resolved ${#store_paths[@]} store paths"
echo ""

attested=0
failed=0

for i in "${!outputs_array[@]}"; do
    output="${outputs_array[$i]}"
    store_path="${store_paths[$i]}"
    store_basename=$(basename "$store_path")
    bundle_path="attestation/$store_basename.bundle.json"

    echo "--- Attesting: $output ---"
    echo "  Store path: $store_path"

    # Serialize the store path as a NAR to a temp file. cosign needs a
    # seekable file to compute the subject digest. The tee + sha256sum
    # pipeline computes the digest in the same pass as the write, so
    # we hash the NAR exactly once (cosign re-hashes it internally
    # when constructing the in-toto statement subject, but that second
    # pass over a ~40MB file takes <10ms and isn't worth eliminating).
    #
    # Name the temp file after the store basename so cosign uses it
    # as the in-toto subject name (cosign derives subject.name from
    # the blob filename).
    nar_dir=$(mktemp -d)
    nar_file="$nar_dir/$store_basename.nar"
    predicate_file=$(mktemp)

    echo "  Serializing NAR..."
    if ! digest=$(nix-store --dump "$store_path" | tee "$nar_file" | sha256sum | cut -d' ' -f1); then
        echo "  Error: NAR serialization failed for $store_path" >&2
        rm -rf "$nar_dir" "$predicate_file"
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
    #
    # Redirect stdout to suppress the DSSE envelope JSON that cosign
    # prints alongside writing the bundle file. Errors go to stderr.
    echo "  Signing with cosign..."
    if ! cosign attest-blob \
        --bundle "$bundle_path" \
        --predicate "$predicate_file" \
        --type "https://slsa.dev/provenance/v1" \
        --yes \
        "$nar_file" > /dev/null; then
        echo "  Error: cosign signing failed for $output" >&2
        rm -rf "$nar_dir" "$predicate_file"
        failed=$((failed + 1))
        continue
    fi

    rm -rf "$nar_dir" "$predicate_file"

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
