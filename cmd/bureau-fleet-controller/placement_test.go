// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/fleet"
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
			Principal:     "@bureau/fleet/prod/machine/workstation:bureau.local",
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
			Principal:    "@bureau/fleet/prod/machine/workstation:bureau.local",
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
		assignments: make(map[ref.UserID]*schema.PrincipalAssignment),
	}
}

// standardService creates a FleetServiceContent with typical GPU service
// values. Useful as a baseline that tests can modify.
func standardService() *fleet.FleetServiceContent {
	return &fleet.FleetServiceContent{
		Template: "bureau/template:whisper-stt",
		Replicas: fleet.ReplicaSpec{Min: 1},
		Resources: fleet.ResourceRequirements{
			MemoryMB:      4096,
			CPUMillicores: 2000,
			GPU:           true,
			GPUMemoryMB:   8192,
		},
		Placement: fleet.PlacementConstraints{
			Requires: []string{"gpu"},
		},
		Failover: "migrate",
		Priority: 10,
	}
}

func TestScoreMachineIneligibleNoInfo(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	fc.machines[testMachineUserID("noinfo")] = &machineState{
		status:      &schema.MachineStatus{CPUPercent: 10},
		assignments: make(map[ref.UserID]*schema.PrincipalAssignment),
	}

	score := fc.scoreMachine(testMachineUserID("noinfo"), standardService())
	if score != ineligible {
		t.Errorf("machine with nil info: got score %d, want %d", score, ineligible)
	}
}

func TestScoreMachineIneligibleNoStatus(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	fc.machines[testMachineUserID("nostatus")] = &machineState{
		info: &schema.MachineInfo{
			MemoryTotalMB: 65536,
			Labels:        map[string]string{"gpu": "rtx4090"},
		},
		assignments: make(map[ref.UserID]*schema.PrincipalAssignment),
	}

	score := fc.scoreMachine(testMachineUserID("nostatus"), standardService())
	if score != ineligible {
		t.Errorf("machine with nil status: got score %d, want %d", score, ineligible)
	}
}

func TestScoreMachineIneligibleCordoned(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	machine := standardMachine()
	machine.info.Labels["cordoned"] = "maintenance"
	fc.machines[testMachineUserID("cordoned")] = machine

	score := fc.scoreMachine(testMachineUserID("cordoned"), standardService())
	if score != ineligible {
		t.Errorf("cordoned machine: got score %d, want %d", score, ineligible)
	}
}

func TestScoreMachineIneligibleMissingLabel(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	machine := standardMachine()
	machine.info.Labels = map[string]string{} // no labels
	fc.machines[testMachineUserID("nolabel")] = machine

	service := standardService()
	service.Placement.Requires = []string{"gpu"}

	score := fc.scoreMachine(testMachineUserID("nolabel"), service)
	if score != ineligible {
		t.Errorf("machine missing required label: got score %d, want %d", score, ineligible)
	}
}

func TestScoreMachineIneligibleLabelValueMismatch(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	machine := standardMachine()
	machine.info.Labels = map[string]string{"gpu": "rtx4090"}
	fc.machines[testMachineUserID("wronggpu")] = machine

	service := standardService()
	service.Placement.Requires = []string{"gpu=h100"}

	score := fc.scoreMachine(testMachineUserID("wronggpu"), service)
	if score != ineligible {
		t.Errorf("label value mismatch: got score %d, want %d", score, ineligible)
	}
}

func TestScoreMachineLabelPresenceCheck(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	machine := standardMachine()
	machine.info.Labels = map[string]string{"gpu": "rtx4090", "persistent": "true"}
	fc.machines[testMachineUserID("haslabels")] = machine

	// "gpu" without =value should match any machine with the "gpu" key.
	service := standardService()
	service.Placement.Requires = []string{"gpu"}

	score := fc.scoreMachine(testMachineUserID("haslabels"), service)
	if score == ineligible {
		t.Error("label presence check failed: machine has 'gpu' label but was scored ineligible")
	}
}

func TestScoreMachineIneligibleNotInAllowedMachines(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	machine := standardMachine()
	fc.machines[testMachineUserID("workstation")] = machine

	service := standardService()
	service.Placement.AllowedMachines = []string{"bureau/fleet/prod/machine/cloud-*"}

	score := fc.scoreMachine(testMachineUserID("workstation"), service)
	if score != ineligible {
		t.Errorf("not in allowed machines: got score %d, want %d", score, ineligible)
	}
}

func TestScoreMachineAllowedMachinesGlobMatch(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	machine := standardMachine()
	fc.machines[testMachineUserID("cloud-gpu-1")] = machine
	machine.info.Principal = "@bureau/fleet/prod/machine/cloud-gpu-1:bureau.local"

	service := standardService()
	service.Placement.AllowedMachines = []string{"bureau/fleet/prod/machine/cloud-*"}

	score := fc.scoreMachine(testMachineUserID("cloud-gpu-1"), service)
	if score == ineligible {
		t.Error("allowed machines glob should match 'bureau/fleet/prod/machine/cloud-gpu-1' against 'bureau/fleet/prod/machine/cloud-*'")
	}
}

func TestScoreMachineIneligibleInsufficientMemory(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	machine := standardMachine()
	machine.info.MemoryTotalMB = 8192
	machine.status.MemoryUsedMB = 7000 // only 1192 MB free
	fc.machines[testMachineUserID("lowmem")] = machine

	service := standardService()
	service.Resources.MemoryMB = 4096 // needs 4 GB

	score := fc.scoreMachine(testMachineUserID("lowmem"), service)
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
	fc.machines[testMachineUserID("nogpu")] = machine

	service := standardService()
	service.Placement.Requires = nil // remove label requirement so we test GPU check specifically

	score := fc.scoreMachine(testMachineUserID("nogpu"), service)
	if score != ineligible {
		t.Errorf("no GPU: got score %d, want %d", score, ineligible)
	}
}

func TestScoreMachineIneligibleAntiAffinity(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	machine := standardMachine()
	machine.assignments[testServiceUserID("service/llm/large")] = &schema.PrincipalAssignment{
		Principal: testEntity(t, "service/llm/large"),
		Labels:    map[string]string{"fleet_managed": "service/fleet/prod"},
	}
	fc.machines[testMachineUserID("hasllm")] = machine

	service := standardService()
	service.Placement.AntiAffinity = []string{testServiceUserID("service/llm/large").String()}

	score := fc.scoreMachine(testMachineUserID("hasllm"), service)
	if score != ineligible {
		t.Errorf("anti-affinity violation: got score %d, want %d", score, ineligible)
	}
}

func TestScoreMachineEligibleBasic(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	machine := standardMachine()
	fc.machines[testMachineUserID("workstation")] = machine

	score := fc.scoreMachine(testMachineUserID("workstation"), standardService())
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

	score := fc.scoreMachine(testMachineUserID("nonexistent"), standardService())
	if score != ineligible {
		t.Errorf("nonexistent machine: got score %d, want %d", score, ineligible)
	}
}

func TestScoreMachinePreferredMachineBonus(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))

	// Create three identical machines.
	for _, name := range []string{"alpha", "beta", "gamma"} {
		machine := standardMachine()
		fc.machines[testMachineUserID(name)] = machine
	}

	service := standardService()
	service.Placement.PreferredMachines = []string{"bureau/fleet/prod/machine/alpha", "bureau/fleet/prod/machine/beta", "bureau/fleet/prod/machine/gamma"}

	scoreAlpha := fc.scoreMachine(testMachineUserID("alpha"), service)
	scoreBeta := fc.scoreMachine(testMachineUserID("beta"), service)
	scoreGamma := fc.scoreMachine(testMachineUserID("gamma"), service)

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
	machineWith.assignments[testServiceUserID("service/rag/local")] = &schema.PrincipalAssignment{
		Principal: testEntity(t, "service/rag/local"),
		Labels:    map[string]string{"fleet_managed": "service/fleet/prod"},
	}
	fc.machines[testMachineUserID("colocated")] = machineWith

	// Machine without co-located service.
	machineWithout := standardMachine()
	fc.machines[testMachineUserID("separate")] = machineWithout

	service := standardService()
	service.Placement.CoLocateWith = []string{testServiceUserID("service/rag/local").String()}

	scoreWith := fc.scoreMachine(testMachineUserID("colocated"), service)
	scoreWithout := fc.scoreMachine(testMachineUserID("separate"), service)

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
	fc.machines[testMachineUserID("idle")] = machineIdle

	// High utilization machine.
	machineBusy := standardMachine()
	machineBusy.status.CPUPercent = 85
	machineBusy.status.MemoryUsedMB = 58000
	fc.machines[testMachineUserID("busy")] = machineBusy

	score := fc.scoreMachine(testMachineUserID("idle"), standardService())
	scoreBusy := fc.scoreMachine(testMachineUserID("busy"), standardService())

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
	fc.machines[testMachineUserID("alpha")] = machineA

	machineB := standardMachine()
	machineB.status.CPUPercent = 80
	machineB.status.MemoryUsedMB = 55000
	fc.machines[testMachineUserID("beta")] = machineB

	machineC := standardMachine()
	machineC.status.CPUPercent = 50
	machineC.status.MemoryUsedMB = 32000
	fc.machines[testMachineUserID("gamma")] = machineC

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
	if candidates[0].machineUserID != testMachineUserID("alpha") {
		t.Errorf("expected bureau/fleet/prod/machine/alpha first (lowest CPU), got %s", candidates[0].machineUserID)
	}
}

func TestScorePlacementFiltersIneligible(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))

	// One eligible machine.
	eligible := standardMachine()
	fc.machines[testMachineUserID("eligible")] = eligible

	// One ineligible machine (cordoned).
	cordoned := standardMachine()
	cordoned.info.Labels["cordoned"] = "maintenance"
	fc.machines[testMachineUserID("cordoned")] = cordoned

	// One ineligible machine (no info).
	fc.machines[testMachineUserID("noinfo")] = &machineState{
		status:      &schema.MachineStatus{CPUPercent: 10},
		assignments: make(map[ref.UserID]*schema.PrincipalAssignment),
	}

	candidates := fc.scorePlacement(standardService())
	if len(candidates) != 1 {
		t.Fatalf("expected 1 eligible candidate, got %d", len(candidates))
	}
	if candidates[0].machineUserID != testMachineUserID("eligible") {
		t.Errorf("expected bureau/fleet/prod/machine/eligible, got %s", candidates[0].machineUserID)
	}
}

func TestScorePlacementTieBreaking(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))

	// Create identical machines — scores will be equal.
	for _, name := range []string{"charlie", "alpha", "bravo"} {
		machine := standardMachine()
		fc.machines[testMachineUserID(name)] = machine
	}

	candidates := fc.scorePlacement(standardService())
	if len(candidates) != 3 {
		t.Fatalf("expected 3 candidates, got %d", len(candidates))
	}

	// Identical scores should be broken by localpart ascending.
	if candidates[0].machineUserID != testMachineUserID("alpha") {
		t.Errorf("expected bureau/fleet/prod/machine/alpha first in tie-break, got %s", candidates[0].machineUserID)
	}
	if candidates[1].machineUserID != testMachineUserID("bravo") {
		t.Errorf("expected bureau/fleet/prod/machine/bravo second in tie-break, got %s", candidates[1].machineUserID)
	}
	if candidates[2].machineUserID != testMachineUserID("charlie") {
		t.Errorf("expected bureau/fleet/prod/machine/charlie third in tie-break, got %s", candidates[2].machineUserID)
	}
}

func TestInTimeWindow(t *testing.T) {
	tests := []struct {
		name   string
		now    time.Time
		window fleet.TimeWindow
		want   bool
	}{
		{
			name:   "within normal range",
			now:    time.Date(2026, 1, 15, 14, 0, 0, 0, time.UTC), // 2 PM
			window: fleet.TimeWindow{StartHour: 9, EndHour: 17},
			want:   true,
		},
		{
			name:   "before normal range",
			now:    time.Date(2026, 1, 15, 7, 0, 0, 0, time.UTC), // 7 AM
			window: fleet.TimeWindow{StartHour: 9, EndHour: 17},
			want:   false,
		},
		{
			name:   "at start of range",
			now:    time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC), // 9 AM
			window: fleet.TimeWindow{StartHour: 9, EndHour: 17},
			want:   true,
		},
		{
			name:   "at end of range (exclusive)",
			now:    time.Date(2026, 1, 15, 17, 0, 0, 0, time.UTC), // 5 PM
			window: fleet.TimeWindow{StartHour: 9, EndHour: 17},
			want:   false,
		},
		{
			name:   "midnight wrapping - in late evening",
			now:    time.Date(2026, 1, 15, 23, 0, 0, 0, time.UTC), // 11 PM
			window: fleet.TimeWindow{StartHour: 22, EndHour: 6},
			want:   true,
		},
		{
			name:   "midnight wrapping - in early morning",
			now:    time.Date(2026, 1, 15, 3, 0, 0, 0, time.UTC), // 3 AM
			window: fleet.TimeWindow{StartHour: 22, EndHour: 6},
			want:   true,
		},
		{
			name:   "midnight wrapping - outside afternoon",
			now:    time.Date(2026, 1, 15, 15, 0, 0, 0, time.UTC), // 3 PM
			window: fleet.TimeWindow{StartHour: 22, EndHour: 6},
			want:   false,
		},
		{
			name:   "same start and end means all day",
			now:    time.Date(2026, 1, 15, 15, 0, 0, 0, time.UTC),
			window: fleet.TimeWindow{StartHour: 8, EndHour: 8},
			want:   true,
		},
		{
			name:   "day-of-week match",
			now:    time.Date(2026, 1, 14, 14, 0, 0, 0, time.UTC), // Wednesday
			window: fleet.TimeWindow{Days: []fleet.DayOfWeek{fleet.Wednesday}, StartHour: 9, EndHour: 17},
			want:   true,
		},
		{
			name:   "day-of-week mismatch",
			now:    time.Date(2026, 1, 14, 14, 0, 0, 0, time.UTC), // Wednesday
			window: fleet.TimeWindow{Days: []fleet.DayOfWeek{fleet.Monday, fleet.Friday}, StartHour: 9, EndHour: 17},
			want:   false,
		},
		{
			name:   "empty days means all days",
			now:    time.Date(2026, 1, 14, 14, 0, 0, 0, time.UTC), // Wednesday
			window: fleet.TimeWindow{StartHour: 9, EndHour: 17},
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
	service.Scheduling = &fleet.SchedulingSpec{
		Class:       "batch",
		Preemptible: true,
		PreferredWindows: []fleet.TimeWindow{
			{StartHour: 22, EndHour: 6},
		},
	}

	// Score during preferred window.
	fcInWindow := newPlacementTestController(t, inWindowTime)
	machineIn := standardMachine()
	fcInWindow.machines[testMachineUserID("worker")] = machineIn
	scoreInWindow := fcInWindow.scoreMachine(testMachineUserID("worker"), service)

	// Score outside preferred window.
	fcOutside := newPlacementTestController(t, outsideWindowTime)
	machineOut := standardMachine()
	fcOutside.machines[testMachineUserID("worker")] = machineOut
	scoreOutside := fcOutside.scoreMachine(testMachineUserID("worker"), service)

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
	fcMorning.machines[testMachineUserID("worker")] = machineMorning
	scoreMorning := fcMorning.scoreMachine(testMachineUserID("worker"), service)

	fcAfternoon := newPlacementTestController(t, afternoonTime)
	machineAfternoon := standardMachine()
	fcAfternoon.machines[testMachineUserID("worker")] = machineAfternoon
	scoreAfternoon := fcAfternoon.scoreMachine(testMachineUserID("worker"), service)

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
	fc.machines[testMachineUserID("lowvram")] = machine

	service := standardService()
	service.Resources.GPUMemoryMB = 8192 // needs 8 GB, only ~4 GB free

	score := fc.scoreMachine(testMachineUserID("lowvram"), service)
	if score != ineligible {
		t.Errorf("insufficient GPU memory: got score %d, want %d", score, ineligible)
	}
}

func TestScoreMachineGPUNoStatsNeutralScore(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	machine := standardMachine()
	// Has GPU hardware but no GPU stats (monitoring unavailable).
	machine.status.GPUStats = nil
	fc.machines[testMachineUserID("nostats")] = machine

	service := standardService()
	service.Resources.GPUMemoryMB = 0 // don't check VRAM so we can test the headroom score path

	score := fc.scoreMachine(testMachineUserID("nostats"), service)
	if score <= 0 {
		t.Errorf("machine with GPUs but no stats should still be eligible, got score %d", score)
	}
}

func TestScoreMachineNoGPUServiceNoGPURequirement(t *testing.T) {
	fc := newPlacementTestController(t, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	machine := standardMachine()
	machine.info.GPUs = nil
	machine.status.GPUStats = nil
	fc.machines[testMachineUserID("cpuonly")] = machine

	// Service that doesn't require GPU.
	service := &fleet.FleetServiceContent{
		Template: "bureau/template:worker",
		Replicas: fleet.ReplicaSpec{Min: 1},
		Resources: fleet.ResourceRequirements{
			MemoryMB:      2048,
			CPUMillicores: 1000,
			GPU:           false,
		},
		Placement: fleet.PlacementConstraints{},
		Failover:  "migrate",
		Priority:  50,
	}

	score := fc.scoreMachine(testMachineUserID("cpuonly"), service)
	if score <= 0 {
		t.Errorf("CPU-only machine with CPU-only service should be eligible, got score %d", score)
	}
}
