// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"encoding/json"
	"testing"
)

func TestGrantJSONRoundTrip(t *testing.T) {
	original := Grant{
		Actions:   []string{"observe/**", "interrupt"},
		Targets:   []string{"bureau/dev/**"},
		ExpiresAt: "2026-03-01T00:00:00Z",
		Ticket:    "tkt-a3f9",
		GrantedBy: "@bureau/dev/pm:bureau.local",
		GrantedAt: "2026-02-12T10:00:00Z",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded Grant
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(decoded.Actions) != 2 || decoded.Actions[0] != "observe/**" || decoded.Actions[1] != "interrupt" {
		t.Errorf("Actions = %v, want [observe/** interrupt]", decoded.Actions)
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
	if decoded.GrantedBy != "@bureau/dev/pm:bureau.local" {
		t.Errorf("GrantedBy = %q, want @bureau/dev/pm:bureau.local", decoded.GrantedBy)
	}
}

func TestGrantJSONOmitsEmptyFields(t *testing.T) {
	// A minimal grant with only required fields should omit optional fields.
	grant := Grant{
		Actions: []string{"matrix/join"},
	}

	data, err := json.Marshal(grant)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	for _, field := range []string{"targets", "expires_at", "ticket", "granted_by", "granted_at"} {
		if _, exists := raw[field]; exists {
			t.Errorf("field %q should be omitted from JSON when empty", field)
		}
	}

	if _, exists := raw["actions"]; !exists {
		t.Error("field \"actions\" should be present")
	}
}

func TestDenialJSONRoundTrip(t *testing.T) {
	original := Denial{
		Actions: []string{"ticket/close", "ticket/reopen"},
		Targets: []string{"bureau/dev/workspace/**"},
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
}

func TestAllowanceJSONRoundTrip(t *testing.T) {
	original := Allowance{
		Actions: []string{"observe", "observe/read-write"},
		Actors:  []string{"bureau/dev/pm", "bureau-admin"},
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
}

func TestAuthorizationPolicyJSONRoundTrip(t *testing.T) {
	original := AuthorizationPolicy{
		Grants: []Grant{
			{Actions: []string{"observe/**"}, Targets: []string{"bureau/dev/**"}},
			{Actions: []string{"matrix/join", "matrix/invite"}},
		},
		Denials: []Denial{
			{Actions: []string{"fleet/**"}},
		},
		Allowances: []Allowance{
			{Actions: []string{"observe"}, Actors: []string{"iree/**"}},
		},
		AllowanceDenials: []AllowanceDenial{
			{Actions: []string{"observe"}, Actors: []string{"iree/secret/**"}},
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
				{Actions: []string{"interrupt", "observe/read-write"}, Targets: []string{"bureau/dev/workspace/**"}},
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

func TestTemporalGrantContentJSONRoundTrip(t *testing.T) {
	original := TemporalGrantContent{
		Grant: Grant{
			Actions:   []string{"observe"},
			Targets:   []string{"service/db/postgres"},
			ExpiresAt: "2026-02-11T18:30:00Z",
			Ticket:    "tkt-c4d1",
			GrantedBy: "@bureau/dev/workspace/tpm:bureau.local",
			GrantedAt: "2026-02-11T16:30:00Z",
		},
		Principal: "bureau/dev/workspace/coder/0",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded TemporalGrantContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.Principal != "bureau/dev/workspace/coder/0" {
		t.Errorf("Principal = %q, want bureau/dev/workspace/coder/0", decoded.Principal)
	}
	if decoded.Grant.Ticket != "tkt-c4d1" {
		t.Errorf("Grant.Ticket = %q, want tkt-c4d1", decoded.Grant.Ticket)
	}
	if decoded.Grant.ExpiresAt != "2026-02-11T18:30:00Z" {
		t.Errorf("Grant.ExpiresAt = %q, want 2026-02-11T18:30:00Z", decoded.Grant.ExpiresAt)
	}
}

func TestTokenSigningKeyContentJSON(t *testing.T) {
	original := TokenSigningKeyContent{
		PublicKey: "d75a980182b10ab7d54bfed3c964073a0ee172f3daa3f4a18446b7e8c3f1b2e0",
		Machine:   "machine/workstation",
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
	if decoded.Machine != "machine/workstation" {
		t.Errorf("Machine = %q, want machine/workstation", decoded.Machine)
	}
}

func TestPrincipalAssignmentAuthorizationField(t *testing.T) {
	// Verify that the Authorization field on PrincipalAssignment
	// round-trips correctly through JSON.
	assignment := PrincipalAssignment{
		Localpart: "iree/amdgpu/pm",
		Template:  "bureau/template:base",
		Authorization: &AuthorizationPolicy{
			Grants: []Grant{
				{Actions: []string{"observe/**"}, Targets: []string{"iree/**"}},
			},
			Allowances: []Allowance{
				{Actions: []string{"observe"}, Actors: []string{"bureau-admin"}},
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
			{Localpart: "test/agent", Template: "bureau/template:base"},
		},
		DefaultPolicy: &AuthorizationPolicy{
			Grants: []Grant{
				{Actions: []string{"service/discover"}, Targets: []string{"service/**"}},
			},
			Allowances: []Allowance{
				{Actions: []string{"observe"}, Actors: []string{"bureau-admin"}},
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
