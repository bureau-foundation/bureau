// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"github.com/bureau-foundation/bureau/lib/ref"
)

// Fleet management event type constants. These events live in the
// #bureau/fleet room and are consumed by the fleet controller service
// and (for HA leases) by every daemon.
//
// Power level policy: runtime operational events (HA leases, service
// status, fleet alerts) are PL 0 — machines and fleet controllers
// write these during normal operation. Administrative configuration
// events (service definitions, machine definitions, fleet controller
// config, binary cache) are PL 100 — only operators write these
// through the CLI, and a compromised member rewriting them could
// disrupt placement, controller behavior, or binary provenance.
const (
	// EventTypeFleetService declares a service that the fleet controller
	// manages. The fleet controller reads these, evaluates placement
	// constraints against available machines, and writes PrincipalAssignment
	// events to the chosen machine config rooms.
	//
	// Admin-only (PL 100): a compromised member injecting phantom service
	// definitions could cause the fleet controller to attempt placements
	// for nonexistent services, wasting resources and potentially
	// displacing real services.
	//
	// State key: service user ID (e.g., "@bureau/fleet/prod/service/stt/whisper:server")
	// Room: #bureau/fleet:<server>
	EventTypeFleetService ref.EventType = "m.bureau.fleet_service"

	// EventTypeMachineDefinition defines a provisionable machine or
	// machine pool. The fleet controller reads these to determine what
	// machines it can bring online when existing capacity is insufficient.
	// Machines that are always on do not need definitions — they register
	// at boot and the fleet controller discovers them via heartbeat.
	//
	// Admin-only (PL 100): a compromised member injecting phantom machine
	// definitions could pollute the fleet controller's capacity model.
	//
	// State key: pool name or machine localpart (e.g., "gpu-cloud-pool"
	// or "machine/spare-workstation")
	// Room: #bureau/fleet:<server>
	EventTypeMachineDefinition ref.EventType = "m.bureau.machine_definition"

	// EventTypeFleetConfig defines global settings for a fleet controller.
	// Each fleet controller has its own config keyed by its localpart.
	//
	// Admin-only (PL 100): controls rebalance policy, pressure thresholds,
	// heartbeat interval, and preemption rules. A compromised member
	// rewriting these could trigger failover storms, constant rebalancing,
	// or unauthorized preemption.
	//
	// State key: fleet controller user ID (e.g., "@bureau/fleet/prod/service/fleet/prod:server")
	// Room: #bureau/fleet:<server>
	EventTypeFleetConfig ref.EventType = "m.bureau.fleet_config"

	// EventTypeHALease represents a lease held by a machine for hosting
	// a critical service. Daemons compete to acquire this lease when the
	// current holder goes offline. The random-backoff-plus-verify pattern
	// compensates for Matrix's last-writer-wins semantics.
	//
	// State key: service user ID (e.g., "@bureau/fleet/prod/service/fleet/prod:server")
	// Room: #bureau/fleet:<server>
	EventTypeHALease ref.EventType = "m.bureau.ha_lease"

	// EventTypeServiceStatus contains application-level metrics published
	// by a service through its proxy. The fleet controller uses these for
	// scaling decisions (e.g., high queue depth triggers replica scale-up).
	//
	// State key: service user ID
	// Room: #bureau/service:<server>
	EventTypeServiceStatus ref.EventType = "m.bureau.service_status"

	// EventTypeFleetAlert is published by the fleet controller for events
	// requiring attention: failover alerts, rebalancing proposals, capacity
	// requests, preemption requests. Timeline messages in #bureau/fleet
	// provide the audit trail.
	//
	// State key: alert ID (unique per alert)
	// Room: #bureau/fleet:<server>
	EventTypeFleetAlert ref.EventType = "m.bureau.fleet_alert"

	// EventTypeFleetCache declares the Nix binary cache that a fleet uses
	// for store path distribution. Machines configure their nix.conf to
	// trust this cache's signing keys and pull from its URL. Compose
	// pipelines push built closures to this cache by name.
	//
	// Admin-only (PL 100): controls binary provenance for every machine
	// in the fleet. A malicious rewrite — replacing the substituter URL
	// and signing keys — is a supply chain compromise affecting all
	// machines, services, and sandboxes.
	//
	// State key: "" (singleton per fleet)
	// Room: #<ns>/fleet/<name>:<server>
	EventTypeFleetCache ref.EventType = "m.bureau.fleet_cache"

	// EventTypeEnvironmentBuild records provenance for a composed
	// environment. Published by the composition pipeline's "publish"
	// step after building and pushing a Nix environment profile.
	// Establishes the chain from source (flake ref) to artifact
	// (store path) with machine and timestamp.
	//
	// Admin-only (PL 100): controls which store paths machines trust
	// as environment profiles. A malicious write could redirect
	// sandboxes to a compromised environment.
	//
	// State key: profile name (e.g., "sysadmin-runner-env")
	// Room: #<ns>/fleet/<name>:<server>
	EventTypeEnvironmentBuild ref.EventType = "m.bureau.environment_build"

	// EventTypeProvenanceRoots holds the cryptographic trust roots for
	// provenance verification. Each named root set contains the Fulcio
	// root certificate and Rekor public key needed to verify Sigstore
	// bundles. Operators publish this during fleet setup; machines sync
	// it and cache the roots locally for offline verification.
	//
	// Admin-only (PL 100): a compromised trust root is a complete
	// supply chain compromise — the attacker can sign arbitrary
	// artifacts that pass verification.
	//
	// State key: "" (singleton per fleet)
	// Room: #<ns>/fleet/<name>:<server>
	EventTypeProvenanceRoots ref.EventType = "m.bureau.provenance_roots"

	// EventTypeProvenancePolicy defines which OIDC identities the
	// fleet trusts and how strictly each artifact category enforces
	// provenance verification. The daemon reads this via /sync and
	// configures its provenance verifier accordingly.
	//
	// Admin-only (PL 100): a compromised policy could whitelist a
	// malicious signer, allowing attacker-controlled binaries,
	// models, or artifacts to pass verification.
	//
	// State key: "" (singleton per fleet)
	// Room: #<ns>/fleet/<name>:<server>
	EventTypeProvenancePolicy ref.EventType = "m.bureau.provenance_policy"
)

// MachineRoomPowerLevels returns the power level content for machine
// presence rooms (both fleet-scoped and global). Members at power level 0
// can publish machine keys, hardware info, status heartbeats, and WebRTC
// signaling. Administrative room events remain locked to admin (PL 100).
func MachineRoomPowerLevels(adminUserID ref.UserID) map[string]any {
	events := AdminProtectedEvents()
	for _, eventType := range []ref.EventType{
		EventTypeMachineKey,
		EventTypeMachineInfo,
		EventTypeMachineStatus,
		EventTypeWebRTCOffer,
		EventTypeWebRTCAnswer,
	} {
		events[eventType] = 0
	}

	return map[string]any{
		"users": map[string]any{
			adminUserID.String(): 100,
		},
		"users_default":  0,
		"events":         events,
		"events_default": 0,
		"state_default":  100,
		"ban":            100,
		"kick":           100,
		"invite":         100,
		"redact":         50,
		"notifications":  map[string]any{"room": 50},
	}
}

// ServiceRoomPowerLevels returns the power level content for service
// directory rooms (both fleet-scoped and global). Members at power level
// 0 can register and deregister services. The invite threshold is 50
// so that machine daemons (granted PL 50 during provisioning) can
// invite service principals for HA failover and fleet placement.
// Administrative room events remain locked to admin (PL 100).
func ServiceRoomPowerLevels(adminUserID ref.UserID) map[string]any {
	events := AdminProtectedEvents()
	events[EventTypeService] = 0

	return map[string]any{
		"users": map[string]any{
			adminUserID.String(): 100,
		},
		"users_default":  0,
		"events":         events,
		"events_default": 0,
		"state_default":  100,
		"ban":            100,
		"kick":           100,
		"invite":         50,
		"redact":         50,
		"notifications":  map[string]any{"room": 50},
	}
}

// FleetRoomPowerLevels returns the power level content for fleet rooms.
// Fleet rooms are admin-controlled. Members (machines, fleet controllers)
// can write runtime operational events: HA leases, service status, and
// fleet alerts. Administrative configuration — service definitions,
// machine definitions, fleet controller config, and binary cache
// config — is restricted to admin (PL 100) because these events are
// only written by operators through the CLI and a compromised member
// rewriting them could disrupt placement, failover, or binary provenance.
func FleetRoomPowerLevels(adminUserID ref.UserID) map[string]any {
	events := AdminProtectedEvents()

	// Operational state: machines and fleet controllers write these
	// during normal fleet operation. HA leases are written by daemons
	// competing for service ownership, service status is published by
	// running services, and fleet alerts are emitted by the fleet
	// controller.
	for _, eventType := range []ref.EventType{
		EventTypeHALease,
		EventTypeServiceStatus,
		EventTypeFleetAlert,
	} {
		events[eventType] = 0
	}

	// Administrative configuration: admin-only (PL 100). These events
	// are written by operators through the CLI, not by runtime
	// components. A compromised member rewriting them could inject
	// phantom services or machines into the fleet controller's model
	// (FleetService, MachineDefinition), manipulate controller behavior
	// parameters like rebalance policy and heartbeat thresholds
	// (FleetConfig), or compromise binary provenance (FleetCache).
	for _, eventType := range []ref.EventType{
		EventTypeFleetService,
		EventTypeMachineDefinition,
		EventTypeFleetConfig,
		EventTypeFleetCache,
		EventTypeEnvironmentBuild,
		EventTypeProvenanceRoots,
		EventTypeProvenancePolicy,
	} {
		events[eventType] = 100
	}

	return map[string]any{
		"users": map[string]any{
			adminUserID.String(): 100,
		},
		"users_default":  0,
		"events":         events,
		"events_default": 0,
		"state_default":  100,
		"ban":            100,
		"kick":           100,
		"invite":         100,
		"redact":         50,
		"notifications":  map[string]any{"room": 50},
	}
}

// FleetCacheContent declares the Nix binary cache that a fleet uses for
// store path distribution. Machines configure their nix.conf to trust this
// cache's signing keys and pull from its URL. Compose pipelines push built
// closures to this cache by name.
//
// This is admin-only (PL 100 in FleetRoomPowerLevels) because it controls
// binary provenance for every machine in the fleet. A malicious rewrite —
// replacing the substituter URL and signing keys — is a supply chain
// compromise affecting all machines, services, and sandboxes.
//
// This type lives in the root schema package (rather than lib/schema/fleet)
// because DaemonStatus references it, and lib/schema/fleet imports
// lib/schema — placing it there would create a circular dependency.
type FleetCacheContent struct {
	// URL is the substituter URL that machines use to pull closures.
	// This is the URL that appears in nix.conf substituters or
	// extra-substituters. Examples:
	//   "https://cache.infra.bureau.foundation"
	//   "http://attic.internal:5580/main"
	URL string `json:"url"`

	// Name is the Attic cache name used for push operations
	// (`attic push <name> <store-path>`). Empty if the cache backend
	// does not support named push (e.g., a static S3 bucket populated
	// by CI with `nix copy`).
	Name string `json:"name,omitempty"`

	// PublicKeys lists the Nix signing public keys for this cache.
	// Each key uses the standard Nix format: "name:base64-ed25519-key".
	// Machines add these to trusted-public-keys in nix.conf. Multiple
	// keys support rotation: publish the new key, wait for propagation
	// via `bureau machine doctor --fix`, then retire the old key.
	PublicKeys []string `json:"public_keys"`

	// ComposeTemplate is the template reference for the environment
	// composition pipeline sandbox. Read by `bureau environment compose`
	// as the default --template value. Set during fleet bootstrap.
	// Example: "bureau/template:nix-builder".
	ComposeTemplate string `json:"compose_template,omitempty"`

	// DefaultSystem is the default Nix system architecture for
	// environment composition (e.g., "x86_64-linux", "aarch64-linux").
	// Read by `bureau environment compose` as the default --system
	// value. When absent, --system is required on every compose
	// invocation. The code never assumes a default — it reads this
	// field or requires the CLI flag.
	DefaultSystem string `json:"default_system,omitempty"`
}

// EnvironmentBuildContent is the content of an EventTypeEnvironmentBuild
// state event. It records provenance for a composed Nix environment
// profile: which flake produced it, which system it targets, which
// store path resulted, which machine built it, and when.
//
// The state key is the profile name (e.g., "sysadmin-runner-env").
// Publishing a new build for the same profile overwrites the previous
// record — the latest build wins.
type EnvironmentBuildContent struct {
	// Profile is the Nix package name (e.g., "sysadmin-runner-env").
	// Matches the state key.
	Profile string `json:"profile"`

	// FlakeRef is the flake reference that was built (e.g.,
	// "github:bureau-foundation/bureau/abc123").
	FlakeRef string `json:"flake_ref"`

	// System is the Nix system triple (e.g., "x86_64-linux").
	System string `json:"system"`

	// StorePath is the resulting Nix store path (e.g.,
	// "/nix/store/...-bureau-sysadmin-runner-env").
	StorePath string `json:"store_path"`

	// Machine is the fleet-scoped machine that performed the build
	// (e.g., "@bureau/fleet/prod/machine/workstation:bureau.local").
	Machine ref.UserID `json:"machine"`

	// ResolvedRevision is the git commit hash the flake reference
	// resolved to at build time. Enables staleness detection: the
	// update command compares this against the current HEAD of the
	// source flake to determine whether a rebuild is needed.
	ResolvedRevision string `json:"resolved_revision,omitempty"`

	// Timestamp is the RFC 3339 build completion time.
	Timestamp string `json:"timestamp"`
}

// ProvenanceRootsContent holds the cryptographic trust roots for
// provenance verification. Each named root set contains the
// certificates and public keys needed to verify Sigstore bundles
// signed under that root. Multiple root sets coexist — for example,
// "sigstore_public" for open-source dependencies signed via Sigstore's
// public infrastructure and "fleet_private" for attestations signed
// by a private Sigstore instance.
//
// See provenance.md for the full verification model.
type ProvenanceRootsContent struct {
	// Roots maps root set names to their trust anchors. Each
	// TrustedIdentity in the provenance policy references a root set
	// by name.
	Roots map[string]ProvenanceTrustRoot `json:"roots"`
}

// ProvenanceTrustRoot is a set of trust anchors for verifying Sigstore
// bundles. The Fulcio root certificate is the CA that issues short-lived
// signing certificates from OIDC tokens. The Rekor public key verifies
// transparency log inclusion proofs. Both are PEM-encoded.
type ProvenanceTrustRoot struct {
	// TUFRootVersion tracks which TUF (The Update Framework) snapshot
	// these roots were extracted from, for roots sourced from
	// Sigstore's public infrastructure. Zero for private instances
	// where TUF is not used.
	TUFRootVersion int `json:"tuf_root_version,omitempty"`

	// FulcioRootPEM is the PEM-encoded X.509 root certificate for
	// Sigstore's certificate authority. Signing certificates in
	// provenance bundles must chain to this root.
	FulcioRootPEM string `json:"fulcio_root_pem"`

	// RekorPublicKeyPEM is the PEM-encoded public key for the
	// transparency log. Inclusion proofs in provenance bundles are
	// verified against this key.
	RekorPublicKeyPEM string `json:"rekor_public_key_pem"`
}

// ProvenancePolicyContent defines which OIDC identities the fleet
// trusts for provenance verification and how strictly each artifact
// category enforces verification. The daemon reads this via /sync and
// configures its provenance verifier.
//
// See provenance.md for the full policy model, enforcement categories,
// and verification points.
type ProvenancePolicyContent struct {
	// TrustedIdentities lists the OIDC signers this fleet accepts.
	// A provenance bundle is verified if its Fulcio certificate
	// chains to the referenced trust root and its OIDC claims match
	// at least one trusted identity's patterns.
	TrustedIdentities []TrustedIdentity `json:"trusted_identities"`

	// Enforcement maps artifact categories to enforcement levels.
	// Well-known categories: "nix_store_paths", "artifacts",
	// "models", "forge_artifacts", "templates". Missing categories
	// default to EnforcementLog. Unrecognized categories are ignored
	// (forward-compatible).
	Enforcement map[string]EnforcementLevel `json:"enforcement"`
}

// TrustedIdentity defines an OIDC signer that the fleet accepts for
// provenance verification. All pattern fields use glob matching.
type TrustedIdentity struct {
	// Name is a human-readable label for this identity (e.g.,
	// "bureau-ci", "partner-models"). Appears in verification
	// results and audit logs.
	Name string `json:"name"`

	// Roots references a named root set in ProvenanceRootsContent.
	// The bundle's Fulcio certificate must chain to this root set
	// for the identity to match.
	Roots string `json:"roots"`

	// Issuer is the expected OIDC issuer URL in the Fulcio
	// certificate. For GitHub Actions:
	// "https://token.actions.githubusercontent.com".
	Issuer string `json:"issuer"`

	// SubjectPattern is a glob matched against the OIDC subject
	// claim in the Fulcio certificate. For GitHub Actions, the
	// subject has the form "repo:<owner>/<repo>:ref:<ref>".
	SubjectPattern string `json:"subject_pattern"`

	// WorkflowPattern is an optional glob matched against the
	// GitHub Actions workflow path claim. Empty means any workflow
	// matches.
	WorkflowPattern string `json:"workflow_pattern,omitempty"`
}

// EnforcementLevel controls how provenance verification results are
// handled at each verification point.
type EnforcementLevel string

const (
	// EnforcementRequire rejects artifacts that fail provenance
	// verification or have no provenance bundle.
	EnforcementRequire EnforcementLevel = "require"

	// EnforcementWarn accepts unverified artifacts but publishes a
	// warning event to the fleet room.
	EnforcementWarn EnforcementLevel = "warn"

	// EnforcementLog accepts unverified artifacts with a log entry
	// only. No operator-visible warning.
	EnforcementLog EnforcementLevel = "log"
)

// IsKnown reports whether the enforcement level is a recognized value.
func (e EnforcementLevel) IsKnown() bool {
	switch e {
	case EnforcementRequire, EnforcementWarn, EnforcementLog:
		return true
	default:
		return false
	}
}
