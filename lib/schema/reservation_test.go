// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"encoding/json"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
)

func TestResourceTypeIsKnown(t *testing.T) {
	for _, testCase := range []struct {
		value ResourceType
		known bool
	}{
		{ResourceMachine, true},
		{ResourceQuota, true},
		{ResourceHuman, true},
		{ResourceService, true},
		{"unknown", false},
		{"", false},
	} {
		if got := testCase.value.IsKnown(); got != testCase.known {
			t.Errorf("ResourceType(%q).IsKnown() = %v, want %v", testCase.value, got, testCase.known)
		}
	}
}

func TestReservationModeIsKnown(t *testing.T) {
	for _, testCase := range []struct {
		value ReservationMode
		known bool
	}{
		{ModeExclusive, true},
		{ModeInclusive, true},
		{ModePartial, true},
		{"unknown", false},
		{"", false},
	} {
		if got := testCase.value.IsKnown(); got != testCase.known {
			t.Errorf("ReservationMode(%q).IsKnown() = %v, want %v", testCase.value, got, testCase.known)
		}
	}
}

func TestClaimStatusIsKnown(t *testing.T) {
	for _, testCase := range []struct {
		value    ClaimStatus
		known    bool
		terminal bool
	}{
		{ClaimPending, true, false},
		{ClaimQueued, true, false},
		{ClaimApproved, true, false},
		{ClaimGranted, true, false},
		{ClaimDenied, true, true},
		{ClaimReleased, true, true},
		{ClaimPreempted, true, true},
		{"unknown", false, false},
		{"", false, false},
	} {
		if got := testCase.value.IsKnown(); got != testCase.known {
			t.Errorf("ClaimStatus(%q).IsKnown() = %v, want %v", testCase.value, got, testCase.known)
		}
		if got := testCase.value.IsTerminal(); got != testCase.terminal {
			t.Errorf("ClaimStatus(%q).IsTerminal() = %v, want %v", testCase.value, got, testCase.terminal)
		}
	}
}

func TestResourceRefValidate(t *testing.T) {
	tests := []struct {
		name    string
		ref     ResourceRef
		wantErr bool
	}{
		{
			name:    "valid machine",
			ref:     ResourceRef{Type: ResourceMachine, Target: "gpu-box"},
			wantErr: false,
		},
		{
			name:    "valid quota",
			ref:     ResourceRef{Type: ResourceQuota, Target: "claude-api"},
			wantErr: false,
		},
		{
			name:    "missing type",
			ref:     ResourceRef{Target: "gpu-box"},
			wantErr: true,
		},
		{
			name:    "unknown type",
			ref:     ResourceRef{Type: "warp_drive", Target: "enterprise"},
			wantErr: true,
		},
		{
			name:    "missing target",
			ref:     ResourceRef{Type: ResourceMachine},
			wantErr: true,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.ref.Validate()
			if (err != nil) != testCase.wantErr {
				t.Errorf("Validate() error = %v, wantErr = %v", err, testCase.wantErr)
			}
		})
	}
}

func validReservationGrant(t *testing.T) ReservationGrant {
	t.Helper()
	return ReservationGrant{
		Holder:      testEntity(t, "@bureau/fleet/prod/agent/benchmark:bureau.local"),
		Resource:    ResourceRef{Type: ResourceMachine, Target: "gpu-box"},
		Mode:        ModeExclusive,
		GrantedAt:   "2026-03-04T03:00:00Z",
		ExpiresAt:   "2026-03-04T05:00:00Z",
		RelayTicket: "tkt-xyz",
	}
}

func TestReservationGrantRoundTrip(t *testing.T) {
	original := validReservationGrant(t)

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded ReservationGrant
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.Holder != original.Holder {
		t.Errorf("Holder = %v, want %v", decoded.Holder, original.Holder)
	}
	if decoded.Resource.Type != original.Resource.Type {
		t.Errorf("Resource.Type = %v, want %v", decoded.Resource.Type, original.Resource.Type)
	}
	if decoded.Resource.Target != original.Resource.Target {
		t.Errorf("Resource.Target = %v, want %v", decoded.Resource.Target, original.Resource.Target)
	}
	if decoded.Mode != original.Mode {
		t.Errorf("Mode = %v, want %v", decoded.Mode, original.Mode)
	}
	if decoded.GrantedAt != original.GrantedAt {
		t.Errorf("GrantedAt = %v, want %v", decoded.GrantedAt, original.GrantedAt)
	}
	if decoded.ExpiresAt != original.ExpiresAt {
		t.Errorf("ExpiresAt = %v, want %v", decoded.ExpiresAt, original.ExpiresAt)
	}
	if decoded.RelayTicket != original.RelayTicket {
		t.Errorf("RelayTicket = %v, want %v", decoded.RelayTicket, original.RelayTicket)
	}
}

func TestReservationGrantValidate(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*ReservationGrant)
		wantErr bool
	}{
		{
			name:    "valid",
			modify:  func(_ *ReservationGrant) {},
			wantErr: false,
		},
		{
			name:    "missing holder",
			modify:  func(g *ReservationGrant) { g.Holder = ref.Entity{} },
			wantErr: true,
		},
		{
			name:    "missing resource type",
			modify:  func(g *ReservationGrant) { g.Resource.Type = "" },
			wantErr: true,
		},
		{
			name:    "missing mode",
			modify:  func(g *ReservationGrant) { g.Mode = "" },
			wantErr: true,
		},
		{
			name:    "unknown mode",
			modify:  func(g *ReservationGrant) { g.Mode = "quantum" },
			wantErr: true,
		},
		{
			name:    "missing granted_at",
			modify:  func(g *ReservationGrant) { g.GrantedAt = "" },
			wantErr: true,
		},
		{
			name:    "invalid granted_at",
			modify:  func(g *ReservationGrant) { g.GrantedAt = "not-a-date" },
			wantErr: true,
		},
		{
			name:    "missing expires_at",
			modify:  func(g *ReservationGrant) { g.ExpiresAt = "" },
			wantErr: true,
		},
		{
			name:    "invalid expires_at",
			modify:  func(g *ReservationGrant) { g.ExpiresAt = "tomorrow" },
			wantErr: true,
		},
		{
			name:    "missing relay_ticket",
			modify:  func(g *ReservationGrant) { g.RelayTicket = "" },
			wantErr: true,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			grant := validReservationGrant(t)
			testCase.modify(&grant)
			err := grant.Validate()
			if (err != nil) != testCase.wantErr {
				t.Errorf("Validate() error = %v, wantErr = %v", err, testCase.wantErr)
			}
		})
	}
}

func TestRelayLinkRoundTrip(t *testing.T) {
	original := RelayLink{
		OriginRoom:   ref.MustParseRoomID("!abc123:bureau.local"),
		OriginTicket: "tkt-a3f9",
		Requester:    testEntity(t, "@bureau/fleet/prod/agent/benchmark:bureau.local"),
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded RelayLink
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.OriginRoom != original.OriginRoom {
		t.Errorf("OriginRoom = %v, want %v", decoded.OriginRoom, original.OriginRoom)
	}
	if decoded.OriginTicket != original.OriginTicket {
		t.Errorf("OriginTicket = %v, want %v", decoded.OriginTicket, original.OriginTicket)
	}
	if decoded.Requester != original.Requester {
		t.Errorf("Requester = %v, want %v", decoded.Requester, original.Requester)
	}
}

func TestRelayLinkValidate(t *testing.T) {
	tests := []struct {
		name    string
		link    RelayLink
		wantErr bool
	}{
		{
			name: "valid",
			link: RelayLink{
				OriginRoom:   ref.MustParseRoomID("!abc123:bureau.local"),
				OriginTicket: "tkt-a3f9",
				Requester:    testEntity(t, "@bureau/fleet/prod/agent/benchmark:bureau.local"),
			},
			wantErr: false,
		},
		{
			name: "missing origin_room",
			link: RelayLink{
				OriginTicket: "tkt-a3f9",
				Requester:    testEntity(t, "@bureau/fleet/prod/agent/benchmark:bureau.local"),
			},
			wantErr: true,
		},
		{
			name: "missing origin_ticket",
			link: RelayLink{
				OriginRoom: ref.MustParseRoomID("!abc123:bureau.local"),
				Requester:  testEntity(t, "@bureau/fleet/prod/agent/benchmark:bureau.local"),
			},
			wantErr: true,
		},
		{
			name: "missing requester",
			link: RelayLink{
				OriginRoom:   ref.MustParseRoomID("!abc123:bureau.local"),
				OriginTicket: "tkt-a3f9",
			},
			wantErr: true,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.link.Validate()
			if (err != nil) != testCase.wantErr {
				t.Errorf("Validate() error = %v, wantErr = %v", err, testCase.wantErr)
			}
		})
	}
}

func TestRelayPolicyValidate(t *testing.T) {
	tests := []struct {
		name    string
		policy  RelayPolicy
		wantErr bool
	}{
		{
			name: "valid fleet_member source",
			policy: RelayPolicy{
				Sources: []RelaySource{
					{Match: RelayMatchFleetMember, Fleet: "prod"},
				},
			},
			wantErr: false,
		},
		{
			name: "valid room source",
			policy: RelayPolicy{
				Sources: []RelaySource{
					{Match: RelayMatchRoom, Room: ref.MustParseRoomID("!abc123:bureau.local")},
				},
			},
			wantErr: false,
		},
		{
			name: "valid namespace source",
			policy: RelayPolicy{
				Sources: []RelaySource{
					{Match: RelayMatchNamespace, Namespace: "iree"},
				},
			},
			wantErr: false,
		},
		{
			name: "valid with allowed types and filter",
			policy: RelayPolicy{
				Sources: []RelaySource{
					{Match: RelayMatchFleetMember, Fleet: "prod"},
				},
				AllowedTypes:   []string{"resource_request"},
				OutboundFilter: &RelayFilter{Include: []string{"status", "close_reason"}},
			},
			wantErr: false,
		},
		{
			name:    "empty sources",
			policy:  RelayPolicy{},
			wantErr: true,
		},
		{
			name: "missing match",
			policy: RelayPolicy{
				Sources: []RelaySource{
					{Fleet: "prod"},
				},
			},
			wantErr: true,
		},
		{
			name: "fleet_member without fleet",
			policy: RelayPolicy{
				Sources: []RelaySource{
					{Match: RelayMatchFleetMember},
				},
			},
			wantErr: true,
		},
		{
			name: "room without room",
			policy: RelayPolicy{
				Sources: []RelaySource{
					{Match: RelayMatchRoom},
				},
			},
			wantErr: true,
		},
		{
			name: "namespace without namespace",
			policy: RelayPolicy{
				Sources: []RelaySource{
					{Match: RelayMatchNamespace},
				},
			},
			wantErr: true,
		},
		{
			name: "unknown match",
			policy: RelayPolicy{
				Sources: []RelaySource{
					{Match: "magic"},
				},
			},
			wantErr: true,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.policy.Validate()
			if (err != nil) != testCase.wantErr {
				t.Errorf("Validate() error = %v, wantErr = %v", err, testCase.wantErr)
			}
		})
	}
}

func TestRelayPolicyRoundTrip(t *testing.T) {
	original := RelayPolicy{
		Sources: []RelaySource{
			{Match: RelayMatchFleetMember, Fleet: "prod"},
			{Match: RelayMatchRoom, Room: ref.MustParseRoomID("!abc123:bureau.local")},
		},
		AllowedTypes: []string{"resource_request"},
		OutboundFilter: &RelayFilter{
			Include: []string{"status", "close_reason"},
			Exclude: []string{"body"},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded RelayPolicy
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(decoded.Sources) != 2 {
		t.Fatalf("Sources length = %d, want 2", len(decoded.Sources))
	}
	if decoded.Sources[0].Match != RelayMatchFleetMember {
		t.Errorf("Sources[0].Match = %v, want %v", decoded.Sources[0].Match, RelayMatchFleetMember)
	}
	if decoded.Sources[0].Fleet != "prod" {
		t.Errorf("Sources[0].Fleet = %v, want %v", decoded.Sources[0].Fleet, "prod")
	}
	if len(decoded.AllowedTypes) != 1 || decoded.AllowedTypes[0] != "resource_request" {
		t.Errorf("AllowedTypes = %v, want [resource_request]", decoded.AllowedTypes)
	}
	if decoded.OutboundFilter == nil {
		t.Fatal("OutboundFilter is nil")
	}
	if len(decoded.OutboundFilter.Include) != 2 {
		t.Errorf("OutboundFilter.Include length = %d, want 2", len(decoded.OutboundFilter.Include))
	}
}

func TestRelayFilterAllows(t *testing.T) {
	tests := []struct {
		name   string
		filter *RelayFilter
		field  string
		want   bool
	}{
		// Nil filter: only status and close_reason allowed (default policy).
		{name: "nil allows status", filter: nil, field: "status", want: true},
		{name: "nil allows close_reason", filter: nil, field: "close_reason", want: true},
		{name: "nil allows status_reason", filter: nil, field: "status_reason", want: true},
		{name: "nil denies body", filter: nil, field: "body", want: false},

		// Non-nil, empty Include: allow all (minus Exclude).
		{
			name:   "empty include allows everything",
			filter: &RelayFilter{},
			field:  "status_reason",
			want:   true,
		},
		{
			name:   "empty include with exclude blocks excluded",
			filter: &RelayFilter{Exclude: []string{"body", "status_reason"}},
			field:  "status_reason",
			want:   false,
		},
		{
			name:   "empty include with exclude allows non-excluded",
			filter: &RelayFilter{Exclude: []string{"body"}},
			field:  "status",
			want:   true,
		},

		// Non-nil, populated Include: only listed fields allowed.
		{
			name:   "include list allows listed field",
			filter: &RelayFilter{Include: []string{"status", "close_reason", "status_reason"}},
			field:  "status_reason",
			want:   true,
		},
		{
			name:   "include list denies unlisted field",
			filter: &RelayFilter{Include: []string{"status", "close_reason"}},
			field:  "status_reason",
			want:   false,
		},

		// Exclude takes precedence over Include.
		{
			name:   "exclude overrides include",
			filter: &RelayFilter{Include: []string{"status", "close_reason"}, Exclude: []string{"close_reason"}},
			field:  "close_reason",
			want:   false,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if got := testCase.filter.Allows(testCase.field); got != testCase.want {
				t.Errorf("Allows(%q) = %v, want %v", testCase.field, got, testCase.want)
			}
		})
	}
}

func TestMachineDrainContentValidate(t *testing.T) {
	tests := []struct {
		name    string
		drain   MachineDrainContent
		wantErr bool
	}{
		{
			name: "valid with services",
			drain: MachineDrainContent{
				Services:          []string{"buildbarn-worker", "whisper-stt"},
				ReservationHolder: testEntity(t, "@bureau/fleet/prod/agent/benchmark:bureau.local"),
				RequestedAt:       "2026-03-04T03:00:00Z",
			},
			wantErr: false,
		},
		{
			name: "valid without services (drain all)",
			drain: MachineDrainContent{
				ReservationHolder: testEntity(t, "@bureau/fleet/prod/agent/benchmark:bureau.local"),
				RequestedAt:       "2026-03-04T03:00:00Z",
			},
			wantErr: false,
		},
		{
			name: "missing reservation_holder",
			drain: MachineDrainContent{
				RequestedAt: "2026-03-04T03:00:00Z",
			},
			wantErr: true,
		},
		{
			name: "missing requested_at",
			drain: MachineDrainContent{
				ReservationHolder: testEntity(t, "@bureau/fleet/prod/agent/benchmark:bureau.local"),
			},
			wantErr: true,
		},
		{
			name: "invalid requested_at",
			drain: MachineDrainContent{
				ReservationHolder: testEntity(t, "@bureau/fleet/prod/agent/benchmark:bureau.local"),
				RequestedAt:       "yesterday",
			},
			wantErr: true,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.drain.Validate()
			if (err != nil) != testCase.wantErr {
				t.Errorf("Validate() error = %v, wantErr = %v", err, testCase.wantErr)
			}
		})
	}
}

func TestMachineDrainContentRoundTrip(t *testing.T) {
	original := MachineDrainContent{
		Services:          []string{"buildbarn-worker"},
		ReservationHolder: testEntity(t, "@bureau/fleet/prod/agent/benchmark:bureau.local"),
		RequestedAt:       "2026-03-04T03:00:00Z",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded MachineDrainContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(decoded.Services) != 1 || decoded.Services[0] != "buildbarn-worker" {
		t.Errorf("Services = %v, want [buildbarn-worker]", decoded.Services)
	}
	if decoded.ReservationHolder != original.ReservationHolder {
		t.Errorf("ReservationHolder = %v, want %v", decoded.ReservationHolder, original.ReservationHolder)
	}
	if decoded.RequestedAt != original.RequestedAt {
		t.Errorf("RequestedAt = %v, want %v", decoded.RequestedAt, original.RequestedAt)
	}
}

func TestOpsRoomPowerLevels(t *testing.T) {
	adminUserID := mustUserID(t, "@bureau-admin:bureau.local")
	levels := OpsRoomPowerLevels(adminUserID)

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

	// Ticket service events require PL 25.
	for _, eventType := range []ref.EventType{
		EventTypeTicket,
		EventTypeTicketConfig,
		EventTypeRelayLink,
	} {
		if events[eventType] != 25 {
			t.Errorf("%s power level = %v, want 25", eventType, events[eventType])
		}
	}

	// Resource owner events require PL 50.
	for _, eventType := range []ref.EventType{
		EventTypeReservation,
		EventTypeMachineDrain,
	} {
		if events[eventType] != 50 {
			t.Errorf("%s power level = %v, want 50", eventType, events[eventType])
		}
	}

	// Timeline messages at PL 0 for operational audit trail.
	for _, eventType := range []ref.EventType{
		MatrixEventTypeMessage,
		EventTypeServiceReady,
	} {
		if events[eventType] != 0 {
			t.Errorf("%s power level = %v, want 0", eventType, events[eventType])
		}
	}

	// Relay policy is admin-only (PL 100).
	if events[EventTypeRelayPolicy] != 100 {
		t.Errorf("%s power level = %v, want 100", EventTypeRelayPolicy, events[EventTypeRelayPolicy])
	}

	// events_default denies by default.
	if levels["events_default"] != 100 {
		t.Errorf("events_default = %v, want 100", levels["events_default"])
	}

	// state_default denies by default.
	if levels["state_default"] != 100 {
		t.Errorf("state_default = %v, want 100", levels["state_default"])
	}

	// Ops rooms are admin-invite-only.
	if levels["invite"] != 100 {
		t.Errorf("invite = %v, want 100", levels["invite"])
	}
}
