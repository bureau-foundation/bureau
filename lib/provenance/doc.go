// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package provenance implements offline Sigstore bundle verification for
// Bureau's supply chain provenance system.
//
// Bureau verifies provenance at ingestion boundaries — the points where
// external artifacts cross into the fleet's trust domain. Signing happens
// online during CI (via Fulcio short-lived certificates and Rekor
// transparency log inclusion), but all verification is local: the bundle
// is self-contained, and the trust roots are cached from Matrix state
// events.
//
// The verifier is configured from two Matrix state events:
//
//   - m.bureau.provenance_roots (schema.ProvenanceRootsContent): named
//     sets of Fulcio root certificates and Rekor public keys. Multiple
//     root sets coexist for different signing infrastructures (e.g.,
//     Sigstore public good vs. a private instance).
//
//   - m.bureau.provenance_policy (schema.ProvenancePolicyContent):
//     trusted OIDC identities and per-category enforcement levels.
//     Each identity references a named root set and specifies issuer
//     and subject/workflow glob patterns.
//
// Verification binds each trusted identity to its declared root set:
// a bundle signed under root set A cannot match an identity configured
// for root set B, even if the OIDC claims happen to match. This
// prevents cross-root-set identity confusion when multiple Fulcio
// instances accept overlapping OIDC issuers.
//
// See docs/design/provenance.md for the full design.
package provenance
