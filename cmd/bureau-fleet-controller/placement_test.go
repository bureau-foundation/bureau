// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// newPlacementTestController creates a FleetController with a fake clock
// for deterministic placement scoring tests.
func newPlacementTestController(t *testing.T, now time.Time) *FleetController {
	t.Helper()
	fc := newTestFleetController(t)
	fc.clock = clock.Fake(now)
	return fc
}

// standardMachine creates a machineState with typical values: 16 cores,
// 65536 MB memory, 42% CPU, 32000 MB memory used, one GPU. Useful as
// a baseline that tests can modify.
func standardMachine() *machineState {
	return &machineState{
		info: &schema.MachineInfo{
			Principal:     "@machine/workstation:bureau.local",
			Hostname:      "workstation",
			MemoryTotalMB: 65536,
			CPU: schema.CPUInfo{
				CoresPerSocket: 16,
				Sockets:        1,
			},
			GPUs: []schema.GPUInfo{
				{
					Vendor:         "NVIDIA",
					ModelName:      "RTX 4090",
					VRAMTotalBytes: 25769803776, // 24576 MB
					PCISlot:        "0000:01:00.0",
				},
			},
			Labels: map[string]string{
				"gpu": "rtx4090",
			},
		},
		status: &schema.MachineStatus{
			Principal:    "@machine/workstation:bureau.local",
			CPUPercent:   42,
			MemoryUsedMB: 32000,
			GPUStats: []schema.GPUStatus{
				{
					PCISlot:            "0000:01:00.0",
					UtilizationPercent: 30,
					VRAMUsedBytes:      5368709120, // 5120 MB
				},
			},
		},
		assignments: make(map[string]*schema.PrincipalAssignment),
	}
}

// standardService creates a FleetServiceContent with typical GPU service
// values. Useful as a baseline that tests can modify.
func standardService() *schema.FleetServiceContent {
	return &schema.FleetServiceContent{
		Template: "bureau/template:whisper-stt",
		Replicas: schema.ReplicaSpec{Min: 1},
		Resources: schema.ResourceRequirements{
			MemoryMB:      4096,
			CPUMillicores: 2000,
			GPU:           true,
			GPUMemoryMB:   8192,
		},
		Placement: schema.PlacementConstraints{
			Requires: []string{"gpu"},
		},
		Failover: "migrate",
		Priority: 10,
	}
}

func TestScoreMachineIneligibleNoInfo(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	fc.machines["machine/noinfo"] = &machineState{
		status:      &schema.MachineStatus{CPUPercent: 10},
		assignments: make(map[string]*schema.PrincipalAssignment),
	}

	score := fc.scoreMachine("machine/noinfo", standardService())
	if score != ineligible {
		t.Errorf("machine with nil info: got score %d, want %d", score, ineligible)
	}
}

func TestScoreMachineIneligibleNoStatus(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	fc.machines["machine/nostatus"] = &machineState{
		info: &schema.MachineInfo{
			MemoryTotalMB: 65536,
			Labels:        map[string]string{"gpu": "rtx4090"},
		},
		assignments: make(map[string]*schema.PrincipalAssignment),
	}

	score := fc.scoreMachine("machine/nostatus", standardService())
	if score != ineligible {
		t.Errorf("machine with nil status: got score %d, want %d", score, ineligible)
	}
}

func TestScoreMachineIneligibleCordoned(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	machine := standardMachine()
	machine.info.Labels["cordoned"] = "maintenance"
	fc.machines["machine/cordoned"] = machine

	score := fc.scoreMachine("machine/cordoned", standardService())
	if score != ineligible {
		t.Errorf("cordoned machine: got score %d, want %d", score, ineligible)
	}
}

func TestScoreMachineIneligibleMissingLabel(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	machine := standardMachine()
	machine.info.Labels = map[string]string{} // no labels
	fc.machines["machine/nolabel"] = machine

	service := standardService()
	service.Placement.Requires = []string{"gpu"}

	score := fc.scoreMachine("machine/nolabel", service)
	if score != ineligible {
		t.Errorf("machine missing required label: got score %d, want %d", score, ineligible)
	}
}

func TestScoreMachineIneligibleLabelValueMismatch(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	machine := standardMachine()
	machine.info.Labels = map[string]string{"gpu": "rtx4090"}
	fc.machines["machine/wronggpu"] = machine

	service := standardService()
	service.Placement.Requires = []string{"gpu=h100"}

	score := fc.scoreMachine("machine/wronggpu", service)
	if score != ineligible {
		t.Errorf("label value mismatch: got score %d, want %d", score, ineligible)
	}
}

func TestScoreMachineLabelPresenceCheck(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	machine := standardMachine()
	machine.info.Labels = map[string]string{"gpu": "rtx4090", "persistent": "true"}
	fc.machines["machine/haslabels"] = machine

	// "gpu" without =value should match any machine with the "gpu" key.
	service := standardService()
	service.Placement.Requires = []string{"gpu"}

	score := fc.scoreMachine("machine/haslabels", service)
	if score == ineligible {
		t.Error("label presence check failed: machine has 'gpu' label but was scored ineligible")
	}
}

func TestScoreMachineIneligibleNotInAllowedMachines(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	machine := standardMachine()
	fc.machines["machine/workstation"] = machine

	service := standardService()
	service.Placement.AllowedMachines = []string{"machine/cloud-*"}

	score := fc.scoreMachine("machine/workstation", service)
	if score != ineligible {
		t.Errorf("not in allowed machines: got score %d, want %d", score, ineligible)
	}
}

func TestScoreMachineAllowedMachinesGlobMatch(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	machine := standardMachine()
	fc.machines["machine/cloud-gpu-1"] = machine
	machine.info.Principal = "@machine/cloud-gpu-1:bureau.local"

	service := standardService()
	service.Placement.AllowedMachines = []string{"machine/cloud-*"}

	score := fc.scoreMachine("machine/cloud-gpu-1", service)
	if score == ineligible {
		t.Error("allowed machines glob should match 'machine/cloud-gpu-1' against 'machine/cloud-*'")
	}
}

func TestScoreMachineIneligibleInsufficientMemory(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	machine := standardMachine()
	machine.info.MemoryTotalMB = 8192
	machine.status.MemoryUsedMB = 7000 // only 1192 MB free
	fc.machines["machine/lowmem"] = machine

	service := standardService()
	service.Resources.MemoryMB = 4096 // needs 4 GB

	score := fc.scoreMachine("machine/lowmem", service)
	if score != ineligible {
		t.Errorf("insufficient memory: got score %d, want %d", score, ineligible)
	}
}

func TestScoreMachineIneligibleNoGPU(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	machine := standardMachine()
	machine.info.GPUs = nil
	machine.status.GPUStats = nil
	machine.info.Labels = map[string]string{} // no gpu label
	fc.machines["machine/nogpu"] = machine

	service := standardService()
	service.Placement.Requires = nil // remove label requirement so we test GPU check specifically

	score := fc.scoreMachine("machine/nogpu", service)
	if score != ineligible {
		t.Errorf("no GPU: got score %d, want %d", score, ineligible)
	}
}

func TestScoreMachineIneligibleAntiAffinity(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	machine := standardMachine()
	machine.assignments["service/llm/large"] = &schema.PrincipalAssignment{
		Localpart: "service/llm/large",
		Labels:    map[string]string{"fleet_managed": "service/fleet/prod"},
	}
	fc.machines["machine/hasllm"] = machine

	service := standardService()
	service.Placement.AntiAffinity = []string{"service/llm/large"}

	score := fc.scoreMachine("machine/hasllm", service)
	if score != ineligible {
		t.Errorf("anti-affinity violation: got score %d, want %d", score, ineligible)
	}
}

func TestScoreMachineEligibleBasic(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	machine := standardMachine()
	fc.machines["machine/workstation"] = machine

	score := fc.scoreMachine("machine/workstation", standardService())
	if score <= 0 {
		t.Errorf("eligible machine should have positive score, got %d", score)
	}
	// Score should be within the 0-100000 range.
	if score > 100000 {
		t.Errorf("score %d exceeds maximum 100000", score)
	}
}

func TestScoreMachineNotFound(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))

	score := fc.scoreMachine("machine/nonexistent", standardService())
	if score != ineligible {
		t.Errorf("nonexistent machine: got score %d, want %d", score, ineligible)
	}
}

func TestScoreMachinePreferredMachineBonus(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))

	// Create three identical machines.
	for _, name := range []string{"machine/alpha", "machine/beta", "machine/gamma"} {
		machine := standardMachine()
		fc.machines[name] = machine
	}

	service := standardService()
	service.Placement.PreferredMachines = []string{"machine/alpha", "machine/beta", "machine/gamma"}

	scoreAlpha := fc.scoreMachine("machine/alpha", service)
	scoreBeta := fc.scoreMachine("machine/beta", service)
	scoreGamma := fc.scoreMachine("machine/gamma", service)

	if scoreAlpha <= scoreBeta {
		t.Errorf("first preferred (%d) should score higher than second preferred (%d)", scoreAlpha, scoreBeta)
	}
	if scoreBeta <= scoreGamma {
		t.Errorf("second preferred (%d) should score higher than third preferred (%d)", scoreBeta, scoreGamma)
	}
}

func TestScoreMachineCoLocateBonus(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))

	// Machine with co-located service.
	machineWith := standardMachine()
	machineWith.assignments["service/rag/local"] = &schema.PrincipalAssignment{
		Localpart: "service/rag/local",
		Labels:    map[string]string{"fleet_managed": "service/fleet/prod"},
	}
	fc.machines["machine/colocated"] = machineWith

	// Machine without co-located service.
	machineWithout := standardMachine()
	fc.machines["machine/separate"] = machineWithout

	service := standardService()
	service.Placement.CoLocateWith = []string{"service/rag/local"}

	scoreWith := fc.scoreMachine("machine/colocated", service)
	scoreWithout := fc.scoreMachine("machine/separate", service)

	if scoreWith <= scoreWithout {
		t.Errorf("co-located machine (%d) should score higher than separate machine (%d)", scoreWith, scoreWithout)
	}
}

func TestScoreMachineLowUtilizationPreferred(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))

	// Low utilization machine.
	machineIdle := standardMachine()
	machineIdle.status.CPUPercent = 10
	machineIdle.status.MemoryUsedMB = 8000
	fc.machines["machine/idle"] = machineIdle

	// High utilization machine.
	machineBusy := standardMachine()
	machineBusy.status.CPUPercent = 85
	machineBusy.status.MemoryUsedMB = 58000
	fc.machines["machine/busy"] = machineBusy

	score := fc.scoreMachine("machine/idle", standardService())
	scoreBusy := fc.scoreMachine("machine/busy", standardService())

	if score <= scoreBusy {
		t.Errorf("idle machine (%d) should score higher than busy machine (%d)", score, scoreBusy)
	}
}

func TestScorePlacementSortOrder(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))

	// Create machines with different utilization levels.
	machineA := standardMachine()
	machineA.status.CPUPercent = 20
	machineA.status.MemoryUsedMB = 16000
	fc.machines["machine/alpha"] = machineA

	machineB := standardMachine()
	machineB.status.CPUPercent = 80
	machineB.status.MemoryUsedMB = 55000
	fc.machines["machine/beta"] = machineB

	machineC := standardMachine()
	machineC.status.CPUPercent = 50
	machineC.status.MemoryUsedMB = 32000
	fc.machines["machine/gamma"] = machineC

	candidates := fc.scorePlacement(standardService())

	if len(candidates) != 3 {
		t.Fatalf("expected 3 candidates, got %d", len(candidates))
	}

	// Should be sorted by score descending.
	for i := 1; i < len(candidates); i++ {
		if candidates[i].score > candidates[i-1].score {
			t.Errorf("candidates not sorted: index %d (score %d) > index %d (score %d)",
				i, candidates[i].score, i-1, candidates[i-1].score)
		}
	}

	// Lowest CPU machine should be first (highest score).
	if candidates[0].machineLocalpart != "machine/alpha" {
		t.Errorf("expected machine/alpha first (lowest CPU), got %s", candidates[0].machineLocalpart)
	}
}

func TestScorePlacementFiltersIneligible(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))

	// One eligible machine.
	eligible := standardMachine()
	fc.machines["machine/eligible"] = eligible

	// One ineligible machine (cordoned).
	cordoned := standardMachine()
	cordoned.info.Labels["cordoned"] = "maintenance"
	fc.machines["machine/cordoned"] = cordoned

	// One ineligible machine (no info).
	fc.machines["machine/noinfo"] = &machineState{
		status:      &schema.MachineStatus{CPUPercent: 10},
		assignments: make(map[string]*schema.PrincipalAssignment),
	}

	candidates := fc.scorePlacement(standardService())
	if len(candidates) != 1 {
		t.Fatalf("expected 1 eligible candidate, got %d", len(candidates))
	}
	if candidates[0].machineLocalpart != "machine/eligible" {
		t.Errorf("expected machine/eligible, got %s", candidates[0].machineLocalpart)
	}
}

func TestScorePlacementTieBreaking(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))

	// Create identical machines — scores will be equal.
	for _, name := range []string{"machine/charlie", "machine/alpha", "machine/bravo"} {
		machine := standardMachine()
		fc.machines[name] = machine
	}

	candidates := fc.scorePlacement(standardService())
	if len(candidates) != 3 {
		t.Fatalf("expected 3 candidates, got %d", len(candidates))
	}

	// Identical scores should be broken by localpart ascending.
	if candidates[0].machineLocalpart != "machine/alpha" {
		t.Errorf("expected machine/alpha first in tie-break, got %s", candidates[0].machineLocalpart)
	}
	if candidates[1].machineLocalpart != "machine/bravo" {
		t.Errorf("expected machine/bravo second in tie-break, got %s", candidates[1].machineLocalpart)
	}
	if candidates[2].machineLocalpart != "machine/charlie" {
		t.Errorf("expected machine/charlie third in tie-break, got %s", candidates[2].machineLocalpart)
	}
}

func TestInTimeWindow(t *testing.T) {
	tests := []struct {
		name   string
		now    time.Time
		window schema.TimeWindow
		want   bool
	}{
		{
			name:   "within normal range",
			now:    time.Date(2026, 1, 15, 14, 0, 0, 0, time.UTC), // 2 PM
			window: schema.TimeWindow{StartHour: 9, EndHour: 17},
			want:   true,
		},
		{
			name:   "before normal range",
			now:    time.Date(2026, 1, 15, 7, 0, 0, 0, time.UTC), // 7 AM
			window: schema.TimeWindow{StartHour: 9, EndHour: 17},
			want:   false,
		},
		{
			name:   "at start of range",
			now:    time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC), // 9 AM
			window: schema.TimeWindow{StartHour: 9, EndHour: 17},
			want:   true,
		},
		{
			name:   "at end of range (exclusive)",
			now:    time.Date(2026, 1, 15, 17, 0, 0, 0, time.UTC), // 5 PM
			window: schema.TimeWindow{StartHour: 9, EndHour: 17},
			want:   false,
		},
		{
			name:   "midnight wrapping - in late evening",
			now:    time.Date(2026, 1, 15, 23, 0, 0, 0, time.UTC), // 11 PM
			window: schema.TimeWindow{StartHour: 22, EndHour: 6},
			want:   true,
		},
		{
			name:   "midnight wrapping - in early morning",
			now:    time.Date(2026, 1, 15, 3, 0, 0, 0, time.UTC), // 3 AM
			window: schema.TimeWindow{StartHour: 22, EndHour: 6},
			want:   true,
		},
		{
			name:   "midnight wrapping - outside afternoon",
			now:    time.Date(2026, 1, 15, 15, 0, 0, 0, time.UTC), // 3 PM
			window: schema.TimeWindow{StartHour: 22, EndHour: 6},
			want:   false,
		},
		{
			name:   "same start and end means all day",
			now:    time.Date(2026, 1, 15, 15, 0, 0, 0, time.UTC),
			window: schema.TimeWindow{StartHour: 8, EndHour: 8},
			want:   true,
		},
		{
			name:   "day-of-week match",
			now:    time.Date(2026, 1, 14, 14, 0, 0, 0, time.UTC), // Wednesday
			window: schema.TimeWindow{Days: []string{"wed"}, StartHour: 9, EndHour: 17},
			want:   true,
		},
		{
			name:   "day-of-week mismatch",
			now:    time.Date(2026, 1, 14, 14, 0, 0, 0, time.UTC), // Wednesday
			window: schema.TimeWindow{Days: []string{"mon", "fri"}, StartHour: 9, EndHour: 17},
			want:   false,
		},
		{
			name:   "empty days means all days",
			now:    time.Date(2026, 1, 14, 14, 0, 0, 0, time.UTC), // Wednesday
			window: schema.TimeWindow{StartHour: 9, EndHour: 17},
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := inTimeWindow(tt.now, tt.window)
			if got != tt.want {
				t.Errorf("inTimeWindow(%v, %+v) = %v, want %v",
					tt.now.Format("Mon 15:04"), tt.window, got, tt.want)
			}
		})
	}
}

func TestScoreMachineBatchTimeOfDay(t *testing.T) {
	// Wednesday 3 AM — inside the 22-6 preferred window.
	inWindowTime := time.Date(2026, 1, 15, 3, 0, 0, 0, time.UTC)
	// Wednesday 2 PM — outside the 22-6 preferred window.
	outsideWindowTime := time.Date(2026, 1, 15, 14, 0, 0, 0, time.UTC)

	service := standardService()
	service.Scheduling = &schema.SchedulingSpec{
		Class:       "batch",
		Preemptible: true,
		PreferredWindows: []schema.TimeWindow{
			{StartHour: 22, EndHour: 6},
		},
	}

	// Score during preferred window.
	fcInWindow := newPlacementTestController(t, inWindowTime)
	machineIn := standardMachine()
	fcInWindow.machines["machine/worker"] = machineIn
	scoreInWindow := fcInWindow.scoreMachine("machine/worker", service)

	// Score outside preferred window.
	fcOutside := newPlacementTestController(t, outsideWindowTime)
	machineOut := standardMachine()
	fcOutside.machines["machine/worker"] = machineOut
	scoreOutside := fcOutside.scoreMachine("machine/worker", service)

	if scoreInWindow <= scoreOutside {
		t.Errorf("score during preferred window (%d) should be higher than outside (%d)",
			scoreInWindow, scoreOutside)
	}

	// The in-window score should be roughly 3.3x the outside score
	// (1000/300 = 3.33). Allow some tolerance for rounding.
	ratio := float64(scoreInWindow) / float64(scoreOutside)
	if ratio < 2.5 || ratio > 4.0 {
		t.Errorf("window/non-window score ratio = %.2f, expected ~3.33 (scores: %d vs %d)",
			ratio, scoreInWindow, scoreOutside)
	}
}

func TestScoreMachineNonBatchIgnoresTimeOfDay(t *testing.T) {
	morningTime := time.Date(2026, 1, 15, 3, 0, 0, 0, time.UTC)
	afternoonTime := time.Date(2026, 1, 15, 14, 0, 0, 0, time.UTC)

	service := standardService()
	// No Scheduling — always-on service.

	fcMorning := newPlacementTestController(t, morningTime)
	machineMorning := standardMachine()
	fcMorning.machines["machine/worker"] = machineMorning
	scoreMorning := fcMorning.scoreMachine("machine/worker", service)

	fcAfternoon := newPlacementTestController(t, afternoonTime)
	machineAfternoon := standardMachine()
	fcAfternoon.machines["machine/worker"] = machineAfternoon
	scoreAfternoon := fcAfternoon.scoreMachine("machine/worker", service)

	// Non-batch services should score identically regardless of time.
	if scoreMorning != scoreAfternoon {
		t.Errorf("non-batch service scored differently at different times: %d vs %d",
			scoreMorning, scoreAfternoon)
	}
}

func TestScoreMachineGPUMemoryInsufficient(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	machine := standardMachine()
	// GPU has 24576 MB total, 20480 MB used (only ~4096 MB free).
	machine.status.GPUStats[0].VRAMUsedBytes = 21474836480 // 20480 MB
	fc.machines["machine/lowvram"] = machine

	service := standardService()
	service.Resources.GPUMemoryMB = 8192 // needs 8 GB, only ~4 GB free

	score := fc.scoreMachine("machine/lowvram", service)
	if score != ineligible {
		t.Errorf("insufficient GPU memory: got score %d, want %d", score, ineligible)
	}
}

func TestScoreMachineGPUNoStatsNeutralScore(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	machine := standardMachine()
	// Has GPU hardware but no GPU stats (monitoring unavailable).
	machine.status.GPUStats = nil
	fc.machines["machine/nostats"] = machine

	service := standardService()
	service.Resources.GPUMemoryMB = 0 // don't check VRAM so we can test the headroom score path

	score := fc.scoreMachine("machine/nostats", service)
	if score <= 0 {
		t.Errorf("machine with GPUs but no stats should still be eligible, got score %d", score)
	}
}

func TestScoreMachineNoGPUServiceNoGPURequirement(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	machine := standardMachine()
	machine.info.GPUs = nil
	machine.status.GPUStats = nil
	fc.machines["machine/cpuonly"] = machine

	// Service that doesn't require GPU.
	service := &schema.FleetServiceContent{
		Template: "bureau/template:worker",
		Replicas: schema.ReplicaSpec{Min: 1},
		Resources: schema.ResourceRequirements{
			MemoryMB:      2048,
			CPUMillicores: 1000,
			GPU:           false,
		},
		Placement: schema.PlacementConstraints{},
		Failover:  "migrate",
		Priority:  50,
	}

	score := fc.scoreMachine("machine/cpuonly", service)
	if score <= 0 {
		t.Errorf("CPU-only machine with CPU-only service should be eligible, got score %d", score)
	}
}
