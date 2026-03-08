# Provenance

[nix.md](nix.md) describes how Bureau builds and distributes binaries and
environment closures through Nix store paths and binary caches;
[artifacts.md](artifacts.md) describes the content-addressable storage
layer; [credentials.md](credentials.md) describes the sealed credential
model and privilege separation; [service-distribution.md](service-distribution.md)
describes how out-of-tree services are packaged and delivered as flakes.
This document describes provenance verification — how Bureau
cryptographically verifies the origin of binaries, artifacts, models,
and templates that cross trust boundaries into the system.

---

## Why Provenance

Bureau's runtime trust model is infrastructure-enforced. The daemon
controls what runs. The launcher holds the privilege boundary. The proxy
mediates credentials. Sandboxes constrain what agents can reach. Within
a running Bureau instance, an agent cannot misrepresent its identity
because it never holds the credentials that define its identity. This is
a stronger security property than attestation: the system enforces who
you are, rather than requiring you to prove it.

But infrastructure enforcement stops at the infrastructure boundary.
Before a binary runs in a sandbox, someone decided to deploy it. Before
an artifact is served from the CAS, something ingested it. Before a
model processes data, a pipeline or operator selected it. At each of
these ingestion points, the question is not "who is running this?" (the
runtime answers that) but "where did this come from, and should I trust
it?"

Recording provenance metadata (which flake ref, which machine, which
store path) is necessary but not sufficient. Metadata is only as
trustworthy as the channel that delivers it. A compromised CI pipeline
or a man-in-the-middle on the binary cache could substitute different
bytes for a store path, and a metadata record that says "built from
commit X" would be wrong without any way to detect it. Nix's Ed25519
cache signature proves "the cache operator signed this" but not "this
was built from the source you intended."

Provenance verification adds the missing layer: cryptographic proof
that an artifact was produced by a specific build pipeline, from a
specific source, by a specific identity. The proof travels with the
artifact and is verified locally — no network requests at verification
time, no callback to the originating infrastructure.

---

## Trust Model

Bureau uses a two-tier trust model: cryptographic provenance at
ingestion boundaries, infrastructure enforcement at runtime. The tiers
are complementary, not redundant.

### Tier 1: Ingestion Boundary (Provenance Verification)

Every artifact entering Bureau from outside the fleet's infrastructure
boundary carries a **provenance bundle** — a self-contained
cryptographic proof of origin. The bundle contains:

- A **signature** over the artifact's content hash.
- A **short-lived X.509 certificate** issued by a certificate authority
  (Fulcio, or a private instance) that embeds the signer's OIDC
  identity claims: which CI provider, which repository, which workflow,
  which branch, which commit.
- A **transparency log inclusion proof** (Rekor) that timestamps the
  signature and makes it publicly auditable.

The certificate is ephemeral — valid for minutes, not years. There is no
long-lived signing key to manage, rotate, or protect. The trust anchor
is the OIDC identity (the CI pipeline's ambient token), not a key.

Verification checks:

1. The certificate chains to a trusted Fulcio root (cached locally).
2. The signature is valid for the artifact's content hash.
3. The OIDC claims in the certificate match the fleet's provenance
   policy (trusted repos, workflows, branches).
4. The Rekor inclusion proof confirms the signature was logged (optional
   for offline operation — the proof is in the bundle, verification is
   local).

All four checks are local operations against cached trust material and
the bundle itself. No network requests.

### Tier 2: Runtime (Infrastructure Enforcement)

Once an artifact has passed provenance verification at ingestion, the
runtime trust model takes over. The daemon deploys it, the launcher
enforces the sandbox boundary, the proxy mediates credentials. No
further provenance checks occur during runtime — the ingestion
verification is the gate, and the infrastructure enforces everything
after.

This separation is deliberate. Provenance verification is expensive in
a conceptual sense (it requires understanding certificate chains, OIDC
claims, and transparency logs) but cheap computationally (a few
milliseconds of signature verification). Infrastructure enforcement is
the opposite: conceptually simple (the sandbox can't reach what it
can't reach) but operationally rich (namespace creation, credential
injection, proxy routing). Each tier handles what it does well.

### When Each Tier Applies

| Boundary | Trust mechanism | Examples |
|----------|----------------|----------|
| CI → binary cache | Provenance bundle | Bureau binaries, dev shell closures |
| Binary cache → fleet machine | Provenance verification + Nix signature | `nix-store --realise` with bundle check |
| External source → artifact store | Provenance verification | Downloaded models, datasets, build outputs |
| Forge → Bureau | Provenance verification | GitHub release binaries, Actions artifacts |
| Fleet A → Fleet B | Provenance verification | Federated artifact/environment transfer |
| Template flake → template room | Provenance verification | `bureau template publish --flake` |
| Within fleet | Infrastructure enforcement | Sandbox creation, credential injection, proxy routing |
| Within sandbox | Infrastructure enforcement | MatrixPolicy, service socket auth |

---

## Trust Material

Provenance verification requires two categories of configuration: trust
roots (the cryptographic anchors) and provenance policy (the operator's
intent). Both are distributed via Matrix state events and cached locally
on each machine.

### Trust Roots

Trust roots are the root certificates and public keys needed to verify
provenance bundles. They change rarely — root CA rotation is a
deliberate ceremony, not a routine operation.

Bureau supports two trust root configurations:

**Sigstore public infrastructure.** The default for open-source
workflows. Fulcio and Rekor are operated by the OpenSSF. Trust roots
are distributed as a TUF (The Update Framework) repository with pinned
root keys. Bureau fetches the TUF root once during fleet setup and
verifies subsequent updates against the pinned root.

**Private Sigstore instance.** For fleets that must not depend on
external infrastructure or that require their own root of trust. A
private Fulcio CA and Rekor instance run on fleet infrastructure (or a
trusted operator's infrastructure). Trust roots are the operator's own
CA certificate and Rekor public key. This is the path for fully
air-gapped deployments.

Trust roots are stored as an `m.bureau.provenance_roots` state event in
the fleet room. The daemon syncs this event and writes the roots to a
local file for the launcher and verification code to read. The event
is admin-only (PL 100) — a compromised trust root is a complete
supply chain compromise.

```json
{
  "roots": {
    "sigstore_public": {
      "tuf_root_version": 5,
      "fulcio_root_pem": "-----BEGIN CERTIFICATE-----\n...",
      "rekor_public_key_pem": "-----BEGIN PUBLIC KEY-----\n..."
    }
  }
}
```

Multiple root sets can coexist (e.g., `sigstore_public` for open-source
dependencies and `fleet_private` for fleet-internal attestations). Each
trusted identity (below) references which root set it uses.

### Provenance Policy

Provenance policy defines which OIDC identities the fleet trusts and
how strictly each artifact category enforces verification. It is stored
as an `m.bureau.provenance_policy` state event in the fleet room,
admin-only (PL 100).

The policy has two parts: trusted identities and enforcement rules.

**Trusted identities** define which OIDC signers the fleet accepts.
Each identity is a named entry with match criteria:

```json
{
  "trusted_identities": [
    {
      "name": "bureau-ci",
      "roots": "sigstore_public",
      "issuer": "https://token.actions.githubusercontent.com",
      "subject_pattern": "repo:bureau-foundation/*:ref:refs/heads/main",
      "workflow_pattern": ".github/workflows/ci.yaml"
    },
    {
      "name": "bureau-services",
      "roots": "sigstore_public",
      "issuer": "https://token.actions.githubusercontent.com",
      "subject_pattern": "repo:bureau-foundation/bureau-*:ref:refs/heads/main"
    },
    {
      "name": "partner-models",
      "roots": "sigstore_public",
      "issuer": "https://token.actions.githubusercontent.com",
      "subject_pattern": "repo:trusted-model-org/*:ref:refs/tags/v*"
    }
  ]
}
```

The `subject_pattern` and `workflow_pattern` fields use glob matching
against the OIDC token's subject and workflow claims. The subject
claim in GitHub Actions OIDC tokens has the format
`repo:<owner>/<repo>:ref:<ref>` — the pattern matches against this
full string.

**Enforcement rules** control how strictly each artifact category
requires provenance verification:

```json
{
  "enforcement": {
    "nix_store_paths": "require",
    "artifacts": "warn",
    "models": "require",
    "forge_artifacts": "warn",
    "templates": "log"
  }
}
```

Three enforcement levels:

- **`require`** — reject the artifact if provenance verification fails
  or no bundle is present. The artifact does not enter the system.
- **`warn`** — accept the artifact but publish a warning to the fleet
  room. Operators see unverified ingestions. Use during rollout.
- **`log`** — accept the artifact and log the verification result.
  No operator-visible warning. Use for categories where provenance
  is not yet available from upstream sources.

Enforcement levels can be tightened incrementally as provenance coverage
expands. A fleet might start with `log` for everything, move templates
and models to `warn`, then graduate to `require` once all sources
produce attestations.

---

## Provenance Bundles

A provenance bundle is a self-contained JSON file following the
Sigstore bundle format (defined by the sigstore protobuf-specs). It
contains everything needed for offline verification: the cryptographic
signature over the artifact's content hash, the short-lived Fulcio
certificate (with OIDC claims as X.509 extensions), and the Rekor
transparency log inclusion proof (a Merkle proof plus signed tree
head). Bundles are typically 2-5 KB.

Bundles travel with the artifact they attest — alongside a `.narinfo`
in the binary cache, embedded in an artifact store metadata record, or
attached to a Matrix state event.

### Provenance Predicate

The bundle's signature covers an in-toto provenance statement (SLSA
v1 predicate type) that binds the artifact to its build context. The
statement identifies the artifact by name and content hash (the
subject), and describes the build that produced it (the predicate).

The OIDC identity (repo, workflow, branch, commit) is not in the
predicate — it is embedded in the Fulcio certificate as X.509
extensions. The predicate carries the build-system-specific context.
The certificate and predicate together answer both "who built this?"
and "what did they build?"

Bureau defines a custom build type URI for each artifact category:

- **Nix store paths** (`bureau.foundation/provenance/nix-derivation/v1`):
  the predicate carries the flake reference, flake output attribute,
  resolved git revision, and optionally resolved dependency store
  paths. This lets a verifier confirm not just that the artifact came
  from the right repo, but that it was the specific flake output
  expected.

- **Artifacts** (`bureau.foundation/provenance/artifact/v1`): the
  predicate carries the artifact reference, source repository and
  revision, and content type. Used for build outputs, datasets, and
  other files entering the CAS from external sources.

- **Models** (`bureau.foundation/provenance/model/v1`): the predicate
  carries the model identifier, source (registry and revision), and
  framework. Used for model files from Hugging Face, provider
  downloads, or operator uploads.

Verification code does not interpret the predicate contents — it
verifies the signature and checks the OIDC claims in the certificate.
The predicate is informational metadata for operators and audit trails.

---

## Verification Points

Provenance verification occurs at each ingestion boundary. Each
verification point calls the same core verification function with the
bundle, the artifact hash, the trust roots, and the provenance policy.
The function returns a typed result: verified (with identity details),
unverified (no bundle present), or rejected (verification failed).

### Nix Store Paths

**When:** the daemon prefetches a store path via `nix-store --realise`
for binary updates (`reconcileBureauVersion`) or environment deployment.

**How:** after `nix-store --realise` succeeds (Nix's own Ed25519
cache signature verified), the daemon fetches the provenance bundle
from the binary cache at `attestation/<store-path-basename>.bundle.json`
alongside the `.narinfo`. The bundle is verified against trust roots
and the policy's `nix_store_paths` enforcement level. If enforcement
is `require` and verification fails, the store path is not used — the
daemon does not create a sandbox with an unverified binary, and does
not exec-update to an unverified daemon or launcher binary.

The fast path (`os.Stat` check for existing store paths) extends to
check for a cached verification result. Once a store path has been
verified, the result is cached locally (the store path is immutable,
so the verification result is stable). Subsequent reconciliation
cycles skip re-verification.

**Integration with existing flow:** the change to `prefetchBureauVersion`
and `reconcileBureauVersion` is minimal. After the existing
`prefetchNixStore` call succeeds, add a `verifyProvenance` call. The
existing content-hash comparison (`version.Compare`) continues to work
unchanged — provenance verification is an additional gate, not a
replacement for hash comparison.

### Artifacts

**When:** the artifact service ingests content from an external source
(not from a Bureau agent producing artifacts within the fleet).

**How:** the ingestion request includes an optional provenance bundle.
The artifact service verifies the bundle against the artifact's
file-domain hash (the BLAKE3 digest that is the artifact's identity in
the CAS). If the policy's `artifacts` enforcement level requires
verification and no bundle is present or verification fails, ingestion
is rejected.

For artifacts produced within the fleet (by agents running in
sandboxes), provenance verification is unnecessary — the infrastructure
enforces which agent produced the artifact, and the artifact's
lineage is recorded via the producing agent's Matrix identity and the
artifact tag's state event.

### Models

**When:** a model is registered with the model service or downloaded
for local inference.

**How:** model files from external sources (Hugging Face, provider
downloads, operator uploads) carry provenance bundles attesting their
origin. The model service verifies the bundle against the model file's
content hash before making the model available for inference. The
policy's `models` enforcement level controls strictness.

Model provenance is particularly valuable because models are opaque —
you cannot inspect a model file to determine if it has been tampered
with. A backdoored model produces correct outputs on most inputs.
Provenance verification is the primary defense: proving that the
model came from a trusted training pipeline is more practical than
attempting to verify model behavior.

### Forge Artifacts

**When:** a forge connector downloads release binaries, Actions
artifacts, or other build outputs from a forge.

**How:** GitHub Actions already produces Sigstore attestation bundles
for artifacts built in Actions workflows. When the forge connector
downloads an artifact, it also fetches the attestation bundle (via
GitHub's attestation API or alongside the artifact). Verification
uses the same provenance policy — the forge artifact's OIDC identity
must match a trusted identity.

For self-hosted forges (Forgejo, GitLab) that don't yet have Sigstore
integration, the policy's `forge_artifacts` enforcement level can be
set to `log` until attestation support is available.

### Templates

**When:** `bureau template publish --flake` publishes a template
from a flake reference.

**How:** the publishing command evaluates the flake, reads the
`bureauTemplate` output, and resolves the binary's store path. If the
store path has a provenance bundle in the binary cache, the bundle is
verified and its key metadata (signer identity, source revision,
build type) is embedded in the template's `origin` field in the
Matrix state event. The daemon can then verify template provenance
without re-fetching the bundle.

For templates published from URLs or local files (not flakes), no
provenance bundle is available. The policy's `templates` enforcement
level controls whether this is acceptable.

Template provenance becomes important for federation: when a fleet
accepts templates from an external source, the provenance bundle in
the template's origin field proves the template was derived from a
specific source by a specific build pipeline.

---

## Binary Cache Layout

The provenance bundle for a Nix store path is stored alongside the
`.narinfo` in the binary cache:

```
nix-cache-info
<hash>.narinfo
nar/<hash>.nar.zst
attestation/<store-path-basename>.bundle.json
```

The `attestation/` prefix separates provenance bundles from Nix's own
cache layout. The key is the store path basename (e.g.,
`abc123-bureau-daemon`) — the same identifier that appears in the
`.narinfo` filename, ensuring a 1:1 mapping.

For the R2 cache (CI-produced Bureau binaries and dev shell closures),
bundles are uploaded in the same CI step that pushes store paths. For
fleet caches (Attic-backed, environment composition results), bundles
are uploaded by the composition pipeline alongside the `attic push`.

### Cache Miss Behavior

When a store path has no corresponding bundle in the cache, the
verification function returns `unverified`. The enforcement level
determines the response:

- `require` → reject the store path.
- `warn` → accept with a warning event in the fleet room.
- `log` → accept with a log entry.

This handles the transition period where not all store paths have
attestations (e.g., upstream nixpkgs paths will never have Bureau-
issued attestations). The enforcement level for `nix_store_paths`
can distinguish between Bureau-produced paths (require attestation)
and upstream paths (allow unattested) via the path prefix or derivation
name. The provenance policy may gain pattern-based enforcement rules
in the future to express this distinction precisely; for now, the
enforcement level applies uniformly per category.

---

## CI Integration

### GitHub Actions

Bureau's CI workflow (`.github/workflows/ci.yaml`) gains three changes:

**1. OIDC token permission.** Add `id-token: write` to the workflow's
permissions block, enabling GitHub Actions to generate OIDC tokens
for Sigstore signing.

**2. Attestation generation step.** After the existing `nix build` and
`nix store sign` steps, a new step generates Sigstore bundles for each
store path that will be pushed to the cache:

```yaml
- name: Generate provenance attestations
  if: github.ref == 'refs/heads/main' && github.event_name == 'push'
  run: |
    for output in bureau bureau-daemon bureau-launcher bureau-proxy ...; do
      STORE_PATH=$(nix path-info ".#$output")
      BASENAME=$(basename "$STORE_PATH")
      DIGEST=$(nix-store --dump "$STORE_PATH" | sha256sum | cut -d' ' -f1)

      cosign attest-blob \
        --bundle "attestation/$BASENAME.bundle.json" \
        --predicate <(bureau-provenance-statement "$output" "$STORE_PATH") \
        --yes \
        /dev/null
    done
```

The `bureau-provenance-statement` helper generates the in-toto
statement for each store path. It is a small script or Go binary that
reads the flake metadata and produces the SLSA provenance predicate.

**3. Bundle upload.** Bundles are uploaded to R2 alongside the store
paths:

```yaml
- name: Upload attestation bundles
  if: github.ref == 'refs/heads/main' && github.event_name == 'push'
  run: |
    aws s3 sync attestation/ \
      "s3://bureau-cache/attestation/" \
      --endpoint-url "https://${{ secrets.R2_ENDPOINT }}"
```

### Out-of-Tree Service Repositories

Service repositories that follow the flake-as-package model
(service-distribution.md) generate attestation bundles in their own CI
workflows using the same pattern. The bundles are pushed to whichever
binary cache the service uses. Bureau machines pull the bundles from
the same cache they pull the store paths from — the attestation path
mirrors the nar path.

### Environment Composition Pipeline

The `bureau environment compose` pipeline runs inside a Bureau sandbox
and pushes composed environments to the fleet's Attic cache. For
fleet-internal trust, the existing Nix cache signing key and the
`EnvironmentBuildContent` provenance record in Matrix are sufficient.

For cross-fleet trust (federation), the composition pipeline can
generate an attestation bundle using the fleet's OIDC provider (if a
private Sigstore instance is deployed) or the machine's existing age
keypair as a signing key (with a simplified verification path that
does not require Fulcio or Rekor). The appropriate mechanism depends
on the fleet's deployment model and is controlled by the provenance
policy.

---

## Offline Operation

Provenance verification is designed for offline operation. The only
network-dependent moment is the initial signing in CI, which happens
outside Bureau.

**What is fetched once:**

- Trust roots (Fulcio root certificate, Rekor public key). Stored in
  the `m.bureau.provenance_roots` state event. Synced to machines
  via Matrix. Cached locally by the daemon.
- Provenance bundles. Fetched from the binary cache alongside store
  paths. Cached locally after verification.

**What requires no network:**

- Certificate chain verification (local crypto against cached root).
- Signature verification (local crypto against certificate public key).
- OIDC claim extraction (parsing certificate extensions).
- Policy matching (pattern matching against cached policy).
- Transparency log proof verification (Merkle proof against cached
  Rekor public key — the proof is in the bundle).

**What is optional and network-dependent:**

- Checking the live Rekor transparency log for consistency (verifying
  the bundle's log entry against the current log state). This is an
  additional assurance — "the log hasn't been tampered with since the
  bundle was created" — but is not required for basic verification.
  Air-gapped deployments skip this check. Connected deployments may
  perform it periodically as a background audit.

**Trust root updates:**

- Sigstore public roots rotate through TUF, which is itself designed
  for offline verification with pinned root keys. An operator fetches
  the latest TUF snapshot, verifies it against the pinned root,
  extracts updated Fulcio/Rekor keys, and publishes them to the fleet
  room. Machines pick up the update on the next Matrix sync.
- Private Sigstore instance roots are managed directly by the operator
  — publish the new CA certificate to the fleet room, machines update
  on sync.
- In both cases, the operator controls the timing. No automatic root
  rotation occurs without explicit operator action.

---

## Federation

Provenance verification is the trust mechanism for cross-fleet
federation. Within a fleet, infrastructure enforcement is sufficient.
Across fleets, you need portable trust — something that travels with
the artifact and can be verified without access to the originating
fleet's infrastructure.

### Cross-Fleet Artifact Transfer

When Fleet A sends an artifact to Fleet B (environment closure, model,
build output), the artifact carries its provenance bundle. Fleet B
verifies the bundle against its own provenance policy — its own trust
roots and its own trusted identities. Fleet B does not need access to
Fleet A's infrastructure, CI system, or binary cache. The bundle is
self-contained.

The practical workflow:

1. Fleet A's CI builds an artifact, generates an attestation bundle
   (signed via OIDC in CI).
2. The artifact and bundle are pushed to Fleet A's binary cache.
3. Fleet B's operator adds Fleet A's CI identity to Fleet B's
   provenance policy: "trust artifacts signed by GitHub Actions OIDC
   from `fleet-a-org/*` repos."
4. Fleet B pulls the artifact and bundle from Fleet A's cache (or
   receives them via any transport — Matrix, direct transfer, USB).
5. Fleet B verifies the bundle locally against its own trust roots
   and policy.

No shared secrets, no mutual infrastructure access, no online
callbacks. The only shared element is the trust root (both fleets
trust the same Sigstore public infrastructure, or both trust a shared
private CA).

### Shared Binary Cache

Two fleets sharing a binary cache (both reading from the same R2
bucket) is the simplest federation path. Both fleets configure the
cache as a Nix substituter with the cache's signing key. Provenance
bundles in the `attestation/` directory are available to both fleets.
Each fleet's provenance policy independently determines which store
paths it accepts.

### Template Federation

When a fleet publishes templates for external consumption, the
template's `origin` field carries the provenance metadata extracted
during `bureau template publish --flake`. A receiving fleet verifies
the origin's attestation details against its own policy before
accepting the template.

---

## Schema Types

### `m.bureau.provenance_roots`

State event in the fleet room. State key: empty string (one per room).
Power level: 100 (admin only). A compromised trust root is a complete
supply chain compromise — the same security posture as
`m.bureau.fleet_cache`.

The event carries a map of named root sets. Each root set contains the
PEM-encoded Fulcio root certificate and Rekor public key needed to
verify bundles signed under that root. For roots sourced from Sigstore
public infrastructure, the TUF root version tracks which TUF snapshot
the roots were extracted from. Private instances set this to zero.

Multiple root sets can coexist — for example, `sigstore_public` for
open-source dependencies and `fleet_private` for fleet-internal
attestations. Each trusted identity (below) references which root set
it uses.

### `m.bureau.provenance_policy`

State event in the fleet room. State key: empty string (one per room).
Power level: 100 (admin only).

The policy has two parts: trusted identities and enforcement rules.

**Trusted identities** are a list of named OIDC signer descriptions.
Each entry specifies:

- A human-readable name (for audit logs and verification results).
- Which root set from `m.bureau.provenance_roots` the bundle must
  chain to.
- The expected OIDC issuer URL (e.g.,
  `https://token.actions.githubusercontent.com` for GitHub Actions).
- A glob pattern matched against the OIDC subject claim. For GitHub
  Actions, the subject has the form `repo:<owner>/<repo>:ref:<ref>`.
- An optional glob pattern matched against the workflow path claim.

**Enforcement rules** are a map from artifact category to enforcement
level. Three levels:

- **`require`** — reject the artifact if provenance verification fails
  or no bundle is present. The artifact does not enter the system.
- **`warn`** — accept the artifact but publish a warning to the fleet
  room. Operators see unverified ingestions. Useful during rollout.
- **`log`** — accept the artifact and log the verification result.
  No operator-visible warning. Useful for categories where provenance
  is not yet available from upstream sources.

Enforcement levels can be tightened incrementally as provenance coverage
expands. A fleet might start with `log` for everything, move models
to `warn`, then graduate to `require` once all sources produce
attestations.

### Enforcement Categories

| Category | Verification point | What it covers |
|----------|-------------------|----------------|
| `nix_store_paths` | Daemon prefetch | Bureau binaries, environment closures, host environment |
| `artifacts` | Artifact service ingestion | External files entering the CAS |
| `models` | Model service registration | Model files from external sources |
| `forge_artifacts` | Forge connector download | Release binaries, Actions artifacts, build outputs |
| `templates` | Template publishing | Templates published from flakes or URLs |

Unrecognized categories are ignored (forward-compatible). Missing
categories default to `log` (permissive by default, tighten
explicitly).

### Verification Result

The core verification function returns one of three outcomes for each
artifact:

- **Verified** — the bundle is valid and the signer matches a trusted
  identity. The result carries the matched identity name, the OIDC
  issuer and subject from the Fulcio certificate, and the Rekor log
  timestamp.
- **Unverified** — no bundle was present. The enforcement level
  determines whether this is acceptable.
- **Rejected** — verification failed: invalid signature, untrusted
  signer, certificate chain error, or policy mismatch. The result
  carries a description of the failure.

---

## Implementation

### Go Library

Provenance verification is implemented in `lib/provenance/` using only
the Go standard library — no third-party cryptography or Sigstore SDK
dependencies. The package parses Sigstore bundle v0.3 JSON, performs
all cryptographic verification offline, and matches OIDC claims against
fleet policy. The zero-dependency approach eliminates the 62 transitive
dependencies that sigstore-go would introduce and gives Bureau full
control over the verification logic in a security-critical path.

The verification pipeline has six phases, each of which must succeed
before the next begins:

- **Certificate chain verification.** The Fulcio leaf certificate must
  chain to one of the root set's trust anchors via `x509.Certificate.Verify`.
  Intermediate certificates (non-self-signed CAs from the trusted root
  file) are placed in the intermediates pool, not the roots pool, so
  that path length constraints and name constraints are enforced by the
  Go X.509 verifier. Self-signed classification uses a three-layer
  check: fast negative on Distinguished Name mismatch, fast negative on
  Authority Key Identifier / Subject Key Identifier mismatch, then
  `cert.CheckSignatureFrom(cert)` as the authoritative cryptographic
  test.

- **Signature verification.** The artifact signature is verified against
  the Fulcio leaf certificate's public key. For DSSE (in-toto) bundles,
  the signature covers the DSSE envelope (PAE-encoded); for
  messageSignature bundles, it covers the raw artifact digest. Both
  ECDSA and Ed25519 leaf keys are supported.

- **Signed Entry Timestamp (SET).** The Rekor transparency log's SET
  authenticates the `integratedTime`, `logIndex`, `logID`, and entry
  body. The SET is an ECDSA-SHA256 signature over canonical JSON with
  fields in alphabetical order. Without SET verification, an attacker
  who compromises a bundle in transit could substitute the
  `integratedTime` to make an expired certificate appear valid.

- **Merkle inclusion proof.** The tlog entry's inclusion proof is
  verified against the signed checkpoint's root hash using RFC 6962
  interior node hashing (`SHA256(0x01 || left || right)`). The
  checkpoint is a C2SP signed note: the note body carries the tree
  origin, size, and root hash; the signature line carries a key ID
  and Ed25519 signature. The checkpoint origin line is cross-checked
  against the signature's signer name to prevent checkpoint
  substitution across logs.

- **Transparency log binding.** The tlog entry body is verified to
  contain the same leaf certificate and artifact signature as the
  bundle, preventing an attacker from substituting a valid tlog entry
  from a different signing operation.

- **Certificate validity window.** The Fulcio leaf certificate's
  NotBefore/NotAfter window is checked against the authenticated
  `integratedTime` from the SET. This is checked last because the
  time must be authenticated by the SET before it can be trusted.

The package provides three entry points:

- `NewVerifier(roots, policy)` constructs a verifier from fleet trust
  roots and provenance policy. Trust roots are parsed from PEM (for
  Matrix state events) or from Sigstore trusted root JSON (for test
  fixtures and the public good instance).
- `Verifier.Verify(bundle, algorithm, digest)` runs the full
  verification pipeline and returns a typed result: verified (with
  matched identity name, OIDC issuer, subject, and authenticated
  timestamp), unverified, or rejected (with error).
- `FetchBundle(client, cacheURL, basename)` retrieves bundles from
  binary caches by Nix store path basename.

### CLI

`bureau fleet provenance` manages trust roots and policy:

```
bureau fleet provenance roots     Show current trust roots
bureau fleet provenance policy    Show current provenance policy
bureau fleet provenance verify    Verify a specific store path or artifact
bureau fleet provenance init      Bootstrap trust roots from Sigstore public TUF
```

`bureau template publish --flake` gains provenance verification of the
template's binary store path during publishing, embedding the
verification result in the template's `origin` field.

### Daemon Integration

The daemon's prefetch path (`prefetchBureauVersion`,
`prefetchNixStore`) gains a `verifyProvenance` call after each
successful `nix-store --realise`. The verification result is cached
in a local file alongside the store path (keyed by store path
basename). The `reconcileBureauVersion` function checks the cached
verification result before proceeding with binary updates.

The daemon also verifies provenance when processing new templates
via /sync, if the template has an `origin` field with provenance
metadata.

---

## Relationship to Other Design Documents

- [nix.md](nix.md) describes the binary cache layout, Nix store path
  signing, environment composition pipeline, and the prefetch path
  that provenance verification extends.
- [artifacts.md](artifacts.md) describes the CAS engine and content
  hashing. Provenance verification adds origin binding on top of
  content-addressing — the hash proves integrity, the provenance
  proves origin.
- [credentials.md](credentials.md) describes the sealed credential
  model. Provenance verification and credential management are
  complementary: credentials control what a principal can do,
  provenance controls whether the principal's binary is trusted.
- [service-distribution.md](service-distribution.md) describes the
  flake-as-package model and template publishing. Provenance
  verification extends template publishing with attestation checks.
- [fleet.md](fleet.md) describes fleet management and machine
  lifecycle. Provenance policy is fleet-scoped.
- [forges.md](forges.md) describes forge connectors. Provenance
  verification applies to artifacts downloaded from forges.
- [model-service.md](model-service.md) describes the model gateway.
  Provenance verification applies to model files from external sources.
