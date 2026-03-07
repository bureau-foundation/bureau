// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import (
	"encoding/json"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
)

func validReservationContent() ReservationContent {
	return ReservationContent{
		Claims: []ResourceClaim{
			{
				Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"},
				Mode:     schema.ModeExclusive,
				Status:   schema.ClaimPending,
				Machine:  &MachineReservation{DrainServices: []string{"buildbarn-worker"}},
			},
		},
		MaxDuration: "2h",
	}
}

func TestReservationContentRoundTrip(t *testing.T) {
	original := validReservationContent()

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded ReservationContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(decoded.Claims) != 1 {
		t.Fatalf("Claims length = %d, want 1", len(decoded.Claims))
	}
	if decoded.Claims[0].Resource.Type != schema.ResourceMachine {
		t.Errorf("Claims[0].Resource.Type = %v, want %v", decoded.Claims[0].Resource.Type, schema.ResourceMachine)
	}
	if decoded.Claims[0].Resource.Target != "gpu-box" {
		t.Errorf("Claims[0].Resource.Target = %v, want %v", decoded.Claims[0].Resource.Target, "gpu-box")
	}
	if decoded.Claims[0].Mode != schema.ModeExclusive {
		t.Errorf("Claims[0].Mode = %v, want %v", decoded.Claims[0].Mode, schema.ModeExclusive)
	}
	if decoded.Claims[0].Machine == nil {
		t.Fatal("Claims[0].Machine is nil")
	}
	if len(decoded.Claims[0].Machine.DrainServices) != 1 {
		t.Errorf("Claims[0].Machine.DrainServices length = %d, want 1", len(decoded.Claims[0].Machine.DrainServices))
	}
	if decoded.MaxDuration != "2h" {
		t.Errorf("MaxDuration = %v, want %v", decoded.MaxDuration, "2h")
	}
}

func TestReservationContentValidate(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*ReservationContent)
		wantErr bool
	}{
		{
			name:    "valid single machine claim",
			modify:  func(_ *ReservationContent) {},
			wantErr: false,
		},
		{
			name: "valid multi-claim",
			modify: func(r *ReservationContent) {
				r.Claims = append(r.Claims, ResourceClaim{
					Resource: schema.ResourceRef{Type: schema.ResourceQuota, Target: "claude-api"},
					Mode:     schema.ModeInclusive,
					Status:   schema.ClaimPending,
					Quota:    &QuotaReservation{CostLimit: "50.00"},
				})
			},
			wantErr: false,
		},
		{
			name: "empty claims",
			modify: func(r *ReservationContent) {
				r.Claims = nil
			},
			wantErr: true,
		},
		{
			name: "missing max_duration",
			modify: func(r *ReservationContent) {
				r.MaxDuration = ""
			},
			wantErr: true,
		},
		{
			name: "invalid max_duration",
			modify: func(r *ReservationContent) {
				r.MaxDuration = "forever"
			},
			wantErr: true,
		},
		{
			name: "negative max_duration",
			modify: func(r *ReservationContent) {
				r.MaxDuration = "-1h"
			},
			wantErr: true,
		},
		{
			name: "zero max_duration",
			modify: func(r *ReservationContent) {
				r.MaxDuration = "0s"
			},
			wantErr: true,
		},
		{
			name: "invalid claim resource",
			modify: func(r *ReservationContent) {
				r.Claims[0].Resource.Type = ""
			},
			wantErr: true,
		},
		{
			name: "invalid claim mode",
			modify: func(r *ReservationContent) {
				r.Claims[0].Mode = "turbo"
			},
			wantErr: true,
		},
		{
			name: "invalid claim status",
			modify: func(r *ReservationContent) {
				r.Claims[0].Status = "limbo"
			},
			wantErr: true,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			reservation := validReservationContent()
			testCase.modify(&reservation)
			err := reservation.Validate()
			if (err != nil) != testCase.wantErr {
				t.Errorf("Validate() error = %v, wantErr = %v", err, testCase.wantErr)
			}
		})
	}
}

func TestResourceClaimTypeSpecificValidation(t *testing.T) {
	tests := []struct {
		name    string
		claim   ResourceClaim
		wantErr bool
	}{
		{
			name: "machine claim with machine params",
			claim: ResourceClaim{
				Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"},
				Mode:     schema.ModeExclusive,
				Status:   schema.ClaimPending,
				Machine:  &MachineReservation{DrainServices: []string{"buildbarn-worker"}},
			},
			wantErr: false,
		},
		{
			name: "machine claim without params is valid",
			claim: ResourceClaim{
				Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"},
				Mode:     schema.ModeInclusive,
				Status:   schema.ClaimPending,
			},
			wantErr: false,
		},
		{
			name: "machine claim with quota params is invalid",
			claim: ResourceClaim{
				Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"},
				Mode:     schema.ModeExclusive,
				Status:   schema.ClaimPending,
				Quota:    &QuotaReservation{CostLimit: "50.00"},
			},
			wantErr: true,
		},
		{
			name: "quota claim with quota params",
			claim: ResourceClaim{
				Resource: schema.ResourceRef{Type: schema.ResourceQuota, Target: "claude-api"},
				Mode:     schema.ModeInclusive,
				Status:   schema.ClaimPending,
				Quota:    &QuotaReservation{TokenBudget: 100000},
			},
			wantErr: false,
		},
		{
			name: "quota claim with machine params is invalid",
			claim: ResourceClaim{
				Resource: schema.ResourceRef{Type: schema.ResourceQuota, Target: "claude-api"},
				Mode:     schema.ModeInclusive,
				Status:   schema.ClaimPending,
				Machine:  &MachineReservation{},
			},
			wantErr: true,
		},
		{
			name: "human claim with machine params is invalid",
			claim: ResourceClaim{
				Resource: schema.ResourceRef{Type: schema.ResourceHuman, Target: "@engineer:bureau.local"},
				Mode:     schema.ModeExclusive,
				Status:   schema.ClaimPending,
				Machine:  &MachineReservation{},
			},
			wantErr: true,
		},
		{
			name: "service claim with quota params is invalid",
			claim: ResourceClaim{
				Resource: schema.ResourceRef{Type: schema.ResourceService, Target: "stt/whisper"},
				Mode:     schema.ModeExclusive,
				Status:   schema.ClaimPending,
				Quota:    &QuotaReservation{CostLimit: "10.00"},
			},
			wantErr: true,
		},
		{
			name: "claim with valid status_at",
			claim: ResourceClaim{
				Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"},
				Mode:     schema.ModeExclusive,
				Status:   schema.ClaimApproved,
				StatusAt: "2026-03-04T03:00:00Z",
			},
			wantErr: false,
		},
		{
			name: "claim with invalid status_at",
			claim: ResourceClaim{
				Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"},
				Mode:     schema.ModeExclusive,
				Status:   schema.ClaimApproved,
				StatusAt: "last tuesday",
			},
			wantErr: true,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.claim.Validate()
			if (err != nil) != testCase.wantErr {
				t.Errorf("Validate() error = %v, wantErr = %v", err, testCase.wantErr)
			}
		})
	}
}

func TestMachineReservationValidate(t *testing.T) {
	tests := []struct {
		name    string
		machine MachineReservation
		mode    schema.ReservationMode
		wantErr bool
	}{
		{
			name:    "exclusive with drain services",
			machine: MachineReservation{DrainServices: []string{"buildbarn-worker"}},
			mode:    schema.ModeExclusive,
			wantErr: false,
		},
		{
			name:    "exclusive without drain services (drain all)",
			machine: MachineReservation{},
			mode:    schema.ModeExclusive,
			wantErr: false,
		},
		{
			name:    "inclusive empty",
			machine: MachineReservation{},
			mode:    schema.ModeInclusive,
			wantErr: false,
		},
		{
			name: "partial with resources",
			machine: MachineReservation{
				Resources: &PartialMachineResources{CPUCores: 4},
			},
			mode:    schema.ModePartial,
			wantErr: false,
		},
		{
			name:    "partial without resources",
			machine: MachineReservation{},
			mode:    schema.ModePartial,
			wantErr: true,
		},
		{
			name: "non-partial with resources",
			machine: MachineReservation{
				Resources: &PartialMachineResources{CPUCores: 4},
			},
			mode:    schema.ModeExclusive,
			wantErr: true,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.machine.Validate(testCase.mode)
			if (err != nil) != testCase.wantErr {
				t.Errorf("Validate() error = %v, wantErr = %v", err, testCase.wantErr)
			}
		})
	}
}

func TestPartialMachineResourcesValidate(t *testing.T) {
	tests := []struct {
		name      string
		resources PartialMachineResources
		wantErr   bool
	}{
		{
			name:      "cpu cores only",
			resources: PartialMachineResources{CPUCores: 4},
			wantErr:   false,
		},
		{
			name:      "numa nodes only",
			resources: PartialMachineResources{NUMANodes: []int{0, 1}},
			wantErr:   false,
		},
		{
			name:      "gpu fraction only",
			resources: PartialMachineResources{GPUFraction: 0.5},
			wantErr:   false,
		},
		{
			name:      "all set",
			resources: PartialMachineResources{CPUCores: 8, NUMANodes: []int{0}, GPUFraction: 0.75},
			wantErr:   false,
		},
		{
			name:      "nothing set",
			resources: PartialMachineResources{},
			wantErr:   true,
		},
		{
			name:      "negative cpu cores",
			resources: PartialMachineResources{CPUCores: -1},
			wantErr:   true,
		},
		{
			name:      "negative numa node",
			resources: PartialMachineResources{NUMANodes: []int{-1}},
			wantErr:   true,
		},
		{
			name:      "gpu fraction too low",
			resources: PartialMachineResources{GPUFraction: -0.1},
			wantErr:   true,
		},
		{
			name:      "gpu fraction too high",
			resources: PartialMachineResources{GPUFraction: 1.1},
			wantErr:   true,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.resources.Validate()
			if (err != nil) != testCase.wantErr {
				t.Errorf("Validate() error = %v, wantErr = %v", err, testCase.wantErr)
			}
		})
	}
}

func TestQuotaReservationValidate(t *testing.T) {
	tests := []struct {
		name    string
		quota   QuotaReservation
		wantErr bool
	}{
		{
			name:    "token budget only",
			quota:   QuotaReservation{TokenBudget: 100000},
			wantErr: false,
		},
		{
			name:    "cost limit only",
			quota:   QuotaReservation{CostLimit: "50.00"},
			wantErr: false,
		},
		{
			name:    "both set",
			quota:   QuotaReservation{TokenBudget: 100000, CostLimit: "50.00"},
			wantErr: false,
		},
		{
			name:    "neither set",
			quota:   QuotaReservation{},
			wantErr: true,
		},
		{
			name:    "negative token budget",
			quota:   QuotaReservation{TokenBudget: -1},
			wantErr: true,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.quota.Validate()
			if (err != nil) != testCase.wantErr {
				t.Errorf("Validate() error = %v, wantErr = %v", err, testCase.wantErr)
			}
		})
	}
}

func TestTicketContentWithReservation(t *testing.T) {
	t.Run("resource_request ticket requires reservation", func(t *testing.T) {
		tc := validTicketContent()
		tc.Type = TypeResourceRequest
		tc.Reservation = nil
		if err := tc.Validate(); err == nil {
			t.Error("expected error for resource_request without reservation")
		}
	})

	t.Run("resource_request ticket with valid reservation", func(t *testing.T) {
		tc := validTicketContent()
		tc.Type = TypeResourceRequest
		reservation := validReservationContent()
		tc.Reservation = &reservation
		if err := tc.Validate(); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("non-resource_request ticket with reservation is valid", func(t *testing.T) {
		tc := validTicketContent()
		tc.Type = TypeTask
		reservation := validReservationContent()
		tc.Reservation = &reservation
		if err := tc.Validate(); err != nil {
			t.Errorf("unexpected error for task with reservation: %v", err)
		}
	})

	t.Run("pipeline ticket with reservation is valid", func(t *testing.T) {
		tc := validTicketContent()
		tc.Type = TypePipeline
		tc.Pipeline = &PipelineExecutionContent{
			PipelineRef: "gpu-training",
			TotalSteps:  3,
		}
		reservation := validReservationContent()
		tc.Reservation = &reservation
		if err := tc.Validate(); err != nil {
			t.Errorf("unexpected error for pipeline with reservation: %v", err)
		}
	})

	t.Run("resource_request ticket with invalid reservation", func(t *testing.T) {
		tc := validTicketContent()
		tc.Type = TypeResourceRequest
		tc.Reservation = &ReservationContent{MaxDuration: "2h"} // empty claims
		if err := tc.Validate(); err == nil {
			t.Error("expected error for reservation with empty claims")
		}
	})
}

func TestTicketContentWithReservationRoundTrip(t *testing.T) {
	reservation := validReservationContent()
	reservation.Claims = append(reservation.Claims, ResourceClaim{
		Resource:     schema.ResourceRef{Type: schema.ResourceQuota, Target: "claude-api"},
		Mode:         schema.ModeInclusive,
		Status:       schema.ClaimQueued,
		StatusAt:     "2026-03-04T02:55:00Z",
		StatusReason: "Position 2 of 3",
		Quota:        &QuotaReservation{CostLimit: "50.00"},
	})

	original := TicketContent{
		Version:     TicketContentVersion,
		Title:       "Nightly training run",
		Status:      StatusOpen,
		Priority:    2,
		Type:        TypeResourceRequest,
		Reservation: &reservation,
		CreatedBy:   ref.MustParseUserID("@iree/amdgpu/pm:bureau.local"),
		CreatedAt:   "2026-03-04T02:50:00Z",
		UpdatedAt:   "2026-03-04T02:55:00Z",
	}

	if err := original.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// Verify JSON structure.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal raw: %v", err)
	}
	if raw["type"] != "resource_request" {
		t.Errorf("type = %v, want resource_request", raw["type"])
	}
	if _, ok := raw["reservation"]; !ok {
		t.Error("reservation field missing from JSON")
	}

	// Full round-trip.
	var decoded TicketContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.Reservation == nil {
		t.Fatal("Reservation is nil after round-trip")
	}
	if len(decoded.Reservation.Claims) != 2 {
		t.Fatalf("Claims length = %d, want 2", len(decoded.Reservation.Claims))
	}

	// First claim: machine.
	claim0 := decoded.Reservation.Claims[0]
	if claim0.Resource.Type != schema.ResourceMachine {
		t.Errorf("Claims[0].Resource.Type = %v, want %v", claim0.Resource.Type, schema.ResourceMachine)
	}
	if claim0.Mode != schema.ModeExclusive {
		t.Errorf("Claims[0].Mode = %v, want %v", claim0.Mode, schema.ModeExclusive)
	}
	if claim0.Machine == nil {
		t.Fatal("Claims[0].Machine is nil")
	}

	// Second claim: quota.
	claim1 := decoded.Reservation.Claims[1]
	if claim1.Resource.Type != schema.ResourceQuota {
		t.Errorf("Claims[1].Resource.Type = %v, want %v", claim1.Resource.Type, schema.ResourceQuota)
	}
	if claim1.Status != schema.ClaimQueued {
		t.Errorf("Claims[1].Status = %v, want %v", claim1.Status, schema.ClaimQueued)
	}
	if claim1.StatusReason != "Position 2 of 3" {
		t.Errorf("Claims[1].StatusReason = %v, want %v", claim1.StatusReason, "Position 2 of 3")
	}
	if claim1.Quota == nil {
		t.Fatal("Claims[1].Quota is nil")
	}
	if claim1.Quota.CostLimit != "50.00" {
		t.Errorf("Claims[1].Quota.CostLimit = %v, want %v", claim1.Quota.CostLimit, "50.00")
	}

	if decoded.Reservation.MaxDuration != "2h" {
		t.Errorf("MaxDuration = %v, want %v", decoded.Reservation.MaxDuration, "2h")
	}
}

func TestPrefixForTypeResourceRequest(t *testing.T) {
	prefix := PrefixForType(TypeResourceRequest)
	if prefix != "rsv" {
		t.Errorf("PrefixForType(resource_request) = %q, want %q", prefix, "rsv")
	}
}

func TestReservationOmitempty(t *testing.T) {
	// Non-resource_request tickets should not include "reservation" in JSON.
	tc := validTicketContent()
	data, err := json.Marshal(tc)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if _, exists := raw["reservation"]; exists {
		t.Error("reservation field should be omitted when nil")
	}
}
