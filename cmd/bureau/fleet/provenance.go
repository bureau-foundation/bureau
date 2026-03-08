// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/provenance"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// ---- Command tree --------------------------------------------------------

func provenanceCommand() *cli.Command {
	return &cli.Command{
		Name:    "provenance",
		Summary: "Manage provenance trust roots, policy, and verification",
		Description: `Commands for managing Sigstore provenance verification in a fleet.

Provenance verification cryptographically proves that binaries, artifacts,
and models crossing the fleet's trust boundary were produced by authorized
CI pipelines. Trust roots define the Fulcio CA and Rekor public key used
to verify bundles. Policy defines which OIDC identities are trusted and
how strictly each artifact category enforces verification.

Use "roots set" to publish trust roots (from Sigstore's public
trusted_root.json or from your own PEM files), "policy set" to define
trusted identities and enforcement levels, and "verify" to test
verification of a specific artifact.`,
		Subcommands: []*cli.Command{
			rootsCommand(),
			policyCommand(),
			verifyCommand(),
		},
		Examples: []cli.Example{
			{
				Description: "Import Sigstore public good trust roots",
				Command:     "bureau fleet provenance roots set bureau/fleet/prod --name sigstore_public --sigstore-trusted-root trusted_root.json --credential-file ./creds",
			},
			{
				Description: "View trust roots",
				Command:     "bureau fleet provenance roots show bureau/fleet/prod --credential-file ./creds",
			},
			{
				Description: "Set provenance policy",
				Command:     "bureau fleet provenance policy set bureau/fleet/prod --policy-file policy.json --credential-file ./creds",
			},
		},
	}
}

func rootsCommand() *cli.Command {
	return &cli.Command{
		Name:    "roots",
		Summary: "Manage provenance trust roots",
		Subcommands: []*cli.Command{
			rootsShowCommand(),
			rootsSetCommand(),
		},
	}
}

func policyCommand() *cli.Command {
	return &cli.Command{
		Name:    "policy",
		Summary: "Manage provenance policy",
		Subcommands: []*cli.Command{
			policyShowCommand(),
			policySetCommand(),
		},
	}
}

// ---- roots show ----------------------------------------------------------

type rootsShowParams struct {
	cli.SessionConfig
	cli.JSONOutput
}

type rootsShowResult struct {
	Fleet ref.Fleet                     `json:"fleet" desc:"fleet reference"`
	Roots schema.ProvenanceRootsContent `json:"roots" desc:"trust root sets"`
}

func rootsShowCommand() *cli.Command {
	var params rootsShowParams

	return &cli.Command{
		Name:    "show",
		Summary: "Display provenance trust roots for a fleet",
		Description: `Show the current provenance trust roots configured in the fleet room.

Each root set contains a Fulcio CA certificate (for verifying signing
certificates) and a Rekor public key (for verifying transparency log
entries). Root sets are named — a fleet can have multiple root sets
for different signing environments (e.g., "sigstore_public" for
open-source CI and "fleet_private" for internal builds).`,
		Usage: "bureau fleet provenance roots show <namespace/fleet/name> [flags]",
		Examples: []cli.Example{
			{
				Description: "Show trust roots",
				Command:     "bureau fleet provenance roots show bureau/fleet/prod --credential-file ./creds",
			},
			{
				Description: "Show trust roots as JSON",
				Command:     "bureau fleet provenance roots show bureau/fleet/prod --json --credential-file ./creds",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &rootsShowResult{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/fleet/provenance/roots/show"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) == 0 {
				return cli.Validation("fleet localpart is required (e.g., bureau/fleet/prod)")
			}
			if len(args) > 1 {
				return cli.Validation("expected exactly one argument (fleet localpart), got %d", len(args))
			}
			return runRootsShow(ctx, logger, args[0], &params)
		},
	}
}

func runRootsShow(ctx context.Context, logger *slog.Logger, fleetLocalpart string, params *rootsShowParams) error {
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

	roots, err := messaging.GetState[schema.ProvenanceRootsContent](ctx, session, fleetRoomID, schema.EventTypeProvenanceRoots, "")
	if err != nil && !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
		return cli.Transient("reading provenance roots from fleet room: %w", err)
	}

	result := rootsShowResult{
		Fleet: fleet,
		Roots: roots,
	}

	if done, emitError := params.EmitJSON(result); done {
		return emitError
	}

	if len(roots.Roots) == 0 {
		fmt.Fprintf(os.Stdout, "No provenance trust roots configured for fleet %s.\n", fleet.Localpart())
		fmt.Fprintf(os.Stdout, "  Run: bureau fleet provenance roots set %s --name <name> --sigstore-trusted-root <file>\n", fleetLocalpart)
		return nil
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(writer, "Provenance Trust Roots: %s\n\n", fleet.Localpart())
	for name, root := range roots.Roots {
		fmt.Fprintf(writer, "  %s:\n", name)
		if root.TUFRootVersion > 0 {
			fmt.Fprintf(writer, "    TUF Version:\t%d\n", root.TUFRootVersion)
		}
		fmt.Fprintf(writer, "    Fulcio Root:\t%s\n", describePEMCertificates(root.FulcioRootPEM))
		fmt.Fprintf(writer, "    Rekor Key:\t%s\n", describePEMPublicKey(root.RekorPublicKeyPEM))
	}
	writer.Flush()
	return nil
}

// ---- roots set -----------------------------------------------------------

type rootsSetParams struct {
	cli.SessionConfig
	cli.JSONOutput
	Name                string `json:"name"                  flag:"name"                  desc:"name for this root set (e.g., sigstore_public, fleet_private)" required:"true"`
	FulcioRootPEM       string `json:"fulcio_root_pem"       flag:"fulcio-root-pem"       desc:"path to Fulcio root CA certificate PEM file"`
	RekorPublicKeyPEM   string `json:"rekor_public_key_pem"  flag:"rekor-public-key-pem"  desc:"path to Rekor public key PEM file"`
	SigstoreTrustedRoot string `json:"sigstore_trusted_root" flag:"sigstore-trusted-root" desc:"path to Sigstore trusted_root.json file (alternative to individual PEM files)"`
}

type rootsSetResult struct {
	Fleet ref.Fleet                     `json:"fleet" desc:"fleet reference"`
	Roots schema.ProvenanceRootsContent `json:"roots" desc:"trust root sets after update"`
}

func rootsSetCommand() *cli.Command {
	var params rootsSetParams

	return &cli.Command{
		Name:    "set",
		Summary: "Publish or update a provenance trust root set",
		Description: `Add or replace a named trust root set in the fleet room.

Trust roots can be provided in two ways:

  1. A Sigstore trusted_root.json file (--sigstore-trusted-root), which
     is the format distributed by Sigstore's TUF repository. Download it
     from https://github.com/sigstore/root-signing and pass it directly.

  2. Individual PEM files (--fulcio-root-pem and --rekor-public-key-pem)
     for private Sigstore instances or custom trust material.

The --name flag identifies the root set. Multiple root sets can coexist
in a fleet — for example, "sigstore_public" for open-source dependencies
and "fleet_private" for internal builds. Each trusted identity in the
provenance policy references which root set it uses.

This command performs a read-modify-write: it reads the current roots,
adds or replaces the named root set, and publishes the complete set back.
Other root sets are preserved.`,
		Usage: "bureau fleet provenance roots set <namespace/fleet/name> [flags]",
		Examples: []cli.Example{
			{
				Description: "Import Sigstore public good trust roots",
				Command:     "bureau fleet provenance roots set bureau/fleet/prod --name sigstore_public --sigstore-trusted-root trusted_root.json --credential-file ./creds",
			},
			{
				Description: "Set trust roots from individual PEM files",
				Command:     "bureau fleet provenance roots set bureau/fleet/prod --name fleet_private --fulcio-root-pem fulcio-root.pem --rekor-public-key-pem rekor.pub --credential-file ./creds",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &rootsSetResult{} },
		Annotations:    cli.Idempotent(),
		RequiredGrants: []string{"command/fleet/provenance/roots/set"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) == 0 {
				return cli.Validation("fleet localpart is required (e.g., bureau/fleet/prod)")
			}
			if len(args) > 1 {
				return cli.Validation("expected exactly one argument (fleet localpart), got %d", len(args))
			}
			return runRootsSet(ctx, logger, args[0], &params)
		},
	}
}

func runRootsSet(ctx context.Context, logger *slog.Logger, fleetLocalpart string, params *rootsSetParams) error {
	if params.Name == "" {
		return cli.Validation("--name is required (e.g., sigstore_public)")
	}

	// Validate flag mutual exclusivity.
	hasPEMFlags := params.FulcioRootPEM != "" || params.RekorPublicKeyPEM != ""
	hasTrustedRoot := params.SigstoreTrustedRoot != ""

	if hasPEMFlags && hasTrustedRoot {
		return cli.Validation("--fulcio-root-pem/--rekor-public-key-pem and --sigstore-trusted-root are mutually exclusive")
	}
	if !hasPEMFlags && !hasTrustedRoot {
		return cli.Validation("provide either --sigstore-trusted-root or both --fulcio-root-pem and --rekor-public-key-pem")
	}
	if hasPEMFlags && (params.FulcioRootPEM == "" || params.RekorPublicKeyPEM == "") {
		return cli.Validation("both --fulcio-root-pem and --rekor-public-key-pem are required when using individual PEM files")
	}

	// Build the trust root from the provided input.
	var trustRoot schema.ProvenanceTrustRoot

	if hasTrustedRoot {
		data, err := os.ReadFile(params.SigstoreTrustedRoot)
		if err != nil {
			return cli.Validation("cannot read --sigstore-trusted-root %q: %v", params.SigstoreTrustedRoot, err)
		}
		fulcioPEM, rekorPEM, err := provenance.ParseTrustedRootPEMs(data)
		if err != nil {
			return cli.Validation("invalid Sigstore trusted root: %v", err)
		}
		trustRoot.FulcioRootPEM = fulcioPEM
		trustRoot.RekorPublicKeyPEM = rekorPEM
	} else {
		fulcioData, err := os.ReadFile(params.FulcioRootPEM)
		if err != nil {
			return cli.Validation("cannot read --fulcio-root-pem %q: %v", params.FulcioRootPEM, err)
		}
		rekorData, err := os.ReadFile(params.RekorPublicKeyPEM)
		if err != nil {
			return cli.Validation("cannot read --rekor-public-key-pem %q: %v", params.RekorPublicKeyPEM, err)
		}
		trustRoot.FulcioRootPEM = string(fulcioData)
		trustRoot.RekorPublicKeyPEM = string(rekorData)
	}

	// Validate the PEM material parses correctly by constructing a
	// verifier. NewVerifier calls parseTrustRoot internally, which
	// validates PEM decoding, X.509 parsing, and key type.
	testRoots := schema.ProvenanceRootsContent{
		Roots: map[string]schema.ProvenanceTrustRoot{
			params.Name: trustRoot,
		},
	}
	if _, err := provenance.NewVerifier(testRoots, schema.ProvenancePolicyContent{}); err != nil {
		return cli.Validation("invalid trust root material: %v", err)
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

	// Read-modify-write: read existing roots, merge in the new root set.
	roots, err := messaging.GetState[schema.ProvenanceRootsContent](ctx, session, fleetRoomID, schema.EventTypeProvenanceRoots, "")
	if err != nil && !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
		return cli.Transient("reading provenance roots from fleet room: %w", err)
	}
	if roots.Roots == nil {
		roots.Roots = make(map[string]schema.ProvenanceTrustRoot)
	}
	roots.Roots[params.Name] = trustRoot

	// Publish.
	_, err = session.SendStateEvent(ctx, fleetRoomID, schema.EventTypeProvenanceRoots, "", roots)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeForbidden) {
			return cli.Forbidden("cannot publish provenance roots to %s: %w", fleetRoomID, err).
				WithHint("Provenance root configuration requires admin power level (PL 100) in the fleet room.")
		}
		return cli.Transient("publishing provenance roots to %s: %w", fleetRoomID, err)
	}

	result := rootsSetResult{
		Fleet: fleet,
		Roots: roots,
	}

	if done, emitError := params.EmitJSON(result); done {
		return emitError
	}

	logger.Info("updated provenance trust roots",
		"fleet", fleet.Localpart(),
		"root_set", params.Name,
		"root_sets", len(roots.Roots),
	)
	return nil
}

// ---- policy show ---------------------------------------------------------

type policyShowParams struct {
	cli.SessionConfig
	cli.JSONOutput
}

type policyShowResult struct {
	Fleet  ref.Fleet                      `json:"fleet"  desc:"fleet reference"`
	Policy schema.ProvenancePolicyContent `json:"policy" desc:"provenance policy"`
}

func policyShowCommand() *cli.Command {
	var params policyShowParams

	return &cli.Command{
		Name:    "show",
		Summary: "Display provenance policy for a fleet",
		Description: `Show the current provenance policy configured in the fleet room.

The policy defines two things:
  1. Trusted identities — which OIDC signers (CI pipelines) the fleet
     accepts. Each identity specifies an issuer URL, subject pattern,
     and which trust root set to verify against.
  2. Enforcement levels — how strictly each artifact category enforces
     verification (require, warn, or log).`,
		Usage: "bureau fleet provenance policy show <namespace/fleet/name> [flags]",
		Examples: []cli.Example{
			{
				Description: "Show provenance policy",
				Command:     "bureau fleet provenance policy show bureau/fleet/prod --credential-file ./creds",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &policyShowResult{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/fleet/provenance/policy/show"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) == 0 {
				return cli.Validation("fleet localpart is required (e.g., bureau/fleet/prod)")
			}
			if len(args) > 1 {
				return cli.Validation("expected exactly one argument (fleet localpart), got %d", len(args))
			}
			return runPolicyShow(ctx, logger, args[0], &params)
		},
	}
}

func runPolicyShow(ctx context.Context, logger *slog.Logger, fleetLocalpart string, params *policyShowParams) error {
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

	policy, err := messaging.GetState[schema.ProvenancePolicyContent](ctx, session, fleetRoomID, schema.EventTypeProvenancePolicy, "")
	if err != nil && !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
		return cli.Transient("reading provenance policy from fleet room: %w", err)
	}

	result := policyShowResult{
		Fleet:  fleet,
		Policy: policy,
	}

	if done, emitError := params.EmitJSON(result); done {
		return emitError
	}

	if len(policy.TrustedIdentities) == 0 && len(policy.Enforcement) == 0 {
		fmt.Fprintf(os.Stdout, "No provenance policy configured for fleet %s.\n", fleet.Localpart())
		fmt.Fprintf(os.Stdout, "  Run: bureau fleet provenance policy set %s --policy-file <file>\n", fleetLocalpart)
		return nil
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(writer, "Provenance Policy: %s\n\n", fleet.Localpart())

	if len(policy.TrustedIdentities) > 0 {
		fmt.Fprintf(writer, "  Trusted Identities:\n")
		for _, identity := range policy.TrustedIdentities {
			fmt.Fprintf(writer, "    %s:\n", identity.Name)
			fmt.Fprintf(writer, "      Roots:\t%s\n", identity.Roots)
			fmt.Fprintf(writer, "      Issuer:\t%s\n", identity.Issuer)
			fmt.Fprintf(writer, "      Subject:\t%s\n", identity.SubjectPattern)
			if identity.WorkflowPattern != "" {
				fmt.Fprintf(writer, "      Workflow:\t%s\n", identity.WorkflowPattern)
			}
		}
		fmt.Fprintln(writer)
	}

	if len(policy.Enforcement) > 0 {
		fmt.Fprintf(writer, "  Enforcement:\n")
		for category, level := range policy.Enforcement {
			fmt.Fprintf(writer, "    %s:\t%s\n", category, level)
		}
	}

	writer.Flush()
	return nil
}

// ---- policy set ----------------------------------------------------------

type policySetParams struct {
	cli.SessionConfig
	cli.JSONOutput
	PolicyFile string `json:"policy_file" flag:"policy-file" desc:"path to JSON file containing the provenance policy" required:"true"`
}

type policySetResult struct {
	Fleet  ref.Fleet                      `json:"fleet"  desc:"fleet reference"`
	Policy schema.ProvenancePolicyContent `json:"policy" desc:"provenance policy after update"`
}

func policySetCommand() *cli.Command {
	var params policySetParams

	return &cli.Command{
		Name:    "set",
		Summary: "Publish provenance policy for a fleet",
		Description: `Publish a provenance policy to the fleet room.

The policy is provided as a JSON file with two sections:

  "trusted_identities": a list of OIDC signers the fleet accepts.
  Each identity specifies a name, root set reference, issuer URL,
  subject pattern (glob), and optional workflow pattern (glob).

  "enforcement": a map of artifact categories to enforcement levels.
  Levels: "require" (reject unverified), "warn" (accept with warning),
  "log" (accept silently). Categories: nix_store_paths, artifacts,
  models, forge_artifacts, templates.

This command performs a full replace of the policy. The policy is a
coherent unit — identities reference root sets and enforcement levels
apply fleet-wide. Partial merges risk inconsistency.

The command validates that every trusted identity references an existing
root set. Set trust roots first with "roots set".`,
		Usage: "bureau fleet provenance policy set <namespace/fleet/name> [flags]",
		Examples: []cli.Example{
			{
				Description: "Set provenance policy from a JSON file",
				Command:     "bureau fleet provenance policy set bureau/fleet/prod --policy-file policy.json --credential-file ./creds",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &policySetResult{} },
		Annotations:    cli.Idempotent(),
		RequiredGrants: []string{"command/fleet/provenance/policy/set"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) == 0 {
				return cli.Validation("fleet localpart is required (e.g., bureau/fleet/prod)")
			}
			if len(args) > 1 {
				return cli.Validation("expected exactly one argument (fleet localpart), got %d", len(args))
			}
			return runPolicySet(ctx, logger, args[0], &params)
		},
	}
}

func runPolicySet(ctx context.Context, logger *slog.Logger, fleetLocalpart string, params *policySetParams) error {
	if params.PolicyFile == "" {
		return cli.Validation("--policy-file is required")
	}

	// Read and parse the policy file.
	data, err := os.ReadFile(params.PolicyFile)
	if err != nil {
		return cli.Validation("cannot read --policy-file %q: %v", params.PolicyFile, err)
	}

	var policy schema.ProvenancePolicyContent
	if err := json.Unmarshal(data, &policy); err != nil {
		return cli.Validation("invalid policy JSON: %v", err)
	}

	// Validate enforcement levels.
	for category, level := range policy.Enforcement {
		if !level.IsKnown() {
			return cli.Validation("unknown enforcement level %q for category %q (valid: require, warn, log)", level, category)
		}
	}

	// Validate identity names are non-empty and root references are non-empty.
	for index, identity := range policy.TrustedIdentities {
		if identity.Name == "" {
			return cli.Validation("trusted identity at index %d has empty name", index)
		}
		if identity.Roots == "" {
			return cli.Validation("trusted identity %q has empty roots reference", identity.Name)
		}
		if identity.Issuer == "" {
			return cli.Validation("trusted identity %q has empty issuer", identity.Name)
		}
		if identity.SubjectPattern == "" {
			return cli.Validation("trusted identity %q has empty subject_pattern", identity.Name)
		}
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

	// Cross-validate: ensure every identity references an existing root set.
	roots, err := messaging.GetState[schema.ProvenanceRootsContent](ctx, session, fleetRoomID, schema.EventTypeProvenanceRoots, "")
	if err != nil && !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
		return cli.Transient("reading provenance roots from fleet room: %w", err)
	}

	if _, verifierError := provenance.NewVerifier(roots, policy); verifierError != nil {
		if len(roots.Roots) == 0 {
			return cli.Validation("cannot validate policy: no trust roots configured").
				WithHint("Run 'bureau fleet provenance roots set' first to configure trust roots.")
		}
		return cli.Validation("policy validation failed: %v", verifierError)
	}

	// Publish (full replace).
	_, err = session.SendStateEvent(ctx, fleetRoomID, schema.EventTypeProvenancePolicy, "", policy)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeForbidden) {
			return cli.Forbidden("cannot publish provenance policy to %s: %w", fleetRoomID, err).
				WithHint("Provenance policy configuration requires admin power level (PL 100) in the fleet room.")
		}
		return cli.Transient("publishing provenance policy to %s: %w", fleetRoomID, err)
	}

	result := policySetResult{
		Fleet:  fleet,
		Policy: policy,
	}

	if done, emitError := params.EmitJSON(result); done {
		return emitError
	}

	logger.Info("updated provenance policy",
		"fleet", fleet.Localpart(),
		"trusted_identities", len(policy.TrustedIdentities),
		"enforcement_categories", len(policy.Enforcement),
	)
	return nil
}

// ---- Shared helpers ------------------------------------------------------

// resolveFleetRoom connects to Matrix, extracts the server name,
// parses the fleet reference, and resolves the fleet room alias to
// a room ID. This is the standard preamble shared by all provenance
// commands.
func resolveFleetRoom(ctx context.Context, session messaging.Session, fleetLocalpart string) (ref.Fleet, ref.RoomID, error) {
	server, err := ref.ServerFromUserID(session.UserID().String())
	if err != nil {
		return ref.Fleet{}, ref.RoomID{}, cli.Internal("cannot determine server name from session: %w", err)
	}

	fleet, err := ref.ParseFleet(fleetLocalpart, server)
	if err != nil {
		return ref.Fleet{}, ref.RoomID{}, cli.Validation("%v", err)
	}

	fleetRoomID, err := session.ResolveAlias(ctx, fleet.RoomAlias())
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			return ref.Fleet{}, ref.RoomID{}, cli.NotFound("fleet room %s not found", fleet.RoomAlias()).
				WithHint("Has the fleet been created? Run 'bureau fleet create' first.")
		}
		return ref.Fleet{}, ref.RoomID{}, cli.Transient("resolving fleet room %s: %w", fleet.RoomAlias(), err)
	}

	return fleet, fleetRoomID, nil
}

// describePEMCertificates returns a human-readable summary of the
// certificates in a PEM string (subject CN, SHA-256 fingerprint prefix).
func describePEMCertificates(pemData string) string {
	if pemData == "" {
		return "(not configured)"
	}

	var descriptions []string
	remaining := []byte(pemData)
	for {
		var block *pem.Block
		block, remaining = pem.Decode(remaining)
		if block == nil {
			break
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			descriptions = append(descriptions, "(invalid certificate)")
			continue
		}
		fingerprint := sha256.Sum256(block.Bytes)
		description := fmt.Sprintf("%s (sha256:%x…)",
			cert.Subject.CommonName, fingerprint[:8])
		descriptions = append(descriptions, description)
	}

	if len(descriptions) == 0 {
		return "(invalid PEM)"
	}
	return strings.Join(descriptions, ", ")
}

// describePEMPublicKey returns a human-readable summary of a PEM public key.
func describePEMPublicKey(pemData string) string {
	if pemData == "" {
		return "(not configured)"
	}

	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		return "(invalid PEM)"
	}

	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return "(invalid public key)"
	}

	fingerprint := sha256.Sum256(block.Bytes)
	return fmt.Sprintf("%T (sha256:%x…)", key, fingerprint[:8])
}
