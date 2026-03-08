// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// TestFleetProvenanceRootsAndPolicy exercises the bureau fleet provenance
// CLI commands end-to-end against a real homeserver: roots show (empty),
// roots set (from Sigstore trusted_root.json), roots show (populated),
// roots set (second root set, verifying read-modify-write preserves the
// first), policy set, and policy show.
func TestFleetProvenanceRootsAndPolicy(t *testing.T) {
	t.Parallel()

	ns := setupTestNamespace(t)
	admin := ns.Admin
	fleet := createTestFleet(t, admin, ns)
	ctx := t.Context()

	credentialFile := writeTestCredentialFile(t,
		testHomeserverURL, admin.UserID().String(), admin.AccessToken())

	trustedRootFile := resolvedDataFile(t, "TRUSTED_ROOT_FIXTURE")

	// ---- roots show: empty state ----

	rootsShowEmpty := runBureauOrFail(t, "fleet", "provenance", "roots", "show",
		fleet.Prefix, "--credential-file", credentialFile, "--json")

	var emptyRootsResult struct {
		Fleet ref.Fleet                     `json:"fleet"`
		Roots schema.ProvenanceRootsContent `json:"roots"`
	}
	if err := json.Unmarshal([]byte(rootsShowEmpty), &emptyRootsResult); err != nil {
		t.Fatalf("unmarshal empty roots show: %v\noutput: %s", err, rootsShowEmpty)
	}
	if len(emptyRootsResult.Roots.Roots) != 0 {
		t.Errorf("expected no roots before configuration, got %d", len(emptyRootsResult.Roots.Roots))
	}

	// ---- roots set: import Sigstore trusted root ----

	rootsSetOutput := runBureauOrFail(t, "fleet", "provenance", "roots", "set",
		fleet.Prefix,
		"--name", "sigstore_public",
		"--sigstore-trusted-root", trustedRootFile,
		"--credential-file", credentialFile, "--json")

	var setRootsResult struct {
		Fleet ref.Fleet                     `json:"fleet"`
		Roots schema.ProvenanceRootsContent `json:"roots"`
	}
	if err := json.Unmarshal([]byte(rootsSetOutput), &setRootsResult); err != nil {
		t.Fatalf("unmarshal roots set: %v\noutput: %s", err, rootsSetOutput)
	}
	if len(setRootsResult.Roots.Roots) != 1 {
		t.Fatalf("expected 1 root set after first set, got %d", len(setRootsResult.Roots.Roots))
	}
	sigstoreRoot, exists := setRootsResult.Roots.Roots["sigstore_public"]
	if !exists {
		t.Fatal("root set 'sigstore_public' not found in set result")
	}
	if sigstoreRoot.FulcioRootPEM == "" {
		t.Error("sigstore_public FulcioRootPEM is empty")
	}
	if sigstoreRoot.RekorPublicKeyPEM == "" {
		t.Error("sigstore_public RekorPublicKeyPEM is empty")
	}

	// ---- roots show: populated state ----

	rootsShowPopulated := runBureauOrFail(t, "fleet", "provenance", "roots", "show",
		fleet.Prefix, "--credential-file", credentialFile, "--json")

	var populatedRootsResult struct {
		Fleet ref.Fleet                     `json:"fleet"`
		Roots schema.ProvenanceRootsContent `json:"roots"`
	}
	if err := json.Unmarshal([]byte(rootsShowPopulated), &populatedRootsResult); err != nil {
		t.Fatalf("unmarshal populated roots show: %v\noutput: %s", err, rootsShowPopulated)
	}
	if len(populatedRootsResult.Roots.Roots) != 1 {
		t.Errorf("expected 1 root set, got %d", len(populatedRootsResult.Roots.Roots))
	}

	// ---- roots set: second root set (read-modify-write) ----

	// Set a second root set using the same fixture. Verify the first
	// is preserved (read-modify-write, not overwrite).
	runBureauOrFail(t, "fleet", "provenance", "roots", "set",
		fleet.Prefix,
		"--name", "fleet_private",
		"--sigstore-trusted-root", trustedRootFile,
		"--credential-file", credentialFile, "--json")

	rootsShowBoth := runBureauOrFail(t, "fleet", "provenance", "roots", "show",
		fleet.Prefix, "--credential-file", credentialFile, "--json")

	var bothRootsResult struct {
		Roots schema.ProvenanceRootsContent `json:"roots"`
	}
	if err := json.Unmarshal([]byte(rootsShowBoth), &bothRootsResult); err != nil {
		t.Fatalf("unmarshal both roots show: %v\noutput: %s", err, rootsShowBoth)
	}
	if len(bothRootsResult.Roots.Roots) != 2 {
		t.Fatalf("expected 2 root sets after second set, got %d", len(bothRootsResult.Roots.Roots))
	}
	if _, exists := bothRootsResult.Roots.Roots["sigstore_public"]; !exists {
		t.Error("first root set 'sigstore_public' was not preserved after second set")
	}
	if _, exists := bothRootsResult.Roots.Roots["fleet_private"]; !exists {
		t.Error("second root set 'fleet_private' not found")
	}

	// ---- policy set ----

	// Write a policy file referencing the root sets we just created.
	policy := schema.ProvenancePolicyContent{
		TrustedIdentities: []schema.TrustedIdentity{
			{
				Name:           "github_actions",
				Roots:          "sigstore_public",
				Issuer:         "https://token.actions.githubusercontent.com",
				SubjectPattern: "https://github.com/bureau-foundation/bureau/*",
			},
		},
		Enforcement: map[string]schema.EnforcementLevel{
			"nix_store_paths": schema.EnforcementWarn,
		},
	}
	policyData, err := json.Marshal(policy)
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	policyFile := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(policyFile, policyData, 0o644); err != nil {
		t.Fatalf("write policy file: %v", err)
	}

	policySetOutput := runBureauOrFail(t, "fleet", "provenance", "policy", "set",
		fleet.Prefix,
		"--policy-file", policyFile,
		"--credential-file", credentialFile, "--json")

	var setPolicyResult struct {
		Fleet  ref.Fleet                      `json:"fleet"`
		Policy schema.ProvenancePolicyContent `json:"policy"`
	}
	if err := json.Unmarshal([]byte(policySetOutput), &setPolicyResult); err != nil {
		t.Fatalf("unmarshal policy set: %v\noutput: %s", err, policySetOutput)
	}
	if len(setPolicyResult.Policy.TrustedIdentities) != 1 {
		t.Errorf("expected 1 trusted identity, got %d", len(setPolicyResult.Policy.TrustedIdentities))
	}
	if setPolicyResult.Policy.TrustedIdentities[0].Name != "github_actions" {
		t.Errorf("identity name = %q, want %q",
			setPolicyResult.Policy.TrustedIdentities[0].Name, "github_actions")
	}

	// ---- policy show ----

	policyShowOutput := runBureauOrFail(t, "fleet", "provenance", "policy", "show",
		fleet.Prefix, "--credential-file", credentialFile, "--json")

	var showPolicyResult struct {
		Policy schema.ProvenancePolicyContent `json:"policy"`
	}
	if err := json.Unmarshal([]byte(policyShowOutput), &showPolicyResult); err != nil {
		t.Fatalf("unmarshal policy show: %v\noutput: %s", err, policyShowOutput)
	}
	if len(showPolicyResult.Policy.TrustedIdentities) != 1 {
		t.Errorf("expected 1 trusted identity from show, got %d",
			len(showPolicyResult.Policy.TrustedIdentities))
	}
	enforcement, exists := showPolicyResult.Policy.Enforcement["nix_store_paths"]
	if !exists {
		t.Error("enforcement for nix_store_paths not found in policy show")
	} else if enforcement != schema.EnforcementWarn {
		t.Errorf("enforcement = %q, want %q", enforcement, schema.EnforcementWarn)
	}

	// ---- Verify via Matrix API directly ----

	roots, err := messaging.GetState[schema.ProvenanceRootsContent](
		ctx, admin, fleet.FleetRoomID, schema.EventTypeProvenanceRoots, "")
	if err != nil {
		t.Fatalf("GetState provenance roots: %v", err)
	}
	if len(roots.Roots) != 2 {
		t.Errorf("Matrix state roots count = %d, want 2", len(roots.Roots))
	}

	matrixPolicy, err := messaging.GetState[schema.ProvenancePolicyContent](
		ctx, admin, fleet.FleetRoomID, schema.EventTypeProvenancePolicy, "")
	if err != nil {
		t.Fatalf("GetState provenance policy: %v", err)
	}
	if len(matrixPolicy.TrustedIdentities) != 1 {
		t.Errorf("Matrix state trusted identities = %d, want 1", len(matrixPolicy.TrustedIdentities))
	}
	if matrixPolicy.TrustedIdentities[0].Issuer != "https://token.actions.githubusercontent.com" {
		t.Errorf("Matrix state issuer = %q, want GitHub Actions",
			matrixPolicy.TrustedIdentities[0].Issuer)
	}
}

// resolvedDataFile resolves a Bazel runfile path from an environment
// variable. Same pattern as resolvedBinary but for data files.
func resolvedDataFile(t *testing.T, envVar string) string {
	t.Helper()

	rlocationPath := os.Getenv(envVar)
	if rlocationPath == "" {
		t.Skipf("%s not set (run via Bazel to provide test data)", envVar)
	}

	runfilesDirectory := os.Getenv("RUNFILES_DIR")
	if runfilesDirectory == "" {
		t.Skipf("%s set but RUNFILES_DIR missing", envVar)
	}

	absolutePath := filepath.Join(runfilesDirectory, rlocationPath)
	if _, err := os.Stat(absolutePath); err != nil {
		t.Skipf("data file not found at %s: %v", absolutePath, err)
	}
	return absolutePath
}
