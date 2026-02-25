// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"encoding/json"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/observation"
)

func TestGrantJSONRoundTrip(t *testing.T) {
	original := Grant{
		Actions:   []string{observation.ActionAll, ActionInterrupt},
		Targets:   []string{"bureau/dev/**"},
		ExpiresAt: "2026-03-01T00:00:00Z",
		Ticket:    "tkt-a3f9",
		GrantedBy: ref.MustParseUserID("@bureau/dev/pm:bureau.local"),
		GrantedAt: "2026-02-12T10:00:00Z",
		Source:    SourceMachineDefault,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded Grant
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(decoded.Actions) != 2 || decoded.Actions[0] != observation.ActionAll || decoded.Actions[1] != ActionInterrupt {
		t.Errorf("Actions = %v, want [%s %s]", decoded.Actions, observation.ActionAll, ActionInterrupt)
	}
	if len(decoded.Targets) != 1 || decoded.Targets[0] != "bureau/dev/**" {
		t.Errorf("Targets = %v, want [bureau/dev/**]", decoded.Targets)
	}
	if decoded.ExpiresAt != "2026-03-01T00:00:00Z" {
		t.Errorf("ExpiresAt = %q, want 2026-03-01T00:00:00Z", decoded.ExpiresAt)
	}
	if decoded.Ticket != "tkt-a3f9" {
		t.Errorf("Ticket = %q, want tkt-a3f9", decoded.Ticket)
	}
	if decoded.GrantedBy != ref.MustParseUserID("@bureau/dev/pm:bureau.local") {
		t.Errorf("GrantedBy = %v, want @bureau/dev/pm:bureau.local", decoded.GrantedBy)
	}
	if decoded.Source != SourceMachineDefault {
		t.Errorf("Source = %q, want %q", decoded.Source, SourceMachineDefault)
	}
}

func TestGrantJSONOmitsEmptyFields(t *testing.T) {
	// A minimal grant with only required fields should omit optional fields.
	grant := Grant{
		Actions: []string{ActionMatrixJoin},
	}

	data, err := json.Marshal(grant)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	for _, field := range []string{"targets", "expires_at", "ticket", "granted_at", "source"} {
		if _, exists := raw[field]; exists {
			t.Errorf("field %q should be omitted from JSON when empty", field)
		}
	}
	// GrantedBy is ref.UserID which implements TextMarshaler — Go's
	// encoding/json emits "" for the zero value even with omitempty.
	// Verify it marshals to empty string rather than being absent.
	if got, ok := raw["granted_by"]; !ok {
		t.Error("field \"granted_by\" should be present as empty string for zero UserID")
	} else if got != "" {
		t.Errorf("field \"granted_by\" = %v, want empty string for zero UserID", got)
	}

	if _, exists := raw["actions"]; !exists {
		t.Error("field \"actions\" should be present")
	}
}

func TestDenialJSONRoundTrip(t *testing.T) {
	original := Denial{
		Actions: []string{"ticket/close", "ticket/reopen"},
		Targets: []string{"bureau/dev/workspace/**"},
		Source:  SourcePrincipal,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded Denial
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(decoded.Actions) != 2 {
		t.Errorf("Actions length = %d, want 2", len(decoded.Actions))
	}
	if len(decoded.Targets) != 1 || decoded.Targets[0] != "bureau/dev/workspace/**" {
		t.Errorf("Targets = %v, want [bureau/dev/workspace/**]", decoded.Targets)
	}
	if decoded.Source != SourcePrincipal {
		t.Errorf("Source = %q, want %q", decoded.Source, SourcePrincipal)
	}
}

func TestAllowanceJSONRoundTrip(t *testing.T) {
	original := Allowance{
		Actions: []string{observation.ActionObserve, observation.ActionReadWrite},
		Actors:  []string{"bureau/dev/pm", "bureau-admin"},
		Source:  SourceTemporal,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded Allowance
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(decoded.Actions) != 2 {
		t.Errorf("Actions length = %d, want 2", len(decoded.Actions))
	}
	if len(decoded.Actors) != 2 || decoded.Actors[0] != "bureau/dev/pm" {
		t.Errorf("Actors = %v, want [bureau/dev/pm bureau-admin]", decoded.Actors)
	}
	if decoded.Source != SourceTemporal {
		t.Errorf("Source = %q, want %q", decoded.Source, SourceTemporal)
	}
}

func TestSourceRoomHelper(t *testing.T) {
	source := SourceRoom("!abc123:bureau.local")
	want := "room:!abc123:bureau.local"
	if source != want {
		t.Errorf("SourceRoom() = %q, want %q", source, want)
	}
}

func TestAuthorizationPolicyJSONRoundTrip(t *testing.T) {
	original := AuthorizationPolicy{
		Grants: []Grant{
			{Actions: []string{observation.ActionAll}, Targets: []string{"bureau/dev/**"}},
			{Actions: []string{ActionMatrixJoin, ActionMatrixInvite}},
		},
		Denials: []Denial{
			{Actions: []string{"fleet/**"}},
		},
		Allowances: []Allowance{
			{Actions: []string{observation.ActionObserve}, Actors: []string{"iree/**"}},
		},
		AllowanceDenials: []AllowanceDenial{
			{Actions: []string{observation.ActionObserve}, Actors: []string{"iree/secret/**"}},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded AuthorizationPolicy
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(decoded.Grants) != 2 {
		t.Errorf("Grants length = %d, want 2", len(decoded.Grants))
	}
	if len(decoded.Denials) != 1 {
		t.Errorf("Denials length = %d, want 1", len(decoded.Denials))
	}
	if len(decoded.Allowances) != 1 {
		t.Errorf("Allowances length = %d, want 1", len(decoded.Allowances))
	}
	if len(decoded.AllowanceDenials) != 1 {
		t.Errorf("AllowanceDenials length = %d, want 1", len(decoded.AllowanceDenials))
	}
}

func TestAuthorizationPolicyJSONOmitsEmpty(t *testing.T) {
	policy := AuthorizationPolicy{}

	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	for _, field := range []string{"grants", "denials", "allowances", "allowance_denials"} {
		if _, exists := raw[field]; exists {
			t.Errorf("field %q should be omitted from JSON when empty", field)
		}
	}
}

func TestRoomAuthorizationPolicyJSONRoundTrip(t *testing.T) {
	original := RoomAuthorizationPolicy{
		MemberGrants: []Grant{
			{Actions: []string{"ticket/create", "ticket/assign"}},
		},
		PowerLevelGrants: map[string][]Grant{
			"50": {
				{Actions: []string{ActionInterrupt, observation.ActionReadWrite}, Targets: []string{"bureau/dev/workspace/**"}},
			},
			"100": {
				{Actions: []string{"**"}, Targets: []string{"bureau/dev/workspace/**"}},
			},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded RoomAuthorizationPolicy
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(decoded.MemberGrants) != 1 {
		t.Errorf("MemberGrants length = %d, want 1", len(decoded.MemberGrants))
	}
	if len(decoded.PowerLevelGrants) != 2 {
		t.Errorf("PowerLevelGrants length = %d, want 2", len(decoded.PowerLevelGrants))
	}
	if grants, ok := decoded.PowerLevelGrants["50"]; !ok || len(grants) != 1 {
		t.Errorf("PowerLevelGrants[50] = %v, want 1 grant", grants)
	}
}

func testUserID(t *testing.T, raw string) ref.UserID {
	t.Helper()
	userID, err := ref.ParseUserID(raw)
	if err != nil {
		t.Fatalf("ParseUserID(%q): %v", raw, err)
	}
	return userID
}

func TestTemporalGrantContentJSONRoundTrip(t *testing.T) {
	principalID := testUserID(t, "@bureau/fleet/test/agent/coder:bureau.local")
	original := TemporalGrantContent{
		Grant: Grant{
			Actions:   []string{observation.ActionObserve},
			Targets:   []string{"service/db/postgres:bureau.local"},
			ExpiresAt: "2026-02-11T18:30:00Z",
			Ticket:    "tkt-c4d1",
			GrantedBy: ref.MustParseUserID("@bureau/dev/workspace/tpm:bureau.local"),
			GrantedAt: "2026-02-11T16:30:00Z",
		},
		Principal: principalID,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded TemporalGrantContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.Principal != principalID {
		t.Errorf("Principal = %v, want %v", decoded.Principal, principalID)
	}
	if decoded.Grant.Ticket != "tkt-c4d1" {
		t.Errorf("Grant.Ticket = %q, want tkt-c4d1", decoded.Grant.Ticket)
	}
	if decoded.Grant.ExpiresAt != "2026-02-11T18:30:00Z" {
		t.Errorf("Grant.ExpiresAt = %q, want 2026-02-11T18:30:00Z", decoded.Grant.ExpiresAt)
	}
}

func TestTokenSigningKeyContentJSON(t *testing.T) {
	machineID := testUserID(t, "@bureau/fleet/prod/machine/workstation:bureau.local")
	original := TokenSigningKeyContent{
		PublicKey: "d75a980182b10ab7d54bfed3c964073a0ee172f3daa3f4a18446b7e8c3f1b2e0",
		Machine:   machineID,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded TokenSigningKeyContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.PublicKey != original.PublicKey {
		t.Errorf("PublicKey = %q, want %q", decoded.PublicKey, original.PublicKey)
	}
	if decoded.Machine != machineID {
		t.Errorf("Machine = %v, want %v", decoded.Machine, machineID)
	}
}

func TestPrincipalAssignmentAuthorizationField(t *testing.T) {
	// Verify that the Authorization field on PrincipalAssignment
	// round-trips correctly through JSON.
	assignment := PrincipalAssignment{
		Principal: testEntity(t, "@bureau/fleet/test/agent/pm:bureau.local"),
		Template:  "bureau/template:base",
		Authorization: &AuthorizationPolicy{
			Grants: []Grant{
				{Actions: []string{observation.ActionAll}, Targets: []string{"iree/**"}},
			},
			Allowances: []Allowance{
				{Actions: []string{observation.ActionObserve}, Actors: []string{"bureau-admin"}},
			},
		},
	}

	data, err := json.Marshal(assignment)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded PrincipalAssignment
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.Authorization == nil {
		t.Fatal("Authorization is nil after round-trip")
	}
	if len(decoded.Authorization.Grants) != 1 {
		t.Errorf("Authorization.Grants length = %d, want 1", len(decoded.Authorization.Grants))
	}
	if len(decoded.Authorization.Allowances) != 1 {
		t.Errorf("Authorization.Allowances length = %d, want 1", len(decoded.Authorization.Allowances))
	}
}

func TestMachineConfigDefaultPolicyField(t *testing.T) {
	config := MachineConfig{
		Principals: []PrincipalAssignment{
			{Principal: testEntity(t, "@bureau/fleet/test/agent/test:bureau.local"), Template: "bureau/template:base"},
		},
		DefaultPolicy: &AuthorizationPolicy{
			Grants: []Grant{
				{Actions: []string{ActionServiceDiscover}, Targets: []string{"service/**"}},
			},
			Allowances: []Allowance{
				{Actions: []string{observation.ActionObserve}, Actors: []string{"bureau-admin"}},
			},
		},
	}

	data, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded MachineConfig
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.DefaultPolicy == nil {
		t.Fatal("DefaultPolicy is nil after round-trip")
	}
	if len(decoded.DefaultPolicy.Grants) != 1 {
		t.Errorf("DefaultPolicy.Grants length = %d, want 1", len(decoded.DefaultPolicy.Grants))
	}
}
