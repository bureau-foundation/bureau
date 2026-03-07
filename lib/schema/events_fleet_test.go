// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"encoding/json"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
)

func mustUserID(t *testing.T, raw string) ref.UserID {
	t.Helper()
	userID, err := ref.ParseUserID(raw)
	if err != nil {
		t.Fatalf("parse user ID %q: %v", raw, err)
	}
	return userID
}

func TestMachineRoomPowerLevels(t *testing.T) {
	adminUserID := mustUserID(t, "@bureau-admin:bureau.local")
	levels := MachineRoomPowerLevels(adminUserID)

	users, ok := levels["users"].(map[string]any)
	if !ok {
		t.Fatal("power levels missing 'users' map")
	}
	if users[adminUserID.String()] != 100 {
		t.Errorf("admin power level = %v, want 100", users[adminUserID.String()])
	}
	if levels["users_default"] != 0 {
		t.Errorf("users_default = %v, want 0", levels["users_default"])
	}

	events, ok := levels["events"].(map[ref.EventType]any)
	if !ok {
		t.Fatal("power levels missing 'events' map")
	}

	// Members must be able to write machine presence events.
	for _, eventType := range []ref.EventType{
		EventTypeMachineKey,
		EventTypeMachineInfo,
		EventTypeMachineStatus,
		EventTypeWebRTCOffer,
		EventTypeWebRTCAnswer,
	} {
		if events[eventType] != 0 {
			t.Errorf("%s power level = %v, want 0", eventType, events[eventType])
		}
	}

	// Dev team metadata is admin-only — explicit regardless of state_default.
	if events[EventTypeDevTeam] != 100 {
		t.Errorf("%s power level = %v, want 100", EventTypeDevTeam, events[EventTypeDevTeam])
	}

	// Admin-protected events must require PL 100.
	for _, eventType := range []ref.EventType{
		ref.EventType("m.room.encryption"), ref.EventType("m.room.server_acl"),
		ref.EventType("m.room.tombstone"), ref.EventType("m.space.child"),
	} {
		if events[eventType] != 100 {
			t.Errorf("%s power level = %v, want 100", eventType, events[eventType])
		}
	}

	if levels["state_default"] != 100 {
		t.Errorf("state_default = %v, want 100", levels["state_default"])
	}
	if levels["events_default"] != 0 {
		t.Errorf("events_default = %v, want 0", levels["events_default"])
	}
}

func TestServiceRoomPowerLevels(t *testing.T) {
	adminUserID := mustUserID(t, "@bureau-admin:bureau.local")
	levels := ServiceRoomPowerLevels(adminUserID)

	users, ok := levels["users"].(map[string]any)
	if !ok {
		t.Fatal("power levels missing 'users' map")
	}
	if users[adminUserID.String()] != 100 {
		t.Errorf("admin power level = %v, want 100", users[adminUserID.String()])
	}
	if levels["users_default"] != 0 {
		t.Errorf("users_default = %v, want 0", levels["users_default"])
	}

	events, ok := levels["events"].(map[ref.EventType]any)
	if !ok {
		t.Fatal("power levels missing 'events' map")
	}

	// Members must be able to register services.
	if events[EventTypeService] != 0 {
		t.Errorf("%s power level = %v, want 0", EventTypeService, events[EventTypeService])
	}

	// Dev team metadata is admin-only — explicit regardless of state_default.
	if events[EventTypeDevTeam] != 100 {
		t.Errorf("%s power level = %v, want 100", EventTypeDevTeam, events[EventTypeDevTeam])
	}

	// Admin-protected events must require PL 100.
	for _, eventType := range []ref.EventType{
		ref.EventType("m.room.encryption"), ref.EventType("m.room.server_acl"),
		ref.EventType("m.room.tombstone"), ref.EventType("m.space.child"),
	} {
		if events[eventType] != 100 {
			t.Errorf("%s power level = %v, want 100", eventType, events[eventType])
		}
	}

	if levels["state_default"] != 100 {
		t.Errorf("state_default = %v, want 100", levels["state_default"])
	}
	if levels["events_default"] != 0 {
		t.Errorf("events_default = %v, want 0", levels["events_default"])
	}

	// Invite threshold is 50 so machine daemons (PL 50) can invite
	// service principals for HA failover and fleet placement.
	if levels["invite"] != 50 {
		t.Errorf("invite = %v, want 50", levels["invite"])
	}
}

func TestFleetRoomPowerLevels(t *testing.T) {
	adminUserID := mustUserID(t, "@bureau-admin:bureau.local")
	levels := FleetRoomPowerLevels(adminUserID)

	users, ok := levels["users"].(map[string]any)
	if !ok {
		t.Fatal("power levels missing 'users' map")
	}
	if users[adminUserID.String()] != 100 {
		t.Errorf("admin power level = %v, want 100", users[adminUserID.String()])
	}
	if levels["users_default"] != 0 {
		t.Errorf("users_default = %v, want 0", levels["users_default"])
	}

	events, ok := levels["events"].(map[ref.EventType]any)
	if !ok {
		t.Fatal("power levels missing 'events' map")
	}

	// Members must be able to write runtime operational events.
	for _, eventType := range []ref.EventType{
		EventTypeHALease,
		EventTypeServiceStatus,
		EventTypeFleetAlert,
	} {
		if events[eventType] != 0 {
			t.Errorf("%s power level = %v, want 0", eventType, events[eventType])
		}
	}

	// Administrative configuration events are admin-only (PL 100).
	// These are written by operators through the CLI, not by runtime
	// components. A compromised member could inject phantom services
	// (FleetService), manipulate controller behavior (FleetConfig),
	// or compromise binary provenance (FleetCache).
	for _, eventType := range []ref.EventType{
		EventTypeFleetService,
		EventTypeMachineDefinition,
		EventTypeFleetConfig,
		EventTypeFleetCache,
		EventTypeEnvironmentBuild,
		EventTypeProvenanceRoots,
		EventTypeProvenancePolicy,
	} {
		if events[eventType] != 100 {
			t.Errorf("%s power level = %v, want 100 (admin-only)", eventType, events[eventType])
		}
	}

	// Dev team metadata is admin-only — explicit regardless of state_default.
	if events[EventTypeDevTeam] != 100 {
		t.Errorf("%s power level = %v, want 100", EventTypeDevTeam, events[EventTypeDevTeam])
	}

	if levels["state_default"] != 100 {
		t.Errorf("state_default = %v, want 100", levels["state_default"])
	}
	if levels["events_default"] != 0 {
		t.Errorf("events_default = %v, want 0", levels["events_default"])
	}

	// Fleet rooms are admin-invite-only.
	if levels["invite"] != 100 {
		t.Errorf("invite = %v, want 100", levels["invite"])
	}
}

func TestFleetCacheContentComposeFields(t *testing.T) {
	original := FleetCacheContent{
		URL:             "https://cache.infra.bureau.foundation",
		Name:            "main",
		PublicKeys:      []string{"cache-1:key1"},
		ComposeTemplate: "bureau/template:nix-builder",
		DefaultSystem:   "x86_64-linux",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded FleetCacheContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.ComposeTemplate != original.ComposeTemplate {
		t.Errorf("ComposeTemplate = %q, want %q", decoded.ComposeTemplate, original.ComposeTemplate)
	}
	if decoded.DefaultSystem != original.DefaultSystem {
		t.Errorf("DefaultSystem = %q, want %q", decoded.DefaultSystem, original.DefaultSystem)
	}

	// Omitempty: absent compose fields should not appear in JSON.
	minimal := FleetCacheContent{
		URL:        "https://example.com",
		PublicKeys: []string{"key"},
	}
	minData, _ := json.Marshal(minimal)
	var raw map[string]any
	json.Unmarshal(minData, &raw)
	if _, exists := raw["compose_template"]; exists {
		t.Error("compose_template should be omitted when empty")
	}
	if _, exists := raw["default_system"]; exists {
		t.Error("default_system should be omitted when empty")
	}
}

func TestProvenanceRootsContentRoundTrip(t *testing.T) {
	original := ProvenanceRootsContent{
		Roots: map[string]ProvenanceTrustRoot{
			"sigstore_public": {
				TUFRootVersion:    8,
				FulcioRootPEM:     "-----BEGIN CERTIFICATE-----\nMIIB+DCCAZygAwIBAgITNVk...\n-----END CERTIFICATE-----\n",
				RekorPublicKeyPEM: "-----BEGIN PUBLIC KEY-----\nMFkwEwYHKoZIzj0CAQYI...\n-----END PUBLIC KEY-----\n",
			},
			"fleet_private": {
				FulcioRootPEM:     "-----BEGIN CERTIFICATE-----\nMIIC3DCCAkWgAwIBAgIT...\n-----END CERTIFICATE-----\n",
				RekorPublicKeyPEM: "-----BEGIN PUBLIC KEY-----\nMHYwEAYHKoZIzj0CAQYF...\n-----END PUBLIC KEY-----\n",
			},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded ProvenanceRootsContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if length := len(decoded.Roots); length != 2 {
		t.Fatalf("Roots count = %d, want 2", length)
	}

	// Verify sigstore_public root (has TUF version).
	pub, ok := decoded.Roots["sigstore_public"]
	if !ok {
		t.Fatal("missing 'sigstore_public' root")
	}
	if pub.TUFRootVersion != 8 {
		t.Errorf("TUFRootVersion = %d, want 8", pub.TUFRootVersion)
	}
	if pub.FulcioRootPEM != original.Roots["sigstore_public"].FulcioRootPEM {
		t.Error("FulcioRootPEM mismatch for sigstore_public")
	}
	if pub.RekorPublicKeyPEM != original.Roots["sigstore_public"].RekorPublicKeyPEM {
		t.Error("RekorPublicKeyPEM mismatch for sigstore_public")
	}

	// Verify fleet_private root (no TUF version).
	priv, ok := decoded.Roots["fleet_private"]
	if !ok {
		t.Fatal("missing 'fleet_private' root")
	}
	if priv.TUFRootVersion != 0 {
		t.Errorf("fleet_private TUFRootVersion = %d, want 0", priv.TUFRootVersion)
	}

	// Omitempty: TUFRootVersion should be absent when zero.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to raw: %v", err)
	}
	roots := raw["roots"].(map[string]any)
	privRaw := roots["fleet_private"].(map[string]any)
	if _, exists := privRaw["tuf_root_version"]; exists {
		t.Error("tuf_root_version should be omitted when zero")
	}
}

func TestProvenancePolicyContentRoundTrip(t *testing.T) {
	original := ProvenancePolicyContent{
		TrustedIdentities: []TrustedIdentity{
			{
				Name:            "bureau-ci",
				Roots:           "sigstore_public",
				Issuer:          "https://token.actions.githubusercontent.com",
				SubjectPattern:  "repo:bureau-foundation/bureau:ref:refs/heads/main",
				WorkflowPattern: ".github/workflows/ci.yaml",
			},
			{
				Name:           "partner-models",
				Roots:          "fleet_private",
				Issuer:         "https://accounts.google.com",
				SubjectPattern: "partner-ml-*@*.iam.gserviceaccount.com",
			},
		},
		Enforcement: map[string]EnforcementLevel{
			"nix_store_paths": EnforcementRequire,
			"artifacts":       EnforcementWarn,
			"models":          EnforcementLog,
			"forge_artifacts": EnforcementRequire,
			"templates":       EnforcementWarn,
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded ProvenancePolicyContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if length := len(decoded.TrustedIdentities); length != 2 {
		t.Fatalf("TrustedIdentities count = %d, want 2", length)
	}

	// Verify first identity (has workflow pattern).
	ci := decoded.TrustedIdentities[0]
	if ci.Name != "bureau-ci" {
		t.Errorf("Name = %q, want %q", ci.Name, "bureau-ci")
	}
	if ci.Roots != "sigstore_public" {
		t.Errorf("Roots = %q, want %q", ci.Roots, "sigstore_public")
	}
	if ci.Issuer != "https://token.actions.githubusercontent.com" {
		t.Errorf("Issuer = %q, want GitHub Actions", ci.Issuer)
	}
	if ci.SubjectPattern != "repo:bureau-foundation/bureau:ref:refs/heads/main" {
		t.Errorf("SubjectPattern = %q, want bureau repo main", ci.SubjectPattern)
	}
	if ci.WorkflowPattern != ".github/workflows/ci.yaml" {
		t.Errorf("WorkflowPattern = %q, want ci.yaml", ci.WorkflowPattern)
	}

	// Verify second identity (no workflow pattern).
	partner := decoded.TrustedIdentities[1]
	if partner.WorkflowPattern != "" {
		t.Errorf("partner WorkflowPattern = %q, want empty", partner.WorkflowPattern)
	}

	// Omitempty: absent workflow_pattern should not appear in JSON.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to raw: %v", err)
	}
	identities := raw["trusted_identities"].([]any)
	partnerRaw := identities[1].(map[string]any)
	if _, exists := partnerRaw["workflow_pattern"]; exists {
		t.Error("workflow_pattern should be omitted when empty")
	}

	// Verify enforcement levels round-trip.
	if length := len(decoded.Enforcement); length != 5 {
		t.Fatalf("Enforcement count = %d, want 5", length)
	}
	for category, want := range original.Enforcement {
		got := decoded.Enforcement[category]
		if got != want {
			t.Errorf("Enforcement[%q] = %q, want %q", category, got, want)
		}
	}
}

func TestEnforcementLevelIsKnown(t *testing.T) {
	known := []EnforcementLevel{
		EnforcementRequire,
		EnforcementWarn,
		EnforcementLog,
	}
	for _, level := range known {
		if !level.IsKnown() {
			t.Errorf("IsKnown(%q) = false, want true", level)
		}
	}

	unknown := []EnforcementLevel{
		"",
		"ignore",
		"block",
		"REQUIRE",
	}
	for _, level := range unknown {
		if level.IsKnown() {
			t.Errorf("IsKnown(%q) = true, want false", level)
		}
	}
}

func TestEnvironmentBuildContentRoundTrip(t *testing.T) {
	original := EnvironmentBuildContent{
		Profile:   "sysadmin-runner-env",
		FlakeRef:  "github:bureau-foundation/bureau/abc123",
		System:    "x86_64-linux",
		StorePath: "/nix/store/xyz-bureau-sysadmin-runner-env",
		Machine:   mustUserID(t, "@bureau/fleet/prod/machine/workstation:bureau.local"),
		Timestamp: "2026-03-04T10:30:00Z",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded EnvironmentBuildContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.Profile != original.Profile {
		t.Errorf("Profile = %q, want %q", decoded.Profile, original.Profile)
	}
	if decoded.FlakeRef != original.FlakeRef {
		t.Errorf("FlakeRef = %q, want %q", decoded.FlakeRef, original.FlakeRef)
	}
	if decoded.System != original.System {
		t.Errorf("System = %q, want %q", decoded.System, original.System)
	}
	if decoded.StorePath != original.StorePath {
		t.Errorf("StorePath = %q, want %q", decoded.StorePath, original.StorePath)
	}
	if decoded.Machine != original.Machine {
		t.Errorf("Machine = %q, want %q", decoded.Machine, original.Machine)
	}
	if decoded.Timestamp != original.Timestamp {
		t.Errorf("Timestamp = %q, want %q", decoded.Timestamp, original.Timestamp)
	}
}
